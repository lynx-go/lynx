package pubsub

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/lynx-go/lynx"
	"github.com/spf13/viper"
)

// fakeApp is a minimal lynx.App implementation for tests.
type fakeApp struct {
	ctx    context.Context
	logger *slog.Logger
}

func newFakeApp() *fakeApp {
	return &fakeApp{
		ctx:    context.Background(),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func (f *fakeApp) Close()                                {}
func (f *fakeApp) Config() *viper.Viper                  { return nil }
func (f *fakeApp) Context() context.Context              { return f.ctx }
func (f *fakeApp) CLI(lynx.CommandFunc) error            { return nil }
func (f *fakeApp) Hooks(...lynx.HookOption) error        { return nil }
func (f *fakeApp) HealthCheckFunc() lynx.HealthCheckFunc { return nil }
func (f *fakeApp) Run() error                            { return nil }
func (f *fakeApp) SetLogger(logger *slog.Logger)         { f.logger = logger }
func (f *fakeApp) Logger(kwargs ...any) *slog.Logger     { return f.logger.With(kwargs...) }

var _ lynx.App = (*fakeApp)(nil)

func pollUntil(deadline time.Duration, interval time.Duration, cond func() bool) bool {
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if cond() {
			return true
		}
		time.Sleep(interval)
	}
	return cond()
}

func TestMessageIDFromContext(t *testing.T) {
	tests := []struct {
		name string
		ctx  context.Context
		want string
	}{
		{
			name: "missing key returns empty string without panic",
			ctx:  context.Background(),
			want: "",
		},
		{
			name: "round-trip via ContextWithMessageID",
			ctx:  ContextWithMessageID(context.Background(), "msg-123"),
			want: "msg-123",
		},
		{
			name: "wrong value type returns empty string",
			ctx:  context.WithValue(context.Background(), MessageIDKey, 42),
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Regression: previously an unchecked type assertion panicked on
			// a missing key.
			if got := MessageIDFromContext(tt.ctx); got != tt.want {
				t.Fatalf("MessageIDFromContext() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMessageKeyFromContext(t *testing.T) {
	if got := MessageKeyFromContext(context.Background()); got != "" {
		t.Fatalf("expected empty string for missing key, got %q", got)
	}
	ctx := ContextWithMessageKey(context.Background(), "key-1")
	if got := MessageKeyFromContext(ctx); got != "key-1" {
		t.Fatalf("MessageKeyFromContext() = %q, want %q", got, "key-1")
	}
}

func TestMessageMetadataHelpers(t *testing.T) {
	msg := message.NewMessage("uuid-1", []byte("payload"))

	SetMessageKey(msg, "k1")
	if got := GetMessageKey(msg); got != "k1" {
		t.Fatalf("GetMessageKey() = %q, want %q", got, "k1")
	}

	SetMessageID(msg, "id-1")
	if got := GetMessageID(msg); got != "id-1" {
		t.Fatalf("GetMessageID() = %q, want %q", got, "id-1")
	}

	fresh := message.NewMessage("uuid-2", nil)
	if got := GetMessageKey(fresh); got != "" {
		t.Fatalf("expected empty message key, got %q", got)
	}
	if got := GetMessageID(fresh); got != "" {
		t.Fatalf("expected empty message id, got %q", got)
	}
}

func TestNewJSONMessage(t *testing.T) {
	msg := NewJSONMessage(map[string]string{"hello": "world"})
	if msg.UUID == "" {
		t.Fatalf("expected non-empty message UUID")
	}
	if got := string(msg.Payload); got != `{"hello":"world"}` {
		t.Fatalf("unexpected payload: %s", got)
	}
}

func TestBrokerBeforeInit(t *testing.T) {
	// Regression: IsRunning/CheckHealth with a nil router must not panic.
	b := NewBroker(Options{}, nil)

	if b.IsRunning() {
		t.Fatalf("expected IsRunning() = false before Init")
	}
	if err := b.CheckHealth(); err == nil {
		t.Fatalf("expected CheckHealth() error before Init")
	}
	if got := b.Binders(); len(got) != 0 {
		t.Fatalf("expected no binders, got %d", len(got))
	}
	if got := b.ID(); got != "" {
		t.Fatalf("expected empty broker ID before Init, got %q", got)
	}
	if got := b.Name(); got != "pubsub-watermill" {
		t.Fatalf("unexpected Name(): %q", got)
	}
}

// startBroker creates a broker backed by the in-memory gochannel pubsub,
// initializes it, subscribes handler to topic, and starts the router. It
// returns the broker once the router reports running.
func startBroker(t *testing.T, opts Options, topic, handlerName string, h HandlerFunc, subOpts ...SubscribeOption) Broker {
	t.Helper()

	b := NewBroker(opts, nil)
	app := newFakeApp()
	if err := b.Init(app); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if b.IsRunning() {
		t.Fatalf("router should not be running right after Init")
	}
	if err := b.CheckHealth(); err == nil {
		t.Fatalf("expected CheckHealth error after Init but before Start")
	}

	if err := b.Subscribe(topic, handlerName, h, subOpts...); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	startDone := make(chan error, 1)
	go func() {
		startDone <- b.Start(ctx)
	}()

	if !pollUntil(5*time.Second, 10*time.Millisecond, b.IsRunning) {
		cancel()
		t.Fatalf("broker router did not start within 5s")
	}
	if err := b.CheckHealth(); err != nil {
		cancel()
		t.Fatalf("expected healthy broker while running, got %v", err)
	}
	// Give gochannel a brief moment to finish wiring subscriptions.
	time.Sleep(200 * time.Millisecond)

	t.Cleanup(func() {
		cancel()
		b.Stop(context.Background())
		select {
		case <-startDone:
		case <-time.After(3 * time.Second):
			t.Errorf("router did not shut down within 3s")
		}
	})
	return b
}

func TestBrokerPublishSubscribe(t *testing.T) {
	type result struct {
		ctx context.Context
		msg *message.Message
	}
	received := make(chan result, 1)

	b := startBroker(t, Options{}, "test.event", "test-handler",
		func(ctx context.Context, msg *message.Message) error {
			received <- result{ctx: ctx, msg: msg}
			return nil
		})

	published := NewJSONMessage(map[string]string{"hello": "world"})
	err := b.Publish(context.Background(), "test.event", published,
		WithMessageKey("key-1"),
		WithMetadataField("foo", "bar"),
	)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}

	select {
	case r := <-received:
		if string(r.msg.Payload) != string(published.Payload) {
			t.Errorf("payload = %s, want %s", r.msg.Payload, published.Payload)
		}
		if got := r.msg.Metadata.Get("foo"); got != "bar" {
			t.Errorf("metadata foo = %q, want %q", got, "bar")
		}
		if got := GetMessageKey(r.msg); got != "key-1" {
			t.Errorf("message key = %q, want %q", got, "key-1")
		}
		// The subscriber wrapper must put the message UUID into the context.
		if got := MessageIDFromContext(r.ctx); got != published.UUID {
			t.Errorf("MessageIDFromContext() = %q, want %q", got, published.UUID)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("did not receive published message within 5s")
	}
}

func TestBrokerStop(t *testing.T) {
	b := NewBroker(Options{}, nil)
	if err := b.Init(newFakeApp()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := b.Subscribe("noop.event", "noop-handler", func(ctx context.Context, msg *message.Message) error {
		return nil
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	startDone := make(chan error, 1)
	go func() {
		startDone <- b.Start(ctx)
	}()
	if !pollUntil(5*time.Second, 10*time.Millisecond, b.IsRunning) {
		cancel()
		t.Fatalf("broker router did not start within 5s")
	}

	cancel()
	b.Stop(context.Background())

	select {
	case <-startDone:
	case <-time.After(3 * time.Second):
		t.Fatalf("Start did not return after Stop")
	}

	// NOTE: watermill's Router.IsRunning is documented as "not aware of router
	// closing", so IsRunning/CheckHealth still report running after Stop.
	// What must fail is publishing on the closed gochannel pubsub.
	if err := b.Publish(context.Background(), "noop.event", NewJSONMessage(nil)); err == nil {
		t.Fatalf("expected Publish error after Stop")
	}
}

func TestBrokerRetriesFailedHandler(t *testing.T) {
	var calls atomic.Int32
	done := make(chan struct{}, 1)

	// The broker wires middleware.Retry{MaxRetries: 3}; a failing handler
	// (without WithContinueOnError) must be invoked more than once.
	b := startBroker(t, Options{}, "retry.event", "retry-handler",
		func(ctx context.Context, msg *message.Message) error {
			if calls.Add(1) >= 2 {
				select {
				case done <- struct{}{}:
				default:
				}
			}
			return errors.New("handler failed")
		})

	if err := b.Publish(context.Background(), "retry.event", NewJSONMessage(nil)); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("handler was not retried (calls=%d)", calls.Load())
	}
	_ = b
}

func TestBrokerContinueOnErrorAcks(t *testing.T) {
	var calls atomic.Int32
	receivedSecond := make(chan struct{}, 1)

	b := startBroker(t, Options{}, "coe.event", "coe-handler",
		func(ctx context.Context, msg *message.Message) error {
			n := calls.Add(1)
			if n == 1 {
				return errors.New("first message fails")
			}
			select {
			case receivedSecond <- struct{}{}:
			default:
			}
			return nil
		}, WithContinueOnError())

	if err := b.Publish(context.Background(), "coe.event", NewJSONMessage(nil)); err != nil {
		t.Fatalf("Publish first: %v", err)
	}
	// Wait for the first (failing) message to be processed.
	if !pollUntil(5*time.Second, 10*time.Millisecond, func() bool { return calls.Load() >= 1 }) {
		t.Fatalf("first message was not processed")
	}

	// The router must still be alive and able to deliver the next message.
	if err := b.CheckHealth(); err != nil {
		t.Fatalf("broker unhealthy after failing handler: %v", err)
	}
	if err := b.Publish(context.Background(), "coe.event", NewJSONMessage(nil)); err != nil {
		t.Fatalf("Publish second: %v", err)
	}

	select {
	case <-receivedSecond:
	case <-time.After(5 * time.Second):
		t.Fatalf("second message was not delivered after ContinueOnError")
	}
	_ = b
}

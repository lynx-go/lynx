package kafka

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/contrib/pubsub"
	"github.com/segmentio/kafka-go"
	"github.com/spf13/viper"
	"gocloud.dev/server/health"
)

// errFakeReaderClosed is returned by fakeReader once it is closed and drained.
var errFakeReaderClosed = errors.New("fake reader closed")

// fetchResult is one queued result of fakeReader.FetchMessage.
type fetchResult struct {
	msg kafka.Message
	err error
}

// fakeReader implements the messageReader seam without a real Kafka broker.
type fakeReader struct {
	mu        sync.Mutex
	queue     []fetchResult
	committed []kafka.Message
	closeCh   chan struct{}
	closed    bool
	closeErr  error
}

func newFakeReader(results ...fetchResult) *fakeReader {
	return &fakeReader{
		queue:   results,
		closeCh: make(chan struct{}),
	}
}

func (r *fakeReader) FetchMessage(ctx context.Context) (kafka.Message, error) {
	r.mu.Lock()
	if len(r.queue) > 0 {
		res := r.queue[0]
		r.queue = r.queue[1:]
		r.mu.Unlock()
		return res.msg, res.err
	}
	closeCh := r.closeCh
	closed := r.closed
	r.mu.Unlock()

	if closed {
		return kafka.Message{}, errFakeReaderClosed
	}
	select {
	case <-closeCh:
		return kafka.Message{}, errFakeReaderClosed
	case <-ctx.Done():
		return kafka.Message{}, ctx.Err()
	}
}

func (r *fakeReader) CommitMessages(_ context.Context, msgs ...kafka.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.committed = append(r.committed, msgs...)
	return nil
}

func (r *fakeReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.closed {
		r.closed = true
		close(r.closeCh)
	}
	return r.closeErr
}

func (r *fakeReader) committedCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.committed)
}

// fakeWriter implements the messageWriter seam without a real Kafka broker.
type fakeWriter struct {
	mu         sync.Mutex
	messages   []kafka.Message
	writeErr   error
	closeCalls int
	closeErr   error
}

func (w *fakeWriter) WriteMessages(_ context.Context, msgs ...kafka.Message) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.writeErr != nil {
		return w.writeErr
	}
	w.messages = append(w.messages, msgs...)
	return nil
}

func (w *fakeWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closeCalls++
	return w.closeErr
}

func (w *fakeWriter) written() []kafka.Message {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]kafka.Message, len(w.messages))
	copy(out, w.messages)
	return out
}

// publishedMessage records one call to fakeBroker.Publish.
type publishedMessage struct {
	topicName string
	msg       *message.Message
}

// subscription records one call to fakeBroker.Subscribe.
type subscription struct {
	topicName   string
	handlerName string
	handler     pubsub.HandlerFunc
}

// fakeBroker implements pubsub.Broker, capturing published messages.
type fakeBroker struct {
	mu              sync.Mutex
	published       []publishedMessage
	publishAttempts int
	publishErr      error
	subscriptions   []subscription
	subscribeErr    error
}

func (b *fakeBroker) Publish(_ context.Context, topicName string, msg *message.Message, _ ...pubsub.PublishOption) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.publishAttempts++
	if b.publishErr != nil {
		return b.publishErr
	}
	b.published = append(b.published, publishedMessage{topicName: topicName, msg: msg})
	return nil
}

func (b *fakeBroker) Subscribe(topicName, handlerName string, h pubsub.HandlerFunc, _ ...pubsub.SubscribeOption) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.subscribeErr != nil {
		return b.subscribeErr
	}
	b.subscriptions = append(b.subscriptions, subscription{topicName: topicName, handlerName: handlerName, handler: h})
	return nil
}

func (b *fakeBroker) publishedMessages() []publishedMessage {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]publishedMessage, len(b.published))
	copy(out, b.published)
	return out
}

func (b *fakeBroker) publishCallCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.publishAttempts
}

func (b *fakeBroker) subscribeCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subscriptions)
}

// --- pubsub.Broker boilerplate (not exercised by these tests) ---

func (b *fakeBroker) CheckHealth() error          { return nil }
func (b *fakeBroker) Name() string                { return "fake-broker" }
func (b *fakeBroker) Init(lynx.Lynx) error        { return nil }
func (b *fakeBroker) Start(context.Context) error { return nil }
func (b *fakeBroker) Stop(context.Context)        {}
func (b *fakeBroker) ID() string                  { return "fake-broker" }
func (b *fakeBroker) IsRunning() bool             { return true }
func (b *fakeBroker) Binders() []pubsub.Binder    { return nil }

// fakeApp implements lynx.Lynx with no-ops; only Context() is meaningful.
type fakeApp struct {
	ctx context.Context
}

func newFakeApp() *fakeApp {
	return &fakeApp{ctx: context.Background()}
}

func (a *fakeApp) Close()                     {}
func (a *fakeApp) Config() *viper.Viper       { return viper.New() }
func (a *fakeApp) Context() context.Context   { return a.ctx }
func (a *fakeApp) CLI(lynx.CommandFunc) error { return nil }
func (a *fakeApp) Hooks(...lynx.HookOption) error {
	return nil
}
func (a *fakeApp) HealthCheckFunc() lynx.HealthCheckFunc {
	return func() []health.Checker { return nil }
}
func (a *fakeApp) Run() error                 { return nil }
func (a *fakeApp) SetLogger(*slog.Logger)     {}
func (a *fakeApp) Logger(...any) *slog.Logger { return slog.Default() }

// captureHandler is a slog.Handler that records matching log records.
type captureHandler struct {
	mu      sync.Mutex
	level   slog.Level
	match   string
	records []slog.Record
}

func newCaptureHandler(level slog.Level, match string) *captureHandler {
	return &captureHandler{level: level, match: match}
}

func (h *captureHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level
}

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Message == h.match {
		h.mu.Lock()
		h.records = append(h.records, r.Clone())
		h.mu.Unlock()
	}
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

func (h *captureHandler) firstAttr(key string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.records) == 0 {
		return "", false
	}
	var val string
	found := false
	h.records[0].Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			val = a.Value.String()
			found = true
			return false
		}
		return true
	})
	return val, found
}

// slogSwap installs a slog default logger backed by handler and returns a
// restore function. lynx-go/x/log falls back to slog.Default(), so this
// captures the package's log output in tests.
func slogSwap(handler slog.Handler) func() {
	prev := slog.Default()
	slog.SetDefault(slog.New(handler))
	return func() { slog.SetDefault(prev) }
}

// waitUntil polls cond until it returns true or the timeout elapses.
func waitUntil(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

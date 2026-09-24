package watermill_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/lynx-go/lynx/contrib/watermill"
	"github.com/lynx-go/lynx/eventbus"
)

// inflightTracker 统计 handler 并发：进入 +1、退出 -1，记录峰值。
type inflightTracker struct {
	mu  sync.Mutex
	cur int
	max int
}

func (t *inflightTracker) enter() {
	t.mu.Lock()
	t.cur++
	if t.cur > t.max {
		t.max = t.cur
	}
	t.mu.Unlock()
}

func (t *inflightTracker) leave() {
	t.mu.Lock()
	t.cur--
	t.mu.Unlock()
}

func (t *inflightTracker) current() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cur
}

func (t *inflightTracker) peak() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.max
}

// waitCond 轮询等待条件成立（并发上限的确定性断言用）。
func waitCond(t *testing.T, d time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(msg)
}

// TestSubscribeMaxInFlightLimitsInFlight 钉住订阅级在途上限：max_in_flight=2
// 时同一事件最多两条消息在处理（handler 阻塞在 release 上，第三条必须留在
// 适配器/transport 侧），释放后全部确认，峰值恰为 2。
func TestSubscribeMaxInFlightLimitsInFlight(t *testing.T) {
	rt := &recordingTransport{topic: "order.limit"}
	acked := make(chan struct{}, 8)
	rt.onAck = func() { acked <- struct{}{} }
	bus := watermill.New(eventbus.Options{
		Transports: []eventbus.Transport{rt},
		Topics:     map[string]eventbus.TopicConfig{"order.limit": {MaxInFlight: 2}},
	})
	if err := bus.Init(nil); err != nil {
		t.Fatalf("Init: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = bus.Start(ctx) }()
	waitBus(t, bus)

	release := make(chan struct{})
	var tr inflightTracker
	if err := bus.Subscribe(ctx, "order.limit", func(context.Context, *eventbus.RawEvent) error {
		tr.enter()
		<-release
		tr.leave()
		return nil
	}, eventbus.WithHandlerName("h-limit")); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	for i := 0; i < 5; i++ {
		if err := bus.Publish(ctx, "order.limit", map[string]string{"id": "x"}); err != nil {
			t.Fatalf("Publish %d: %v", i, err)
		}
	}
	waitCond(t, 3*time.Second, func() bool { return tr.current() == 2 },
		"want exactly 2 in-flight messages at the cap")
	// 上限生效：第三条不得越过（handler 阻塞期间留出观察窗口）。
	time.Sleep(100 * time.Millisecond)
	if got := tr.current(); got != 2 {
		t.Fatalf("in-flight = %d, want 2 (max_in_flight cap must hold)", got)
	}
	close(release)
	for i := 0; i < 5; i++ {
		select {
		case <-acked:
		case <-time.After(3 * time.Second):
			t.Fatalf("only %d/5 messages acked", i)
		}
	}
	if got := tr.peak(); got != 2 {
		t.Fatalf("peak in-flight = %d, want 2", got)
	}
	stopWithin(t, bus, 2*time.Second)
}

// TestSubscribeMaxInFlightDefaultIsOne 钉住默认值：未配置时在途上限为 1
// （串行处理），第二条消息必须等第一条确认后才进入 handler。
func TestSubscribeMaxInFlightDefaultIsOne(t *testing.T) {
	rt := &recordingTransport{topic: "order.serial"}
	acked := make(chan struct{}, 4)
	rt.onAck = func() { acked <- struct{}{} }
	bus := watermill.New(eventbus.Options{Transports: []eventbus.Transport{rt}})
	if err := bus.Init(nil); err != nil {
		t.Fatalf("Init: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = bus.Start(ctx) }()
	waitBus(t, bus)

	release := make(chan struct{})
	var tr inflightTracker
	if err := bus.Subscribe(ctx, "order.serial", func(context.Context, *eventbus.RawEvent) error {
		tr.enter()
		<-release
		tr.leave()
		return nil
	}, eventbus.WithHandlerName("h-serial")); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	for i := 0; i < 3; i++ {
		if err := bus.Publish(ctx, "order.serial", map[string]string{"id": "x"}); err != nil {
			t.Fatalf("Publish %d: %v", i, err)
		}
	}
	waitCond(t, 3*time.Second, func() bool { return tr.current() == 1 },
		"want one in-flight message")
	time.Sleep(100 * time.Millisecond)
	if got := tr.current(); got != 1 {
		t.Fatalf("in-flight = %d, want 1 (default must be serial)", got)
	}
	close(release)
	for i := 0; i < 3; i++ {
		select {
		case <-acked:
		case <-time.After(3 * time.Second):
			t.Fatalf("only %d/3 messages acked", i)
		}
	}
	if got := tr.peak(); got != 1 {
		t.Fatalf("peak in-flight = %d, want 1", got)
	}
	stopWithin(t, bus, 2*time.Second)
}

// TestSubscribeMaxInFlightDefaultPreservesOrder 默认串行下同订阅处理顺序
// 与投递顺序一致（router 每消息一 goroutine 不再导致重排）。
func TestSubscribeMaxInFlightDefaultPreservesOrder(t *testing.T) {
	rt := &recordingTransport{topic: "order.seq"}
	acked := make(chan struct{}, 8)
	rt.onAck = func() { acked <- struct{}{} }
	bus := watermill.New(eventbus.Options{Transports: []eventbus.Transport{rt}})
	if err := bus.Init(nil); err != nil {
		t.Fatalf("Init: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = bus.Start(ctx) }()
	waitBus(t, bus)

	var mu sync.Mutex
	var order []string
	if err := bus.Subscribe(ctx, "order.seq", func(_ context.Context, e *eventbus.RawEvent) error {
		mu.Lock()
		order = append(order, e.ID)
		mu.Unlock()
		return nil
	}, eventbus.WithHandlerName("h-seq")); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	ids := []string{"m0", "m1", "m2", "m3", "m4"}
	for _, id := range ids {
		if err := rt.Publish(ctx, "order.seq", &eventbus.RawEvent{ID: id, Payload: []byte("x"), Headers: map[string]string{}}); err != nil {
			t.Fatalf("Publish %s: %v", id, err)
		}
	}
	for i := 0; i < len(ids); i++ {
		select {
		case <-acked:
		case <-time.After(3 * time.Second):
			t.Fatalf("only %d/%d messages acked", i, len(ids))
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(order) != len(ids) {
		t.Fatalf("handled %d messages, want %d", len(order), len(ids))
	}
	for i, id := range ids {
		if order[i] != id {
			t.Fatalf("order = %v, want %v (serial processing must preserve delivery order)", order, ids)
		}
	}
	stopWithin(t, bus, 2*time.Second)
}

// TestTopicMaxInFlightUnlimited 逃生口：WithTopicMaxInFlight(-1) 不限制在途，
// 三条消息同时进入 handler。
func TestTopicMaxInFlightUnlimited(t *testing.T) {
	rt := &recordingTransport{topic: "order.unlimited"}
	acked := make(chan struct{}, 4)
	rt.onAck = func() { acked <- struct{}{} }
	bus := watermill.New(eventbus.Options{Transports: []eventbus.Transport{rt}})
	if err := bus.Init(nil); err != nil {
		t.Fatalf("Init: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = bus.Start(ctx) }()
	waitBus(t, bus)

	release := make(chan struct{})
	var tr inflightTracker
	topic := eventbus.NewTopic[map[string]string]("order.unlimited", eventbus.WithTopicMaxInFlight(-1))
	if err := topic.Subscribe(ctx, func(context.Context, *eventbus.Event[map[string]string]) error {
		tr.enter()
		<-release
		tr.leave()
		return nil
	}, eventbus.WithBus(bus), eventbus.WithHandlerName("h-unlimited")); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	for i := 0; i < 3; i++ {
		if err := topic.Publish(ctx, map[string]string{"id": "x"}, eventbus.WithBus(bus)); err != nil {
			t.Fatalf("Publish %d: %v", i, err)
		}
	}
	waitCond(t, 3*time.Second, func() bool { return tr.current() == 3 },
		"negative max_in_flight must leave in-flight unbounded")
	close(release)
	for i := 0; i < 3; i++ {
		select {
		case <-acked:
		case <-time.After(3 * time.Second):
			t.Fatalf("only %d/3 messages acked", i)
		}
	}
	stopWithin(t, bus, 2*time.Second)
}

// TestSubscribeMaxInFlightReleasedOnNack 失败（Nack）也必须释放槽位：
// 上限 1 下第一条失败后，后续消息仍能被处理（槽位不泄漏）。
func TestSubscribeMaxInFlightReleasedOnNack(t *testing.T) {
	nacked := make(chan struct{}, 4)
	rt := &recordingTransport{topic: "order.nack", onNack: func() { nacked <- struct{}{} }}
	bus := watermill.New(eventbus.Options{
		Transports: []eventbus.Transport{rt},
		Retry:      &eventbus.RetryOptions{MaxRetries: 0},
	})
	if err := bus.Init(nil); err != nil {
		t.Fatalf("Init: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = bus.Start(ctx) }()
	waitBus(t, bus)

	if err := bus.Subscribe(ctx, "order.nack", func(context.Context, *eventbus.RawEvent) error {
		return errors.New("always fail")
	}, eventbus.WithHandlerName("h-nack")); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	for i := 0; i < 3; i++ {
		if err := bus.Publish(ctx, "order.nack", map[string]string{"id": "x"}); err != nil {
			t.Fatalf("Publish %d: %v", i, err)
		}
	}
	for i := 0; i < 3; i++ {
		select {
		case <-nacked:
		case <-time.After(3 * time.Second):
			t.Fatalf("only %d/3 messages nacked; slot leaked on failure?", i)
		}
	}
	stopWithin(t, bus, 2*time.Second)
}

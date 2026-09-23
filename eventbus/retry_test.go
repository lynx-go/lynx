package eventbus

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

var errRetryTest = errors.New("retry test failure")

// startBus 启动一个内存 Bus 并等待运行就绪。
func startBus(t *testing.T) Bus {
	t.Helper()
	bus := NewMemoryBus(Options{})
	if err := bus.Init(nil); err != nil {
		t.Fatalf("Init: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = bus.Start(ctx) }()
	waitRunning(t, bus)
	return bus
}

// TestTopicRetryTakesEffectOnSubscribe 验证 WithTopicRetry 真正接入订阅路径
// （设计文档 §10.4 四级合并：调用选项 > Topic > Topics[t] > 全局）。
// Topic 设 MaxRetries=0：handler 恒失败时必须只调用一次；
// 若 Topic 级被忽略，全局默认 MaxRetries=3 会调用四次。
func TestTopicRetryTakesEffectOnSubscribe(t *testing.T) {
	bus := startBus(t)
	topic := NewTopic[map[string]string]("retry.topic-zero", WithTopicRetry(RetryOptions{MaxRetries: 0}))

	var calls atomic.Int32
	if err := topic.Subscribe(context.Background(), func(ctx context.Context, e *Event[map[string]string]) error {
		calls.Add(1)
		return errRetryTest
	}, WithBus(bus)); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := topic.Publish(context.Background(), map[string]string{"k": "v"}, WithBus(bus)); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("calls = %d, want 1 (topic MaxRetries=0 must disable retries)", got)
	}
}

// TestSubscribeRetryOptionOverridesTopic 验证调用级 WithSubscribeRetry 覆盖 Topic 基础值：
// Topic 设 0 次、调用设 2 次，前两次失败第三次成功 → 共三次调用。
func TestSubscribeRetryOptionOverridesTopic(t *testing.T) {
	bus := startBus(t)
	topic := NewTopic[map[string]string]("retry.override", WithTopicRetry(RetryOptions{MaxRetries: 0}))

	var calls atomic.Int32
	done := make(chan struct{})
	if err := topic.Subscribe(context.Background(), func(ctx context.Context, e *Event[map[string]string]) error {
		if calls.Add(1) < 3 {
			return errRetryTest
		}
		close(done)
		return nil
	}, WithBus(bus), WithSubscribeRetry(RetryOptions{MaxRetries: 2})); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := topic.Publish(context.Background(), map[string]string{"k": "v"}, WithBus(bus)); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never succeeded")
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("calls = %d, want 3 (call-level MaxRetries=2 overrides topic 0)", got)
	}
}

// TestTopicsConfigRetryFallback 验证 Options.Topics[t].Retry 作为无调用/Topic 级时的回填。
func TestTopicsConfigRetryFallback(t *testing.T) {
	bus := NewMemoryBus(Options{
		Topics: map[string]TopicConfig{"retry.cfg": {Retry: &RetryOptions{MaxRetries: 0}}},
	})
	if err := bus.Init(nil); err != nil {
		t.Fatalf("Init: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = bus.Start(ctx) }()
	waitRunning(t, bus)

	var calls atomic.Int32
	if err := bus.Subscribe(context.Background(), "retry.cfg", func(ctx context.Context, e *RawEvent) error {
		calls.Add(1)
		return errRetryTest
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := bus.Publish(context.Background(), "retry.cfg", map[string]string{"k": "v"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("calls = %d, want 1 (Topics[t].Retry MaxRetries=0 must disable retries)", got)
	}
}

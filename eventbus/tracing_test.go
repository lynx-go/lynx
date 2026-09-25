package eventbus

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// w3cTraceparent 是 W3C Trace Context 示例 traceparent。
const w3cTraceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

// installTestTracer 安装测试 trace 管线为 otel 全局，注册还原（用例串行）。
func installTestTracer(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
		_ = tp.Shutdown(context.Background())
	})
	return sr
}

// TestPublishInjectsTraceContext：发布组装时把 active span 的 W3C 上下文注入
// 事件头（消费侧可还原同一 trace 与父 span）。
func TestPublishInjectsTraceContext(t *testing.T) {
	installTestTracer(t)
	ctx, span := otel.Tracer(tracerName).Start(context.Background(), "publisher")
	defer span.End()

	raw, err := BuildRawEvent(ctx, NewMemoryBus(Options{}), "t", map[string]string{"a": "b"}, &PublishOptions{}, nil)
	if err != nil {
		t.Fatalf("BuildRawEvent: %v", err)
	}
	tp := raw.Headers["traceparent"]
	if !strings.Contains(tp, span.SpanContext().TraceID().String()) {
		t.Fatalf("traceparent = %q, want trace id %s", tp, span.SpanContext().TraceID())
	}
	got := trace.SpanContextFromContext(
		propagation.TraceContext{}.Extract(context.Background(), propagation.MapCarrier(raw.Headers)))
	if got.TraceID() != span.SpanContext().TraceID() || got.SpanID() != span.SpanContext().SpanID() {
		t.Fatalf("extracted = %v, want %v", got, span.SpanContext())
	}
}

// TestPublishWithoutSpanKeepsHeadersUntouched：无 active span 不写 traceparent；
// 调用方显式带入的 traceparent 保持原值（桥接场景不被清空）。
func TestPublishWithoutSpanKeepsHeadersUntouched(t *testing.T) {
	installTestTracer(t)
	bus := NewMemoryBus(Options{})
	raw, err := BuildRawEvent(context.Background(), bus, "t", []byte("x"), &PublishOptions{}, nil)
	if err != nil {
		t.Fatalf("BuildRawEvent: %v", err)
	}
	if v, ok := raw.Headers["traceparent"]; ok {
		t.Fatalf("unexpected traceparent %q without active span", v)
	}

	raw2, err := BuildRawEvent(context.Background(), bus, "t",
		&RawEvent{Headers: map[string]string{"traceparent": w3cTraceparent}}, &PublishOptions{}, nil)
	if err != nil {
		t.Fatalf("BuildRawEvent(*RawEvent): %v", err)
	}
	if got := raw2.Headers["traceparent"]; got != w3cTraceparent {
		t.Fatalf("existing traceparent = %q, want preserved %q", got, w3cTraceparent)
	}
}

// TestInvokeHandlerContinuesRemoteTrace：事件头带远端 traceparent 时开
// consumer span——子 span、consumer kind、handler ctx 即该 span，附带
// messaging 属性。
func TestInvokeHandlerContinuesRemoteTrace(t *testing.T) {
	sr := installTestTracer(t)
	ev := testEvent()
	ev.Topic = "order.created"
	ev.ID = "m1"
	ev.Headers["traceparent"] = w3cTraceparent

	var handlerTraceID, handlerSpanID string
	err := InvokeHandler(context.Background(), discardLogger(), func(ctx context.Context, _ *RawEvent) error {
		sc := trace.SpanContextFromContext(ctx)
		handlerTraceID, handlerSpanID = sc.TraceID().String(), sc.SpanID().String()
		return nil
	}, ev, NewResolver(Options{}), InvokeOptions{
		Topic: "order.created", HandlerName: "h", Retry: RetryOptions{MaxRetries: 0},
	})
	if err != nil {
		t.Fatalf("InvokeHandler: %v", err)
	}

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(spans))
	}
	span := spans[0]
	if span.Name() != "consume order.created" {
		t.Errorf("span name = %q", span.Name())
	}
	if span.SpanKind() != trace.SpanKindConsumer {
		t.Errorf("span kind = %v, want consumer", span.SpanKind())
	}
	if span.Parent().SpanID().String() != "00f067aa0ba902b7" {
		t.Errorf("parent span = %s, want remote parent 00f067aa0ba902b7", span.Parent().SpanID())
	}
	if span.SpanContext().TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("trace id = %s, want remote trace", span.SpanContext().TraceID())
	}
	if handlerTraceID != span.SpanContext().TraceID().String() || handlerSpanID != span.SpanContext().SpanID().String() {
		t.Errorf("handler span = (%s,%s), want consume span (%s,%s)",
			handlerTraceID, handlerSpanID, span.SpanContext().TraceID(), span.SpanContext().SpanID())
	}
	attrs := map[string]string{}
	for _, a := range span.Attributes() {
		attrs[string(a.Key)] = a.Value.AsString()
	}
	if attrs["messaging.destination.name"] != "order.created" || attrs["messaging.message.id"] != "m1" {
		t.Errorf("attributes = %v", attrs)
	}
	if span.Status().Code == codes.Error {
		t.Errorf("successful consume span must not be marked error")
	}
}

// TestInvokeHandlerRecordsTerminalErrorOnSpan：终态失败记入 consumer span。
func TestInvokeHandlerRecordsTerminalErrorOnSpan(t *testing.T) {
	sr := installTestTracer(t)
	ev := testEvent()
	ev.Headers["traceparent"] = w3cTraceparent
	wantErr := errors.New("boom")

	err := InvokeHandler(context.Background(), discardLogger(), func(context.Context, *RawEvent) error {
		return wantErr
	}, ev, NewResolver(Options{}), InvokeOptions{Topic: "t", HandlerName: "h", Retry: RetryOptions{MaxRetries: 0}})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(spans))
	}
	if spans[0].Status().Code != codes.Error {
		t.Fatalf("span status = %v, want error", spans[0].Status())
	}
}

// TestInvokeHandlerWithoutRemoteTraceStartsNoSpan：无远端上下文时不新增 span
// ——未接入 trace 的部署行为与旧版一致。
func TestInvokeHandlerWithoutRemoteTraceStartsNoSpan(t *testing.T) {
	sr := installTestTracer(t)
	err := InvokeHandler(context.Background(), discardLogger(), func(context.Context, *RawEvent) error {
		return nil
	}, testEvent(), NewResolver(Options{}), InvokeOptions{Topic: "t", HandlerName: "h", Retry: RetryOptions{MaxRetries: 0}})
	if err != nil {
		t.Fatalf("InvokeHandler: %v", err)
	}
	if spans := sr.Ended(); len(spans) != 0 {
		t.Fatalf("unexpected spans without remote trace context: %v", spans)
	}
}

// TestBusPropagatesTraceEndToEnd：内存 Bus 端到端——发布侧 active span 的
// trace 经事件头抵达消费 handler 的 ctx（同一 trace）。
func TestBusPropagatesTraceEndToEnd(t *testing.T) {
	installTestTracer(t)
	bus := NewMemoryBus(Options{})
	_ = bus.Init(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = bus.Start(ctx) }()
	waitRunning(t, bus)

	topic := NewTopic[map[string]string]("trace.e2e")
	got := make(chan trace.SpanContext, 1)
	if err := topic.Subscribe(context.Background(), func(hctx context.Context, _ *Event[map[string]string]) error {
		got <- trace.SpanContextFromContext(hctx)
		return nil
	}, WithBus(bus)); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	time.Sleep(20 * time.Millisecond)

	pubCtx, span := otel.Tracer(tracerName).Start(context.Background(), "publisher")
	defer span.End()
	if err := topic.Publish(pubCtx, map[string]string{"a": "b"}, WithBus(bus)); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case sc := <-got:
		if sc.TraceID() != span.SpanContext().TraceID() {
			t.Fatalf("handler trace = %s, want %s", sc.TraceID(), span.SpanContext().TraceID())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for handler")
	}
}

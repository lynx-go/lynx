package eventbus

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// tracerName 是 eventbus 的 OTel instrumentation scope 名。
const tracerName = "github.com/lynx-go/lynx/eventbus"

// injectTrace 把 ctx 的当前 trace 上下文（W3C traceparent / tracestate，经
// 全局 propagator）注入事件头——发布侧传播的唯一入口，消费侧据此续链。
// 无有效 span 时不写入（全局 no-op provider 下零行为变化）；事件头中已有
// traceparent 且当前无 span 时保持原值（显式桥接场景不被清空）。
func injectTrace(ctx context.Context, headers map[string]string) {
	if headers == nil || ctx == nil {
		return
	}
	otel.GetTextMapPropagator().Inject(ctx, propagation.MapCarrier(headers))
}

// startConsumeSpan 从事件头提取远端 trace 上下文，并在其上开 consumer span
// （名 "consume <topic>"，覆盖该消息的全部重试尝试）；无有效远端上下文时
// 不新增 span（返回 no-op，行为与未接入 trace 时一致）。
func startConsumeSpan(ctx context.Context, ev *RawEvent, topic string) (context.Context, trace.Span) {
	if ev == nil {
		return ctx, trace.SpanFromContext(ctx)
	}
	remote := otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier(ev.Headers))
	if !trace.SpanContextFromContext(remote).IsValid() {
		return ctx, trace.SpanFromContext(ctx)
	}
	return otel.Tracer(tracerName).Start(remote, "consume "+topic,
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			attribute.String("messaging.destination.name", topic),
			attribute.String("messaging.message.id", ev.ID),
		),
	)
}

// recordSpanError 把终态错误记到消费 span 上（吞掉/自动确认的失败不入 span，
// 与 Bus 的确认语义一致：那些失败对消息流而言是成功）。
func recordSpanError(span trace.Span, err error) {
	if span == nil || err == nil {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}

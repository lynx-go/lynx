package eventbus

import (
	"context"
	"log/slog"
	"time"

	"github.com/lynx-go/lynx/logging"
)

// InvokeOptions 是共享投递执行器的输入：
//   - Retry：解析后的重试策略（Resolver.RetryFor 的结果）；
//   - Once：AutoAck 语义——只调用一次、不重试（失败仅记日志、恒返回 nil，
//     持久化后端上不参与整条消息的确认裁决）；
//   - Swallow：ContinueOnError 语义——失败记录后吞掉（返回 nil）。
type InvokeOptions struct {
	Topic       string
	HandlerName string
	Retry       RetryOptions
	Once        bool
	Swallow     bool
}

// InvokeHandler 执行一次订阅投递（两种 Bus 的唯一执行点）：构建 handler ctx
// （传播键日志属性 + 逻辑 topic 值）、记录 received 日志、按固定退避重试
// （WK-17：不做指数退避）、逐次失败日志；返回终态错误。
//
// 返回 nil 表示成功或已被 Once/Swallow 吞掉；非 nil 表示重试耗尽仍失败
// （或退避期间 ctx 取消），由适配器决定 Nack / 丢弃。ack 时序（AutoAck
// 先 Ack，WK-13）留在适配器：本执行器不接触消息确认。
func InvokeHandler(ctx context.Context, logger *slog.Logger, h HandlerFunc, ev *RawEvent, resolver *Resolver, opts InvokeOptions) error {
	// 逻辑 topic 值：历史键型保持兼容（内存 Bus 既有语义）。
	hCtx := context.WithValue(ctx, struct{ string }{"x-bus-topic"}, ev.Topic)
	// 还原发布侧日志属性：只补 handler ctx 中尚不存在的键。
	existing := map[string]struct{}{}
	for _, a := range logging.AttrsFrom(hCtx) {
		existing[a.Key] = struct{}{}
	}
	var attrs []slog.Attr
	for _, k := range resolver.PropagateKeys() {
		if _, ok := existing[k]; ok {
			continue
		}
		if v, ok := ev.Headers[k]; ok && v != "" {
			attrs = append(attrs, slog.String(k, v))
		}
	}
	hCtx = logging.WithAttrs(hCtx, attrs...)

	if lm := resolver.LogMessageFor(opts.Topic); lm.Subscribe {
		logger.DebugContext(hCtx, "received event", "topic", opts.Topic, "handler", opts.HandlerName)
	}

	if opts.Once {
		// AutoAck：先确认由适配器完成；这里只调用一次，失败仅记录。
		if err := h(hCtx, ev); err != nil {
			logger.ErrorContext(hCtx, "handler failed (auto_ack, not retried)", "error", err, "handler", opts.HandlerName)
		}
		return nil
	}

	var err error
	for attempt := 0; attempt <= opts.Retry.MaxRetries; attempt++ {
		err = h(hCtx, ev)
		if err == nil {
			return nil
		}
		if opts.Swallow {
			logger.ErrorContext(hCtx, "handler failed but continue_on_error is set", "error", err, "handler", opts.HandlerName)
			return nil
		}
		if attempt < opts.Retry.MaxRetries {
			if opts.Retry.Backoff > 0 {
				select {
				case <-time.After(opts.Retry.Backoff):
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			logger.ErrorContext(hCtx, "handler failed, retrying", "error", err, "attempt", attempt+1, "handler", opts.HandlerName)
		}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	logger.ErrorContext(hCtx, "handler failed after retries", "error", err, "handler", opts.HandlerName)
	return err
}

package serverkit

import (
	"context"
	"log/slog"

	"github.com/google/uuid"
	"github.com/lynx-go/lynx/internal/propagation"
	"github.com/lynx-go/lynx/logging"
)

// wire 键与校验规则的中性归属是 internal/propagation（server 适配器、
// 客户端与 eventbus 共用）；serverkit 只做面向 server 的转发与入站解析。
const (
	// RequestIDKey / UserIDKey 是 request_id / user_id 的共享 wire 键。
	RequestIDKey = propagation.RequestIDHeader
	UserIDKey    = propagation.UserIDHeader
	// MaxPropagationValueLength 是入站传播值的最大长度（SC-22）。
	MaxPropagationValueLength = propagation.MaxValueLength
)

// ValidPropagationValue 判定入站传播值是否可安全沿用。
func ValidPropagationValue(v string) bool { return propagation.Valid(v) }

// AttrsFromValues 构造传播日志属性（request_id / user_id）：值为空或非法
// 时跳过对应字段（不注入日志）。
func AttrsFromValues(requestID, userID string) []slog.Attr {
	return propagation.Attrs(requestID, userID)
}

// ResolveInbound 解析入站请求标识（HTTP 中间件与 gRPC 拦截器共用）：
// 合法值沿用；request_id 缺失/非法时生成新 UUID（返回给调用方回写响应
// 头/metadata）；user_id 非法时丢弃（没有可生成的语义）。返回解析后的
// request_id 与日志属性。
func ResolveInbound(requestID, userID string) (string, []slog.Attr) {
	if !propagation.Valid(requestID) {
		requestID = uuid.NewString()
	}
	return requestID, propagation.Attrs(requestID, userID)
}

// PropagateOutbound 把 ctx 中待传播的请求标识经 set 写入出站载体（委托
// internal/propagation.Outbound）：键为共享 wire 键，已存在不覆盖。
func PropagateOutbound(ctx context.Context, set func(key, value string) bool) bool {
	return propagation.Outbound(ctx, set)
}

// RequestIDFrom 返回 ctx 中的 request_id，未设置时返回空字符串。
func RequestIDFrom(ctx context.Context) string {
	for _, a := range logging.AttrsFrom(ctx) {
		if a.Key == logging.FieldRequestID {
			return a.Value.String()
		}
	}
	return ""
}

package serverkit

import (
	"context"
	"log/slog"

	"github.com/lynx-go/lynx/logging"
)

// wire 键：HTTP header 名与 gRPC metadata 键同源。gRPC metadata 规范要求
// 小写；HTTP 头名大小写不敏感（net/http 会规格化），同一常量两侧通用。
// 中划线键同时规避 Envoy 等代理对下划线 header 的默认拒绝（SC-18）。
const (
	RequestIDKey = "x-request-id"
	UserIDKey    = "x-user-id"
)

// MaxPropagationValueLength 是入站传播值的最大长度（SC-22）：超长或含
// 非法字符的值通常是异常客户端/攻击载荷，直接丢弃/重新生成，不注入
// 日志（避免刷日志与污染下游）。
const MaxPropagationValueLength = 128

// ValidPropagationValue 判定入站传播值是否可安全沿用：非空、长度 ≤128、
// 字符集限定 [A-Za-z0-9-_]（UUID/常见追踪 ID 均落在该集合内）。
func ValidPropagationValue(v string) bool {
	if v == "" || len(v) > MaxPropagationValueLength {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z',
			c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// AttrsFromValues 构造传播日志属性（request_id / user_id）：值为空或非法
// 时跳过对应字段（不注入日志）。
func AttrsFromValues(requestID, userID string) []slog.Attr {
	var attrs []slog.Attr
	if ValidPropagationValue(requestID) {
		attrs = append(attrs, slog.String(logging.FieldRequestID, requestID))
	}
	if ValidPropagationValue(userID) {
		attrs = append(attrs, slog.String(logging.FieldUserID, userID))
	}
	return attrs
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

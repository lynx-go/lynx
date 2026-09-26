// Package propagation 是请求标识（request_id / user_id）传播规则的唯一归属：
// wire 键、入站值校验、日志属性构造与出站传播。server 适配器（HTTP/gRPC）、
// 客户端与 eventbus 共用同一套规则；包本身不依赖 lynx 根包或传输库，避免
// 层级耦合。
package propagation

import (
	"context"
	"log/slog"

	"github.com/lynx-go/lynx/logging"
)

// wire 键：HTTP header 名与 gRPC metadata 键同源。gRPC metadata 规范要求
// 小写；HTTP 头名大小写不敏感（net/http 会规格化），同一常量两侧通用。
// 中划线键同时规避 Envoy 等代理对下划线 header 的默认拒绝（SC-18）。
const (
	RequestIDHeader = "x-request-id"
	UserIDHeader    = "x-user-id"
)

// MaxValueLength 是传播值的最大长度（SC-22）：超长或含非法字符的值通常是
// 异常客户端/攻击载荷，直接丢弃/重新生成，不注入日志（避免刷日志与污染下游）。
const MaxValueLength = 128

// Valid 判定传播值是否可安全沿用：非空、长度 ≤ MaxValueLength、字符集限定
// [A-Za-z0-9-_]（UUID/常见追踪 ID 均落在该集合内）。
func Valid(v string) bool {
	if v == "" || len(v) > MaxValueLength {
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

// Attrs 构造传播日志属性（request_id / user_id）：值为空或非法时跳过对应
// 字段（不注入日志）。
func Attrs(requestID, userID string) []slog.Attr {
	var attrs []slog.Attr
	if Valid(requestID) {
		attrs = append(attrs, slog.String(logging.FieldRequestID, requestID))
	}
	if Valid(userID) {
		attrs = append(attrs, slog.String(logging.FieldUserID, userID))
	}
	return attrs
}

// Outbound 把 ctx 中待传播的请求标识（request_id / user_id）经 set 写入
// 出站载体：键为共享 wire 键，按 request_id → user_id 顺序各调一次。
// set 返回 false 表示目标已存在该键（不覆盖语义，显式设置优先）。
// 返回是否写入了至少一个键；ctx 无白名单属性时 set 不被调用。
//
// 出站不做值校验：入站侧（HTTP/gRPC）已校验并重新生成，业务显式写入 ctx
// 的值由业务负责；接收方仍会在入站边界校验。
func Outbound(ctx context.Context, set func(key, value string) bool) bool {
	if set == nil {
		return false
	}
	var requestID, userID string
	for _, a := range logging.AttrsFrom(ctx) {
		switch a.Key {
		case logging.FieldRequestID:
			requestID = a.Value.String()
		case logging.FieldUserID:
			userID = a.Value.String()
		}
	}
	added := requestID != "" && set(RequestIDHeader, requestID)
	if userID != "" && set(UserIDHeader, userID) {
		added = true
	}
	return added
}

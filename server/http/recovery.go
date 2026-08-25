package http

import (
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
)

// RecoveryOption 用于配置 Recovery 中间件的选项函数。
type RecoveryOption func(*recoveryOptions)

type recoveryOptions struct {
	handler ErrorHandler
}

// WithRecoveryHandler 设置 panic 恢复时使用的 ErrorHandler，缺省为
// DefaultErrorHandler（panic 一律按 500 + 统一 JSON 错误体处理；错误消息
// 为 panic 值的字符串形式）。自定义实现可自行决定状态码与响应体格式。
func WithRecoveryHandler(h ErrorHandler) RecoveryOption {
	return func(o *recoveryOptions) {
		o.handler = h
	}
}

// Recovery 返回 panic 恢复中间件：捕获下游 handler（及其余中间件）抛出的
// panic，记一条 Error 日志（字段 panic 为 panic 值的字符串形式、stack 为
// 完整调用栈），并经 ErrorHandler 写响应——缺省 DefaultErrorHandler 写
// 500 + 通用 JSON 错误体（panic 值可能含连接串/路径等内部细节，不回传
// 客户端，SC-04）。恢复后连接保持可用，后续请求不受影响。
//
// 推荐声明在 WithMiddleware 的第一个参数（最外层）：链内任意一环（含其余
// 中间件与业务 handler）的 panic 都能被恢复，不会拖垮整个进程。与之配套
// 的顺序约定为 Recovery → RequestID → 其余中间件（SC-13）：Recovery 必须
// 保持最外层保命，RequestID 放在其内侧——代价是 Recovery 记录的 panic
// 日志拿不到 request_id（RequestID 尚未执行），属已知取舍，靠
// method/path 字段关联；若业务更看重 panic 日志的 request_id，可自行
// 将 RequestID 提到最外层并接受内层 panic 无人恢复的风险。
//
// 注意：若 panic 发生在响应已开始之后（下游已写过响应头/体），ErrorHandler
// 只能尽力写错误体（可能追加进已发出的响应），无法改写已发送的部分——
// 与 net/http 自身的 panic 兜底行为一致；业务应避免"先写响应再 panic"。
func Recovery(opts ...RecoveryOption) Middleware {
	o := recoveryOptions{}
	for _, opt := range opts {
		opt(&o)
	}
	if o.handler == nil {
		o.handler = DefaultErrorHandler
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if p := recover(); p != nil {
					stack := debug.Stack()
					slog.ErrorContext(r.Context(), "http handler panic recovered",
						"panic", fmt.Sprint(p),
						"stack", string(stack),
					)
					o.handler(r.Context(), w, r, panicError{v: p})
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// panicError 把 recover() 的 panic 值转换为 error，Error() 返回 panic 值
// 的字符串形式，供 ErrorHandler 写入错误响应。
type panicError struct{ v any }

func (e panicError) Error() string { return fmt.Sprint(e.v) }

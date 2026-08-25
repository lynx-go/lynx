package http

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
)

// ErrorHandler 处理业务 handler 返回的错误：向客户端写入状态码与错误响应体。
// 框架提供 DefaultErrorHandler 作为默认实现（StatusError 约定 + 统一 JSON
// 错误体）；自定义实现可自行决定状态码与响应体格式。ctx 由调用方传入，
// NewErrorHandler 传入 r.Context()——携带请求级日志属性（见
// logging.WithAttrs）时，经 logging.NewAttrsHandler 装饰的日志器会自动
// 为错误日志附加 request_id 等字段。
type ErrorHandler func(ctx context.Context, w http.ResponseWriter, r *http.Request, err error)

// StatusError 由业务错误类型实现，声明自身对应的 HTTP 状态码。
// DefaultErrorHandler 优先取该接口的状态码，未实现该接口的错误一律 500。
// 支持错误包装（errors.As 查找），被包装的错误同样生效。
type StatusError interface {
	error
	StatusCode() int
}

// DefaultErrorHandler 是框架默认的 ErrorHandler：
//   - 错误实现 StatusError 时以其 StatusCode() 为状态码，否则 500；
//   - 响应体统一为 {"error":{"message":...}}（application/json）：4xx
//     （业务声明的错误）原样透传 err.Error()；5xx 返回通用消息
//     （http.StatusText），err.Error() 可能携带 DSN/文件路径/内部状态等
//     细节，只进日志不回传客户端（SC-04 信息泄露）；
//   - 仅 5xx 记一条 Error 日志（slog.ErrorContext，日志器为
//     slog.Default()，字段 method/path/status/error——需要注入自定义
//     日志器见 DefaultErrorHandlerWithLogger，SC-19）；非 5xx（业务
//     声明的 4xx 等）不记日志，由业务自行决定是否需要记录。
//
// 若响应已开始（w 已写过 header——NewErrorHandler 传入的包装器可标记），
// 不再改写响应、仅记录一条 Error 日志（status 取已写入的状态码），避免
// 触发 superfluous WriteHeader 与损坏已发出的响应体。
func DefaultErrorHandler(ctx context.Context, w http.ResponseWriter, r *http.Request, err error) {
	defaultErrorHandler(ctx, slog.Default(), w, r, err)
}

// DefaultErrorHandlerWithLogger 返回携带注入日志实例的默认
// ErrorHandler：行为与 DefaultErrorHandler 完全一致，仅错误日志改走
// 给定 logger（SC-19：包级 DefaultErrorHandler 只能回退全局 slog，
// 服务侧可经本变体接入 WithLogger 的实例，如
// Recovery(WithRecoveryHandler(DefaultErrorHandlerWithLogger(srvLogger)))）。
// logger 为 nil 时回退 slog.Default()。
func DefaultErrorHandlerWithLogger(logger *slog.Logger) ErrorHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request, err error) {
		defaultErrorHandler(ctx, logger, w, r, err)
	}
}

// defaultErrorHandler 是 DefaultErrorHandler 的实现主体，logger 由
// 调用方注入（DefaultErrorHandler 传 slog.Default()，SC-19）。
func defaultErrorHandler(ctx context.Context, logger *slog.Logger, w http.ResponseWriter, r *http.Request, err error) {
	if tw, ok := w.(headerWriter); ok && tw.headerWritten() {
		logHTTPError(ctx, logger, "http handler error after response started", r, tw.statusCode(), err)
		return
	}
	status := http.StatusInternalServerError
	var se StatusError
	if errors.As(err, &se) {
		status = se.StatusCode()
	}
	if status >= http.StatusInternalServerError {
		// 5xx 细节不回传客户端（SC-04）：通用消息 + 详情进日志，
		// 日志与响应经 method/path/status 关联。
		writeJSONErrorMessage(w, status, http.StatusText(status))
		logHTTPError(ctx, logger, "http handler error", r, status, err)
		return
	}
	writeJSONError(w, status, err)
}

// HandleFunc 是带错误返回的业务 handler 签名：ctx 为请求上下文
// （r.Context()），w 为响应 writer，r 为请求。返回 nil 表示业务已自行
// 写好响应；返回错误时由 NewErrorHandler 交给 ErrorHandler 处理。
type HandleFunc func(ctx context.Context, w http.ResponseWriter, r *http.Request) error

// NewErrorHandler 将返回错误的业务 handler 包装为标准 http.Handler：
//   - fn 返回 nil：表示业务已自行写好响应，不做任何处理；
//   - fn 返回错误：调用 h 写错误响应，h 为 nil 时使用
//     DefaultErrorHandler；
//   - fn 内 panic 不在此处恢复（panic 恢复由专门的 Recovery 中间件负责，
//     不在 v1.1 范围内）。
//
// 传给 fn 的 w 是轻量包装 writer：记录响应是否已开始（首次
// WriteHeader/Write 即视为已开始），并保留 Flusher/Hijacker 能力
// （Flush/Hijack 透传 + Unwrap 支持 http.ResponseController 穿透）。
// 若 fn 已写过响应头后又返回错误，h 仍会被调用，但 DefaultErrorHandler
// 检测到响应已开始后不再改写响应、仅记日志，不会触发 superfluous
// WriteHeader；自定义 h 需自行处理该情形。
//
// 服务器级默认 ErrorHandler（WithErrorHandler 选项，h 传 nil 时的兜底
// 改取服务器级默认）不在 v1.1 范围内，定位 v1.2；当前 h 传 nil 一律使用
// 包级 DefaultErrorHandler。
func NewErrorHandler(h ErrorHandler, fn HandleFunc) http.Handler {
	if h == nil {
		h = DefaultErrorHandler
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tw := &trackedWriter{ResponseWriter: w}
		if err := fn(r.Context(), tw, r); err != nil {
			h(r.Context(), tw, r, err)
		}
	})
}

// errorResponse 是默认错误响应体结构：{"error":{"message":...}}。
type errorResponse struct {
	Error errorMessage `json:"error"`
}

type errorMessage struct {
	Message string `json:"message"`
}

// writeJSONError 以统一 JSON 错误体写错误响应（消息原样透传，仅用于
// 4xx 业务错误等可安全暴露的消息；5xx 走 writeJSONErrorMessage）。
func writeJSONError(w http.ResponseWriter, status int, err error) {
	writeJSONErrorMessage(w, status, err.Error())
}

// writeJSONErrorMessage 以指定消息写统一 JSON 错误体。
func writeJSONErrorMessage(w http.ResponseWriter, status int, msg string) {
	b, _ := json.Marshal(errorResponse{Error: errorMessage{Message: msg}})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// logHTTPError 记录一条 Error 日志，字段 method/path/status/error。
// ctx 通常为 r.Context()（NewErrorHandler 传入），携带请求级日志属性
// （request_id 等）时经 logging.NewAttrsHandler 自动附加到记录；
// logger 由调用方注入（SC-19），DefaultErrorHandler 传 slog.Default()。
func logHTTPError(ctx context.Context, logger *slog.Logger, msg string, r *http.Request, status int, err error) {
	logger.ErrorContext(ctx, msg,
		"method", r.Method,
		"path", r.URL.Path,
		"status", status,
		"error", err,
	)
}

// headerWriter 标记响应是否已开始，由 NewErrorHandler 包装的
// trackedWriter 实现，供 DefaultErrorHandler 判断是否还能写响应。
type headerWriter interface {
	headerWritten() bool
	statusCode() int
}

// trackedWriter 记录响应状态（首次 WriteHeader/Write 即标记已写入），
// 其余行为透传底层 writer；保留 Flusher/Hijacker 能力（与 requestlog 的
// responseStats 同款守卫模式），Unwrap 使 http.ResponseController 可
// 穿透包装层。
type trackedWriter struct {
	http.ResponseWriter
	code int
}

func (w *trackedWriter) headerWritten() bool { return w.code != 0 }

func (w *trackedWriter) statusCode() int { return w.code }

func (w *trackedWriter) WriteHeader(statusCode int) {
	if w.code == 0 {
		w.code = statusCode
	}
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *trackedWriter) Write(p []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

func (w *trackedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *trackedWriter) Flush() {
	if fl, ok := w.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}

func (w *trackedWriter) Hijack() (_ net.Conn, _ *bufio.ReadWriter, err error) {
	if hj, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, errors.New("underlying ResponseWriter does not support hijacking")
}

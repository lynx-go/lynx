package http

import (
	"bufio"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/trace"
)

// Logger wraps the Log method. Log must be safe to call from multiple
// goroutines. Log must not hold onto an Entry after it returns.
type Logger interface {
	Log(*Entry)
}

// NewHandler 包装 h，为每个请求生成一条访问日志
// （替代 gocloud.dev/server/requestlog.NewHandler）。
func NewHandler(log Logger, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sc := trace.SpanContextFromContext(r.Context())
		ent := &Entry{
			Request:           cloneRequestWithoutBody(r),
			ReceivedTime:      start,
			RequestHeaderSize: headerSize(r.Header),
			UserAgent:         r.UserAgent(),
			Referer:           r.Referer(),
			RemoteIP:          ipFromHostPort(r.RemoteAddr),
			TraceID:           sc.TraceID(),
			SpanID:            sc.SpanID(),
		}
		r2 := new(http.Request)
		*r2 = *r
		rcc := &readCounterCloser{r: r.Body}
		r2.Body = rcc
		w2 := &responseStats{w: w}

		h.ServeHTTP(w2, r2)

		ent.Latency = time.Since(start)
		// The handler may or may not have read the entire body. If the request
		// includes a Content-Length header, use that for a more accurate
		// RequestBodySize.
		ent.RequestBodySize = rcc.n
		if contentLengthStr := r.Header.Get("Content-Length"); contentLengthStr != "" {
			ent.RequestBodySize, _ = strconv.ParseInt(contentLengthStr, 10, 64)
		}
		ent.Status = w2.code
		if ent.Status == 0 {
			ent.Status = http.StatusOK
		}
		ent.ResponseHeaderSize, ent.ResponseBodySize = w2.size()
		// request_id 由中间件写入响应头（如 WithRequestID），这里在请求
		// 完成后读取，保证 request log 与业务日志携带同一 request_id。
		ent.RequestID = w2.Header().Get(RequestIDHeader)
		log.Log(ent)
	})
}

func cloneRequestWithoutBody(r *http.Request) *http.Request {
	// 浅拷贝即可（SC-09）：Entry 只读取 Method/URL 等字段，r.Clone 的
	// header 深拷贝是每请求一次的无谓分配潮；浅拷贝同样把 Body 隔离为
	// nil，日志侧不会误读请求体（Logger 契约保证不持有 Entry）。
	r2 := new(http.Request)
	*r2 = *r
	r2.Body = nil
	return r2
}

// Entry 记录一次已完成的 HTTP 请求的信息
// （替代 gocloud.dev/server/requestlog.Entry）。
type Entry struct {
	// Request 是已完成的请求。
	//
	// 此请求的 Body 恒为 nil，与实际请求体无关。
	Request *http.Request

	ReceivedTime    time.Time
	RequestBodySize int64

	Status             int
	ResponseHeaderSize int64
	ResponseBodySize   int64
	Latency            time.Duration
	TraceID            trace.TraceID
	SpanID             trace.SpanID

	// 以下字段为 Stackdriver 格式兼容保留，均可从 Request 派生。
	Referer           string
	RequestHeaderSize int64
	UserAgent         string
	RemoteIP          string
	// RequestID 是请求级关联 ID（WithRequestID 中间件生成/透传）。
	RequestID string
}

func ipFromHostPort(hp string) string {
	h, _, err := net.SplitHostPort(hp)
	if err != nil {
		return ""
	}
	if len(h) > 0 && h[0] == '[' {
		return h[1 : len(h)-1]
	}
	return h
}

type readCounterCloser struct {
	r   io.ReadCloser
	n   int64
	err error
}

func (rcc *readCounterCloser) Read(p []byte) (n int, err error) {
	if rcc.err != nil {
		return 0, rcc.err
	}
	n, rcc.err = rcc.r.Read(p)
	rcc.n += int64(n)
	return n, rcc.err
}

func (rcc *readCounterCloser) Close() error {
	rcc.err = errors.New("read from closed reader")
	return rcc.r.Close()
}

type writeCounter int64

func (wc *writeCounter) Write(p []byte) (n int, err error) {
	*wc += writeCounter(len(p))
	return len(p), nil
}

func headerSize(h http.Header) int64 {
	var wc writeCounter
	_ = h.Write(&wc)
	return int64(wc) + 2 // for CRLF
}

// responseStats 包装 ResponseWriter 统计状态码与响应大小，并透传
// Flusher/Hijacker 接口能力（websocket/流式响应依赖）。
type responseStats struct {
	w        http.ResponseWriter
	hsize    int64
	wc       writeCounter
	code     int
	hijacked bool
}

func (r *responseStats) Header() http.Header {
	return r.w.Header()
}

func (r *responseStats) WriteHeader(statusCode int) {
	if r.code != 0 {
		return
	}
	r.hsize = headerSize(r.w.Header())
	r.w.WriteHeader(statusCode)
	r.code = statusCode
}

func (r *responseStats) Write(p []byte) (n int, err error) {
	if r.code == 0 {
		r.WriteHeader(http.StatusOK)
	}
	n, err = r.w.Write(p)
	_, _ = r.wc.Write(p[:n])
	return
}

func (r *responseStats) size() (hdr, body int64) {
	if r.code == 0 {
		return headerSize(r.w.Header()), 0
	}
	// Use the header size from the time WriteHeader was called.
	// The Header map can be mutated after the call to add HTTP Trailers,
	// which we don't want to count.
	return r.hsize, int64(r.wc)
}

func (r *responseStats) Hijack() (_ net.Conn, _ *bufio.ReadWriter, err error) {
	defer func() {
		if err == nil {
			r.hijacked = true
		}
	}()
	if hj, ok := r.w.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, errors.New("underlying ResponseWriter does not support hijacking")
}

func (r *responseStats) Flush() {
	if fl, ok := r.w.(http.Flusher); ok {
		fl.Flush()
	}
}

// RequestLogger writes log entries in the Stackdriver forward JSON format.
// The record's fields are suitable for consumption by Stackdriver Logging.
// slog.Logger is concurrency-safe, so no additional locking is required.
type RequestLogger struct {
	logger *slog.Logger
}

// NewRequestLogger returns a new logger.
// onErr 仅为保持既有签名兼容而保留（SC-09）：底层 slog 写入不返回
// 错误，历史上 log() 恒返 nil 使该回调成为死代码，其调用链已删除，
// 传入的函数恒不被调用。
func NewRequestLogger(logger *slog.Logger, onErr func(error)) *RequestLogger {
	_ = onErr
	return &RequestLogger{logger: logger}
}

// Log writes a record to its writer.  Multiple concurrent calls will
// produce sequential writes to its writer.
func (l *RequestLogger) Log(ent *Entry) {
	l.log(ent)
}

func (l *RequestLogger) log(ent *Entry) {
	// r represents the fluent-plugin-google-cloud format
	// See https://github.com/GoogleCloudPlatform/fluent-plugin-google-cloud/blob/f93046d92f7722db2794a042c3f2dde5df91a90b/lib/fluent/plugin/out_google_cloud.rb#L145
	// to check json tags
	var r struct {
		HTTPRequest struct {
			RequestMethod string `json:"requestMethod"`
			RequestURL    string `json:"requestUrl"`
			RequestSize   int64  `json:"requestSize,string"`
			Status        int    `json:"status"`
			ResponseSize  int64  `json:"responseSize,string"`
			UserAgent     string `json:"userAgent"`
			RemoteIP      string `json:"remoteIp"`
			Referer       string `json:"referer"`
			Latency       string `json:"latency"`
		} `json:"httpRequest"`
		Timestamp struct {
			Seconds int64 `json:"seconds"`
			Nanos   int   `json:"nanos"`
		} `json:"timestamp"`
		TraceID   string `json:"logging.googleapis.com/trace"`
		SpanID    string `json:"logging.googleapis.com/spanId"`
		RequestID string `json:"requestId"`
	}
	r.HTTPRequest.RequestMethod = ent.Request.Method
	r.HTTPRequest.RequestURL = ent.Request.URL.String()
	// 请求/响应尺寸取 header 与 body 之和（沿用 gocloud.dev/server/requestlog
	// 的既有算法；LogEntry 规范未精确定义该字段，此处保持与其一致以便迁移）。
	r.HTTPRequest.RequestSize = ent.RequestHeaderSize + ent.RequestBodySize
	r.HTTPRequest.Status = ent.Status
	r.HTTPRequest.ResponseSize = ent.ResponseHeaderSize + ent.ResponseBodySize
	r.HTTPRequest.UserAgent = ent.UserAgent
	r.HTTPRequest.RemoteIP = ent.RemoteIP
	r.HTTPRequest.Referer = ent.Referer
	r.HTTPRequest.Latency = string(appendLatency(nil, ent.Latency))

	t := ent.ReceivedTime.Add(ent.Latency)
	r.Timestamp.Seconds = t.Unix()
	r.Timestamp.Nanos = t.Nanosecond()
	r.TraceID = ent.TraceID.String()
	r.SpanID = ent.SpanID.String()
	r.RequestID = ent.RequestID
	l.logger.Debug("requestlog", "request", r)
}

func appendLatency(b []byte, d time.Duration) []byte {
	// Parses format understood by google-fluentd (which is looser than the documented LogEntry format).
	// See the comment at https://github.com/GoogleCloudPlatform/fluent-plugin-google-cloud/blob/e2f60cdd1d97d79ffe4e91bdbf6bd84837f27fa5/lib/fluent/plugin/out_google_cloud.rb#L1539
	b = strconv.AppendFloat(b, d.Seconds(), 'f', 9, 64)
	b = append(b, 's')
	return b
}

package http

import (
	"log/slog"
	"strconv"
	"time"

	"gocloud.dev/server/requestlog"
)

// RequestLogger writes log entries in the Stackdriver forward JSON format.
// The record's fields are suitable for consumption by Stackdriver Logging.
// slog.Logger is concurrency-safe, so no additional locking is required.
type RequestLogger struct {
	onErr  func(error)
	logger *slog.Logger
}

// NewRequestLogger returns a new logger.
// A nil onErr is treated the same as func(error) {}.
func NewRequestLogger(logger *slog.Logger, onErr func(error)) *RequestLogger {
	return &RequestLogger{
		logger: logger,
		onErr:  onErr,
	}
}

// Log writes a record to its writer.  Multiple concurrent calls will
// produce sequential writes to its writer.
func (l *RequestLogger) Log(ent *requestlog.Entry) {
	if err := l.log(ent); err != nil && l.onErr != nil {
		l.onErr(err)
	}
}

func (l *RequestLogger) log(ent *requestlog.Entry) error {
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
		TraceID string `json:"logging.googleapis.com/trace"`
		SpanID  string `json:"logging.googleapis.com/spanId"`
	}
	r.HTTPRequest.RequestMethod = ent.Request.Method
	r.HTTPRequest.RequestURL = ent.Request.URL.String()
	// TODO(light): determine whether this is the formula LogEntry expects.
	r.HTTPRequest.RequestSize = ent.RequestHeaderSize + ent.RequestBodySize
	r.HTTPRequest.Status = ent.Status
	// TODO(light): determine whether this is the formula LogEntry expects.
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
	l.logger.Debug("requestlog", "request", r)
	return nil
}

func appendLatency(b []byte, d time.Duration) []byte {
	// Parses format understood by google-fluentd (which is looser than the documented LogEntry format).
	// See the comment at https://github.com/GoogleCloudPlatform/fluent-plugin-google-cloud/blob/e2f60cdd1d97e79ffe4e91bdbf6bd84837f27fa5/lib/fluent/plugin/out_google_cloud.rb#L1539
	b = strconv.AppendFloat(b, d.Seconds(), 'f', 9, 64)
	b = append(b, 's')
	return b
}

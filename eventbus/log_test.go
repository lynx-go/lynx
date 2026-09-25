package eventbus

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// 钉死实现：handler 持有的是 *Event[T]，必须满足 slog.LogValuer。
var _ slog.LogValuer = (*Event[map[string]string])(nil)

// 事件在 TextHandler 下输出为独立键值对（event.id=... 等），而不是整段
// fmt.Sprintf("%+v") 的 Go 语法 blob。
func TestEventLogValueText(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	ev := &Event[map[string]string]{
		ID:      "id-1",
		Topic:   "order.created",
		Key:     "order-42",
		Headers: map[string]string{"request_id": "req-1"},
		Payload: map[string]string{"order_id": "o-1"},
		Time:    time.Date(2026, 9, 24, 10, 30, 0, 0, time.UTC),
	}

	logger.Info("recv order created", "handler", "h1", "event", ev)

	got := buf.String()
	for _, want := range []string{
		"handler=h1",
		"event.id=id-1",
		"event.topic=order.created",
		"event.key=order-42",
		"event.headers=map[request_id:req-1]",
		"event.payload=map[order_id:o-1]",
		"event.time=2026-09-24T10:30:00.000Z",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("log output missing %q:\n%s", want, got)
		}
	}
}

// 事件在 JSONHandler 下输出为嵌套对象（LogValuer 返回 group），字段可被
// 日志系统结构化解析，而不是被转义的字符串。
func TestEventLogValueJSON(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	ev := &Event[map[string]string]{
		ID:      "id-1",
		Topic:   "order.created",
		Key:     "order-42",
		Headers: map[string]string{"request_id": "req-1"},
		Payload: map[string]string{"order_id": "o-1"},
		Time:    time.Date(2026, 9, 24, 10, 30, 0, 0, time.UTC),
	}

	logger.Info("recv order created", "handler", "h1", "event", ev)

	var rec struct {
		Msg   string `json:"msg"`
		Event struct {
			ID      string            `json:"id"`
			Topic   string            `json:"topic"`
			Key     string            `json:"key"`
			Headers map[string]string `json:"headers"`
			Payload map[string]string `json:"payload"`
			Time    time.Time         `json:"time"`
		} `json:"event"`
	}
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("event must render as a nested JSON object: %v\n%s", err, buf.String())
	}
	if rec.Msg != "recv order created" {
		t.Errorf("msg = %q", rec.Msg)
	}
	if rec.Event.ID != "id-1" || rec.Event.Topic != "order.created" || rec.Event.Key != "order-42" {
		t.Errorf("envelope = %+v", rec.Event)
	}
	if rec.Event.Headers["request_id"] != "req-1" {
		t.Errorf("headers = %v", rec.Event.Headers)
	}
	if rec.Event.Payload["order_id"] != "o-1" {
		t.Errorf("payload = %v", rec.Event.Payload)
	}
	if want := time.Date(2026, 9, 24, 10, 30, 0, 0, time.UTC); !rec.Event.Time.Equal(want) {
		t.Errorf("time = %v, want %v", rec.Event.Time, want)
	}
}

// nil 指针不得 panic：slog 在 Resolve 时会对 nil 接收者调用 LogValue。
func TestEventLogValueNil(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	var ev *Event[map[string]string]

	logger.Info("nil event", "event", ev)

	if got := buf.String(); !strings.Contains(got, "event=<nil>") {
		t.Errorf("nil event must render as <nil> without panic, got:\n%s", got)
	}
}

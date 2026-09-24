package lynx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// capturedRecord 是 captureHandler 收集的单条日志快照。
type capturedRecord struct {
	msg   string
	attrs map[string]string
}

// captureHandler 是测试用 slog.Handler：按序收集消息与属性（含 With 预置
// 的属性），供断言 OrderedServices 组内日志。WithGroup 未参与断言场景，
// 原样返回即可。
type captureHandler struct {
	mu      *sync.Mutex
	records *[]capturedRecord
	attrs   []slog.Attr
}

func newCaptureHandler() *captureHandler {
	return &captureHandler{mu: &sync.Mutex{}, records: &[]capturedRecord{}}
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	attrs := make(map[string]string, len(h.attrs)+r.NumAttrs())
	for _, a := range h.attrs {
		attrs[a.Key] = a.Value.String()
	}
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.String()
		return true
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	*h.records = append(*h.records, capturedRecord{msg: r.Message, attrs: attrs})
	return nil
}

func (h *captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	merged := make([]slog.Attr, 0, len(h.attrs)+len(attrs))
	merged = append(merged, h.attrs...)
	merged = append(merged, attrs...)
	return &captureHandler{mu: h.mu, records: h.records, attrs: merged}
}

func (h *captureHandler) WithGroup(string) slog.Handler { return h }

// entries 返回 "msg service=<service> group=<group>" 形式的有序快照。
func (h *captureHandler) entries() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(*h.records))
	for _, r := range *h.records {
		out = append(out, fmt.Sprintf("%s service=%s group=%s", r.msg, r.attrs["service"], r.attrs["group"]))
	}
	return out
}

// has 报告是否存在 msg 与 service 属性均匹配的记录。
func (h *captureHandler) has(msg, service string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range *h.records {
		if r.msg == msg && r.attrs["service"] == service {
			return true
		}
	}
	return false
}

// TestOrderedServicesLogsChildLifecycle：组内每个子服务的生命周期调用按序
// 记录 Info 日志（Init 前后各一条、Start 前一条、Stop 前一条），顺序与
// 调用顺序一致（Init/Start 正序、Stop 逆序）；属性携带子服务名（service）
// 与组名（group）。
func TestOrderedServicesLogsChildLifecycle(t *testing.T) {
	h := newCaptureHandler()
	app, err := newLynx(NewOptions(WithIsolated()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	app.SetLogger(slog.New(h))

	log := &orderLog{}
	g := OrderedServices("infra",
		&seqProbe{name: "a", log: log},
		&seqProbe{name: "b", log: log},
	)
	if err := g.Init(app); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- g.Start(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !containsStr(log.all(), "start:b") {
		time.Sleep(5 * time.Millisecond)
	}
	if err := g.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return")
	}

	want := []string{
		"initializing service service=a group=infra",
		"initialized service service=a group=infra",
		"initializing service service=b group=infra",
		"initialized service service=b group=infra",
		"starting service service=a group=infra",
		"starting service service=b group=infra",
		"stopping service service=b group=infra",
		"stopping service service=a group=infra",
	}
	if got := h.entries(); !equalStrs(got, want) {
		t.Fatalf("log entries = %v, want %v", got, want)
	}
}

// TestOrderedServicesInitFailureLogsCleanupStop：Init 中途失败触发的前序
// 子服务清理停止同样计入 Stop 日志（组未登记、App 不会补记）。
func TestOrderedServicesInitFailureLogsCleanupStop(t *testing.T) {
	h := newCaptureHandler()
	app, err := newLynx(NewOptions(WithIsolated()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	app.SetLogger(slog.New(h))

	g := OrderedServices("infra",
		&seqProbe{name: "a", log: &orderLog{}},
		&seqProbe{name: "b", log: &orderLog{}, initErr: errors.New("init-b")},
	)
	if err := g.Init(app); err == nil {
		t.Fatal("Init() = nil, want child init error")
	}
	if !h.has("initializing service", "a") || !h.has("stopping service", "a") {
		t.Fatalf("missing cleanup logs for a: %v", h.entries())
	}
}

// TestOrderedServicesLogsFallbackWithoutAppContext：Init 未拿到 AppContext
// 时组内日志回退 slog.Default()，不丢日志、不 panic。
func TestOrderedServicesLogsFallbackWithoutAppContext(t *testing.T) {
	restore := saveGlobals()
	defer restore()
	h := newCaptureHandler()
	slog.SetDefault(slog.New(h))

	g := OrderedServices("infra", &seqProbe{name: "a", log: &orderLog{}})
	if err := g.Init(nil); err != nil {
		t.Fatal(err)
	}
	if err := g.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}

	for _, msg := range []string{"initializing service", "initialized service", "stopping service"} {
		if !h.has(msg, "a") {
			t.Errorf("default logger missing %q for service a: %v", msg, h.entries())
		}
	}
}

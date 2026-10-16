package debug

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lynx-go/lynx"
)

// controlledFakeAppContext 在 fakeAppContext 之上实现日志级别控制接口。
type controlledFakeAppContext struct {
	fakeAppContext
	level    slog.Level
	adjustOK bool
}

func (f *controlledFakeAppContext) SetLogLevel(level slog.Level) bool {
	if !f.adjustOK {
		return false
	}
	f.level = level
	return true
}

func (f *controlledFakeAppContext) LogLevel() slog.Level { return f.level }

func TestVersionEndpoint(t *testing.T) {
	t.Run("renders placeholders when nothing injected", func(t *testing.T) {
		s := NewService()
		if err := s.Init(&fakeAppContext{logger: discardLogger()}); err != nil {
			t.Fatalf("Init() error = %v", err)
		}
		rec := httptest.NewRecorder()
		s.newMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/version", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("/version status = %d, want 200", rec.Code)
		}
		var v struct {
			Version string `json:"version"`
			Commit  string `json:"commit"`
			Date    string `json:"date"`
			Go      string `json:"go"`
			Service struct {
				Name string `json:"name"`
			} `json:"service"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
			t.Fatalf("unmarshal /version: %v", err)
		}
		// 测试进程未注入：空构建变量渲染 "dev"/"unknown"/"unknown" 占位，
		// 未注入应用的端点输出形状保持稳定。
		if v.Version != "dev" || v.Commit != "unknown" || v.Date != "unknown" {
			t.Errorf("/version build fields = %+v, want dev/unknown/unknown placeholders", v)
		}
		if !strings.HasPrefix(v.Go, "go1.") {
			t.Errorf("/version go = %q, want go1.x", v.Go)
		}
	})

	// 注入反射：根包构建变量（ldflags 唯一目标）非空时端点原样回显，
	// 不再叠加占位渲染。白盒改写须存原值恢复。
	t.Run("echoes injected build vars", func(t *testing.T) {
		oldV, oldC, oldD := lynx.BuildVersion, lynx.BuildCommit, lynx.BuildDate
		defer func() { lynx.BuildVersion, lynx.BuildCommit, lynx.BuildDate = oldV, oldC, oldD }()
		lynx.BuildVersion, lynx.BuildCommit, lynx.BuildDate = "v-test", "abc1234", "2026-10-17T00:00:00Z"

		s := NewService()
		if err := s.Init(&fakeAppContext{logger: discardLogger()}); err != nil {
			t.Fatalf("Init() error = %v", err)
		}
		rec := httptest.NewRecorder()
		s.newMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/version", nil))
		var v struct {
			Version string `json:"version"`
			Commit  string `json:"commit"`
			Date    string `json:"date"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
			t.Fatalf("unmarshal /version: %v", err)
		}
		if v.Version != "v-test" || v.Commit != "abc1234" || v.Date != "2026-10-17T00:00:00Z" {
			t.Errorf("/version build fields = %+v, want injected vars echoed", v)
		}
	})
}

func TestLogLevelEndpointWithoutControl(t *testing.T) {
	s := NewService()
	if err := s.Init(&fakeAppContext{logger: discardLogger()}); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	mux := s.newMux()

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/loglevel", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /loglevel status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"INFO"`) {
		t.Errorf("GET /loglevel body = %q, want default INFO", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/loglevel?level=debug", nil))
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("POST /loglevel status = %d, want 501 without control", rec.Code)
	}
}

func TestLogLevelEndpointWithControl(t *testing.T) {
	fake := &controlledFakeAppContext{level: slog.LevelInfo, adjustOK: true}
	fake.logger = discardLogger()
	s := NewService()
	if err := s.Init(fake); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	mux := s.newMux()

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/loglevel?level=debug", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /loglevel status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"DEBUG"`) {
		t.Errorf("POST /loglevel body = %q, want DEBUG", rec.Body.String())
	}
	if fake.level != slog.LevelDebug {
		t.Errorf("controller level = %v, want Debug", fake.level)
	}

	// JSON body 路径。
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/loglevel",
		strings.NewReader(`{"level":"warn"}`)))
	if rec.Code != http.StatusOK || fake.level != slog.LevelWarn {
		t.Errorf("POST body path: status %d level %v, want 200/Warn", rec.Code, fake.level)
	}

	// 非法级别。
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/loglevel?level=nope", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("POST invalid level status = %d, want 400", rec.Code)
	}

	// 定制 logger（SetLogLevel 拒绝）→ 409。
	fake.adjustOK = false
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/loglevel?level=debug", nil))
	if rec.Code != http.StatusConflict {
		t.Errorf("POST with customized logger status = %d, want 409", rec.Code)
	}
}

var _ lynx.AppContext = (*fakeAppContext)(nil)

package lynx

import (
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/lynx-go/lynx/eventbus"
	"github.com/spf13/viper"
)

// loggerprovider_test.go 验证 WithLoggerProvider 的装配契约：provider 在
// 配置装配与元数据解析之后、总线（及配套服务）构造之前被调用；产物落位
// app.logger 与 slog.SetDefault（WithIsolated 除外）；级别默认路径
// （applyLogLevel）被跳过；错误与 nil 产物使构造快失败。

// TestWithLoggerProviderResolvesBeforeBus：provider 拿到装配好的配置与
// meta；配套服务的 Init 日志（"initializing service"）经 provider 产物
// 输出——证明 logger 在总线块之前就位（装配期日志不再落到框架默认格式）。
func TestWithLoggerProviderResolvesBeforeBus(t *testing.T) {
	restore := saveGlobals()
	defer restore()

	h := newCaptureHandler()
	logger := slog.New(h)
	v := viper.New()
	v.Set("logging.level", "info")
	var gotMeta Metadata
	svc := &providerSvc{}
	app, err := newLynx(NewOptions(
		WithConfig(NewViperConfig(v)),
		WithName("prov-app"),
		WithLoggerProvider(func(ctx AppContext) (*slog.Logger, error) {
			meta := Meta(ctx.Context())
			gotMeta = meta
			return logger, nil
		}),
		WithBusProvider(func(cfg Config) (eventbus.Bus, []Service, error) {
			return &readyGateBus{}, []Service{svc}, nil
		}),
	))
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}
	defer app.Close()

	if gotMeta.Name != "prov-app" {
		t.Errorf("provider meta.Name = %q, want prov-app", gotMeta.Name)
	}
	l, ok := app.(*lynx)
	if !ok {
		t.Fatalf("newLynx() returned %T, want *lynx", app)
	}
	if l.Logger() != logger {
		t.Error("app.Logger() should be the provider-built logger")
	}
	if slog.Default() != logger {
		t.Error("provider logger should be installed as slog default (non-isolated)")
	}
	if !h.has("initializing service", "provider-svc") {
		t.Error("companion service init log should flow through the provider logger")
	}
	if ok := l.SetLogLevel(slog.LevelDebug); ok {
		t.Error("SetLogLevel() = true with provider logger, want false (customized)")
	}
}

// TestWithLoggerProviderSkipsApplyLogLevel：配置了 logging.level 且存在
// provider 时，框架不重建 TextHandler（provider 产物不被覆盖）。
func TestWithLoggerProviderSkipsApplyLogLevel(t *testing.T) {
	restore := saveGlobals()
	defer restore()

	logger := slog.New(slog.NewTextHandler(nil, nil))
	v := viper.New()
	v.Set("logging.level", "debug")
	app, err := newLynx(NewOptions(
		WithConfig(NewViperConfig(v)),
		WithIsolated(),
		WithLoggerProvider(func(AppContext) (*slog.Logger, error) { return logger, nil }),
	))
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}
	defer app.Close()

	l := app.(*lynx)
	if l.Logger() != logger {
		t.Error("app.Logger() should stay the provider logger (applyLogLevel skipped)")
	}
	if slog.Default() == logger {
		t.Error("isolated app should not touch slog default")
	}
	if l.logLevelVar != nil {
		t.Error("applyLogLevel should not install logLevelVar on the provider path")
	}
}

// TestWithLoggerProviderFailureFailsConstruction：provider 出错或返回
// nil logger 时构造失败，错误信息指向 provider。
func TestWithLoggerProviderFailureFailsConstruction(t *testing.T) {
	sentinel := errors.New("no logger backend")
	_, err := newLynx(NewOptions(
		WithIsolated(),
		WithLoggerProvider(func(AppContext) (*slog.Logger, error) { return nil, sentinel }),
	))
	if !errors.Is(err, sentinel) || !strings.Contains(err.Error(), "logger provider failed") {
		t.Fatalf("newLynx() error = %v, want wrapped logger provider failure", err)
	}

	_, err = newLynx(NewOptions(
		WithIsolated(),
		WithLoggerProvider(func(AppContext) (*slog.Logger, error) { return nil, nil }),
	))
	if err == nil || !strings.Contains(err.Error(), "nil logger") {
		t.Fatalf("newLynx() error = %v, want nil logger failure", err)
	}
}

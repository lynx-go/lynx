package lynx

import (
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/pflag"
)

func TestOptionsValidate(t *testing.T) {
	tests := []struct {
		name    string
		options Options
		wantErr error
	}{
		{
			name:    "valid empty name",
			options: Options{},
			wantErr: nil,
		},
		{
			name:    "name at max length",
			options: Options{Name: strings.Repeat("a", 63)},
			wantErr: nil,
		},
		{
			name:    "name too long",
			options: Options{Name: strings.Repeat("a", 64)},
			wantErr: ErrNameTooLong,
		},
		{
			name:    "zero shutdown timeout is allowed",
			options: Options{ShutdownTimeout: 0},
			wantErr: nil,
		},
		{
			name:    "shutdown timeout below minimum",
			options: Options{ShutdownTimeout: MinTimeout - time.Millisecond},
			wantErr: ErrShutdownTimeoutTooSmall,
		},
		{
			name:    "shutdown timeout at minimum",
			options: Options{ShutdownTimeout: MinTimeout},
			wantErr: nil,
		},
		{
			name:    "shutdown timeout at maximum",
			options: Options{ShutdownTimeout: MaxTimeout},
			wantErr: nil,
		},
		{
			name:    "shutdown timeout above maximum",
			options: Options{ShutdownTimeout: MaxTimeout + time.Millisecond},
			wantErr: ErrShutdownTimeoutTooLarge,
		},
		{
			name:    "drain timeout zero is allowed",
			options: Options{DrainTimeout: 0},
			wantErr: nil,
		},
		{
			name:    "drain timeout small positive is allowed",
			options: Options{DrainTimeout: time.Millisecond},
			wantErr: nil,
		},
		{
			name:    "drain timeout negative",
			options: Options{DrainTimeout: -time.Millisecond},
			wantErr: ErrDrainTimeoutInvalid,
		},
		{
			name:    "bus ready timeout zero is allowed",
			options: Options{BusReadyTimeout: 0},
			wantErr: nil,
		},
		{
			name:    "bus ready timeout negative",
			options: Options{BusReadyTimeout: -time.Millisecond},
			wantErr: ErrBusReadyTimeoutInvalid,
		},
		{
			name:    "cleanup timeout zero is allowed",
			options: Options{CleanupTimeout: 0},
			wantErr: nil,
		},
		{
			name:    "cleanup timeout negative",
			options: Options{CleanupTimeout: -time.Millisecond},
			wantErr: ErrCleanupTimeoutInvalid,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.options.Validate()
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestOptionsEnsureDefaults(t *testing.T) {
	o := &Options{}
	o.EnsureDefaults()

	hostname, _ := os.Hostname()
	if o.ID != hostname {
		t.Errorf("ID = %q, want hostname %q", o.ID, hostname)
	}
	if o.Name != DefaultName {
		t.Errorf("Name = %q, want %q", o.Name, DefaultName)
	}
	if o.ShutdownTimeout != DefaultShutdownTimeout {
		t.Errorf("ShutdownTimeout = %v, want %v", o.ShutdownTimeout, DefaultShutdownTimeout)
	}
	// DrainTimeout 无默认值：0 = 不启用排水（整段禁用的回归红线）。
	if o.DrainTimeout != 0 {
		t.Errorf("DrainTimeout = %v, want 0 (no default)", o.DrainTimeout)
	}
	// BusReadyTimeout 默认 10s（CORE-02：取代 newLynx 硬编码的 1 秒预算）。
	if o.BusReadyTimeout != DefaultBusReadyTimeout {
		t.Errorf("BusReadyTimeout = %v, want %v", o.BusReadyTimeout, DefaultBusReadyTimeout)
	}
	// CleanupTimeout 默认 10s（OnPostStop 收尾钩子总预算）。
	if o.CleanupTimeout != DefaultCleanupTimeout {
		t.Errorf("CleanupTimeout = %v, want %v", o.CleanupTimeout, DefaultCleanupTimeout)
	}
	if len(o.ExitSignals) == 0 {
		t.Error("ExitSignals should not be empty")
	}
}

func TestOptionsEnsureDefaultsPreservesSetValues(t *testing.T) {
	o := &Options{
		ID:              "my-id",
		Name:            "my-name",
		ShutdownTimeout: 2 * time.Second,
		ExitSignals:     []os.Signal{syscall.SIGINT},
	}
	o.EnsureDefaults()

	if o.ID != "my-id" {
		t.Errorf("ID = %q, want %q", o.ID, "my-id")
	}
	if o.Name != "my-name" {
		t.Errorf("Name = %q, want %q", o.Name, "my-name")
	}
	if o.ShutdownTimeout != 2*time.Second {
		t.Errorf("ShutdownTimeout = %v, want %v", o.ShutdownTimeout, 2*time.Second)
	}
	if len(o.ExitSignals) != 1 {
		t.Errorf("ExitSignals = %v, want 1 entry", o.ExitSignals)
	}
}

func TestNewOptions(t *testing.T) {
	o := NewOptions()
	if o.ID == "" {
		t.Error("ID should default to hostname")
	}
	if o.ShutdownTimeout != DefaultShutdownTimeout {
		t.Errorf("ShutdownTimeout = %v, want %v", o.ShutdownTimeout, DefaultShutdownTimeout)
	}
	if len(o.ExitSignals) == 0 {
		t.Error("ExitSignals should not be empty")
	}
}

func TestOptionFuncs(t *testing.T) {
	o := NewOptions(
		WithID("id-1"),
		WithName("svc"),
		WithVersion("v1.2.3"),
		WithShutdownTimeout(3*time.Second),
		WithExitSignals(syscall.SIGTERM),
		WithDrainTimeout(2*time.Second),
		WithBusReadyTimeout(30*time.Second),
		WithCleanupTimeout(15*time.Second),
	)
	if o.ID != "id-1" {
		t.Errorf("ID = %q, want %q", o.ID, "id-1")
	}
	if o.Name != "svc" {
		t.Errorf("Name = %q, want %q", o.Name, "svc")
	}
	if o.Version != "v1.2.3" {
		t.Errorf("Version = %q, want %q", o.Version, "v1.2.3")
	}
	if o.ShutdownTimeout != 3*time.Second {
		t.Errorf("ShutdownTimeout = %v, want %v", o.ShutdownTimeout, 3*time.Second)
	}
	if o.DrainTimeout != 2*time.Second {
		t.Errorf("DrainTimeout = %v, want %v", o.DrainTimeout, 2*time.Second)
	}
	if o.BusReadyTimeout != 30*time.Second {
		t.Errorf("BusReadyTimeout = %v, want %v", o.BusReadyTimeout, 30*time.Second)
	}
	if o.CleanupTimeout != 15*time.Second {
		t.Errorf("CleanupTimeout = %v, want %v", o.CleanupTimeout, 15*time.Second)
	}
	if len(o.ExitSignals) != 1 {
		t.Errorf("ExitSignals = %v, want 1 entry", o.ExitSignals)
	}
}

func TestWithBindFlagsAndBindConfig(t *testing.T) {
	flagsBound := false
	bindCalled := false
	o := NewOptions(
		WithBindFlagsFunc(func(f *pflag.FlagSet) { flagsBound = true }),
		WithBindConfigFunc(func(f *pflag.FlagSet, c ConfigSource) error {
			bindCalled = true
			return nil
		}),
	)
	if o.BindFlagsFunc == nil || o.BindConfigFunc == nil {
		t.Fatal("BindFlagsFunc and BindConfigFunc should be set")
	}
	o.BindFlagsFunc(nil)
	if !flagsBound {
		t.Error("BindFlagsFunc was not the provided function")
	}
	if err := o.BindConfigFunc(nil, nil); err != nil {
		t.Fatalf("BindConfigFunc returned error: %v", err)
	}
	if !bindCalled {
		t.Error("BindConfigFunc was not the provided function")
	}
}

// TestDefaultConfigFlagsEnabled 验证默认 flags 默认开启：未显式设置
// BindFlagsFunc/BindConfigFunc 时自动使用框架内置实现。
func TestDefaultConfigFlagsEnabled(t *testing.T) {
	o := &Options{}
	o.EnsureDefaults()
	if o.BindFlagsFunc == nil {
		t.Error("BindFlagsFunc should default to DefaultBindFlagsFunc")
	}
	if o.BindConfigFunc == nil {
		t.Error("BindConfigFunc should default to DefaultBindConfigFunc")
	}

	// NewOptions 路径同样生效。
	o2 := NewOptions()
	if o2.BindFlagsFunc == nil || o2.BindConfigFunc == nil {
		t.Error("NewOptions should enable default config flags")
	}
	// StopTimeout 与 Name 双轨默认值已消除。
	if o2.Name != DefaultName {
		t.Errorf("Name = %q, want %q", o2.Name, DefaultName)
	}
	if o2.StopTimeout != DefaultStopTimeout {
		t.Errorf("StopTimeout = %v, want %v", o2.StopTimeout, DefaultStopTimeout)
	}
}

// TestWithDisableConfigFlags 验证 opt-out：显式关闭默认 flags 后
// EnsureDefaults 不再启用它们（含 newLynx 的二次 EnsureDefaults 路径）。
func TestWithDisableConfigFlags(t *testing.T) {
	o := NewOptions(WithDisableConfigFlags())
	if o.BindFlagsFunc != nil || o.BindConfigFunc != nil {
		t.Fatal("WithDisableConfigFlags should clear BindFlagsFunc and BindConfigFunc")
	}
	o.EnsureDefaults()
	if o.BindFlagsFunc != nil || o.BindConfigFunc != nil {
		t.Fatal("EnsureDefaults must not re-enable disabled config flags")
	}
}

func TestOptionsString(t *testing.T) {
	o := NewOptions(WithName("svc"), WithVersion("v1"))
	s := o.String()
	if !strings.Contains(s, `"name":"svc"`) {
		t.Errorf("String() = %q, want it to contain name", s)
	}
	if !strings.Contains(s, `"version":"v1"`) {
		t.Errorf("String() = %q, want it to contain version", s)
	}
	// json tag 与 Options.String() 序列化一致（CORE-02）。
	if !strings.Contains(s, `"bus_ready_timeout":`) {
		t.Errorf("String() = %q, want it to contain bus_ready_timeout", s)
	}
}

// recordingConfigSource 记录写入型绑定调用，验证 WithConfigFile 的绑定
// 行为（读侧方法不参与，嵌入的 Config 仅用于满足接口）。
type recordingConfigSource struct {
	Config
	files  []string
	search []string
}

func (r *recordingConfigSource) Set(path string, value any)               {}
func (r *recordingConfigSource) SetFile(path string)                      { r.files = append(r.files, path) }
func (r *recordingConfigSource) AddSearchPath(dir string)                 { r.search = append(r.search, dir) }
func (r *recordingConfigSource) SetFileFormat(format string)              {}
func (r *recordingConfigSource) SetEnvPrefix(prefix string)               {}
func (r *recordingConfigSource) SetEnvKeyReplacer(repl *strings.Replacer) {}
func (r *recordingConfigSource) AutomaticEnv()                            {}
func (r *recordingConfigSource) BindEnv(path string, env ...string) error { return nil }

// TestWithConfigFileBindsPath：路径直接绑定为配置文件，默认 flags 关闭
// 且 EnsureDefaults 二次调用（newLynx 路径）不得回填。
func TestWithConfigFileBindsPath(t *testing.T) {
	o := NewOptions(WithConfigFile("prod.yaml"))
	if o.BindFlagsFunc != nil {
		t.Fatal("WithConfigFile should disable default flags (BindFlagsFunc nil)")
	}
	if o.BindConfigFunc == nil {
		t.Fatal("WithConfigFile should install a BindConfigFunc")
	}
	o.EnsureDefaults()
	if o.BindFlagsFunc != nil {
		t.Fatal("EnsureDefaults must not re-enable flags disabled by WithConfigFile")
	}
	src := &recordingConfigSource{}
	if err := o.BindConfigFunc(nil, src); err != nil {
		t.Fatalf("BindConfigFunc returned error: %v", err)
	}
	if len(src.files) != 1 || src.files[0] != "prod.yaml" {
		t.Errorf("SetFile calls = %v, want [prod.yaml]", src.files)
	}
	if len(src.search) != 0 {
		t.Errorf("AddSearchPath calls = %v, want none with explicit path", src.search)
	}
}

// TestWithConfigFileEmptyPathFallsBackToWorkDir：空路径回退搜索工作目录
// （与 DefaultBindConfigFunc 的回退一致）。
func TestWithConfigFileEmptyPathFallsBackToWorkDir(t *testing.T) {
	o := NewOptions(WithConfigFile(""))
	src := &recordingConfigSource{}
	if err := o.BindConfigFunc(nil, src); err != nil {
		t.Fatalf("BindConfigFunc returned error: %v", err)
	}
	if len(src.files) != 0 {
		t.Errorf("SetFile calls = %v, want none with empty path", src.files)
	}
	if len(src.search) != 1 || src.search[0] != "." {
		t.Errorf("AddSearchPath calls = %v, want [.]", src.search)
	}
}

// TestWithConfigFileEquivalence：单一选项 ≡ 手工正确顺序（先 Disable
// 后 Bind）的两选项组合。顺序敏感：两者写反会静默丢失自定义绑定。
func TestWithConfigFileEquivalence(t *testing.T) {
	fused := NewOptions(WithConfigFile("cfg.yaml"))
	manual := NewOptions(
		WithDisableConfigFlags(),
		WithBindConfigFunc(func(_ *pflag.FlagSet, c ConfigSource) error {
			c.SetFile("cfg.yaml")
			return nil
		}),
	)
	if fused.BindFlagsFunc != nil || manual.BindFlagsFunc != nil {
		t.Fatal("both forms should have BindFlagsFunc disabled")
	}
	fusedSrc, manualSrc := &recordingConfigSource{}, &recordingConfigSource{}
	if err := fused.BindConfigFunc(nil, fusedSrc); err != nil {
		t.Fatalf("fused BindConfigFunc error: %v", err)
	}
	if err := manual.BindConfigFunc(nil, manualSrc); err != nil {
		t.Fatalf("manual BindConfigFunc error: %v", err)
	}
	if len(fusedSrc.files) != 1 || fusedSrc.files[0] != "cfg.yaml" {
		t.Errorf("fused SetFile calls = %v, want [cfg.yaml]", fusedSrc.files)
	}
	if len(manualSrc.files) != 1 || manualSrc.files[0] != "cfg.yaml" {
		t.Errorf("manual SetFile calls = %v, want [cfg.yaml]", manualSrc.files)
	}
}

// TestWithConfigFileLastWins：与其他选项遵循后到者胜——用户随后
// 设置的 BindConfigFunc 胜出，且 flags 保持关闭。
func TestWithConfigFileLastWins(t *testing.T) {
	custom := false
	o := NewOptions(
		WithConfigFile("cfg.yaml"),
		WithBindConfigFunc(func(_ *pflag.FlagSet, c ConfigSource) error {
			custom = true
			return nil
		}),
	)
	if o.BindFlagsFunc != nil {
		t.Fatal("flags should stay disabled")
	}
	if err := o.BindConfigFunc(nil, &recordingConfigSource{}); err != nil {
		t.Fatalf("BindConfigFunc returned error: %v", err)
	}
	if !custom {
		t.Error("later WithBindConfigFunc should win over WithConfigFile's binding")
	}
}

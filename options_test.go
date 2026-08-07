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
	// DrainTimeout 无默认值：0 = 不启用排水（与 v1.0 行为一致的回归红线）。
	if o.DrainTimeout != 0 {
		t.Errorf("DrainTimeout = %v, want 0 (no default)", o.DrainTimeout)
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
	if len(o.ExitSignals) != 1 {
		t.Errorf("ExitSignals = %v, want 1 entry", o.ExitSignals)
	}
}

func TestWithSetFlagsAndBindConfig(t *testing.T) {
	flagsSet := false
	bindCalled := false
	o := NewOptions(
		WithSetFlagsFunc(func(f *pflag.FlagSet) { flagsSet = true }),
		WithBindConfigFunc(func(f *pflag.FlagSet, c ConfigSource) error {
			bindCalled = true
			return nil
		}),
	)
	if o.SetFlagsFunc == nil || o.BindConfigFunc == nil {
		t.Fatal("SetFlagsFunc and BindConfigFunc should be set")
	}
	o.SetFlagsFunc(nil)
	if !flagsSet {
		t.Error("SetFlagsFunc was not the provided function")
	}
	if err := o.BindConfigFunc(nil, nil); err != nil {
		t.Fatalf("BindConfigFunc returned error: %v", err)
	}
	if !bindCalled {
		t.Error("BindConfigFunc was not the provided function")
	}
}

// TestDefaultConfigFlagsEnabled 验证默认 flags 默认开启：未显式设置
// SetFlagsFunc/BindConfigFunc 时自动使用框架内置实现。
func TestDefaultConfigFlagsEnabled(t *testing.T) {
	o := &Options{}
	o.EnsureDefaults()
	if o.SetFlagsFunc == nil {
		t.Error("SetFlagsFunc should default to DefaultSetFlagsFunc")
	}
	if o.BindConfigFunc == nil {
		t.Error("BindConfigFunc should default to DefaultBindConfigFunc")
	}

	// NewOptions 路径同样生效。
	o2 := NewOptions()
	if o2.SetFlagsFunc == nil || o2.BindConfigFunc == nil {
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
	if o.SetFlagsFunc != nil || o.BindConfigFunc != nil {
		t.Fatal("WithDisableConfigFlags should clear SetFlagsFunc and BindConfigFunc")
	}
	o.EnsureDefaults()
	if o.SetFlagsFunc != nil || o.BindConfigFunc != nil {
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
}

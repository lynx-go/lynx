package lynx

import (
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/go-viper/mapstructure/v2"
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
			options: Options{ShutdownTimeout: MinShutdownTimeout - time.Millisecond},
			wantErr: ErrCloseTimeoutTooSmall,
		},
		{
			name:    "shutdown timeout at minimum",
			options: Options{ShutdownTimeout: MinShutdownTimeout},
			wantErr: nil,
		},
		{
			name:    "shutdown timeout at maximum",
			options: Options{ShutdownTimeout: MaxShutdownTimeout},
			wantErr: nil,
		},
		{
			name:    "shutdown timeout above maximum",
			options: Options{ShutdownTimeout: MaxShutdownTimeout + time.Millisecond},
			wantErr: ErrCloseTimeoutTooLarge,
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
	if len(o.ExitSignals) != 1 {
		t.Errorf("ExitSignals = %v, want 1 entry", o.ExitSignals)
	}
}

func TestWithSetFlagsAndBindConfig(t *testing.T) {
	flagsSet := false
	bindCalled := false
	o := NewOptions(
		WithSetFlagsFunc(func(f *pflag.FlagSet) { flagsSet = true }),
		WithBindConfigFunc(func(f *pflag.FlagSet, v Config) error {
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

func TestWithUseDefaultConfigFlagsFunc(t *testing.T) {
	o := NewOptions(WithUseDefaultConfigFlagsFunc())
	if o.SetFlagsFunc == nil {
		t.Error("SetFlagsFunc should be set to DefaultSetFlagsFunc")
	}
	if o.BindConfigFunc == nil {
		t.Error("BindConfigFunc should be set to DefaultBindConfigFunc")
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

func TestTagNameFuncs(t *testing.T) {
	jsonCfg := &mapstructure.DecoderConfig{}
	TagNameJSON(jsonCfg)
	if jsonCfg.TagName != "json" {
		t.Errorf("TagName = %q, want %q", jsonCfg.TagName, "json")
	}

	yamlCfg := &mapstructure.DecoderConfig{}
	TagNameYAML(yamlCfg)
	if yamlCfg.TagName != "yaml" {
		t.Errorf("TagName = %q, want %q", yamlCfg.TagName, "yaml")
	}
}

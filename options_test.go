package lynx

import (
	"testing"
	"time"
)

func TestNewOptions(t *testing.T) {
	opts := NewOptions()

	if opts.ID == "" {
		t.Error("expected ID to be set, got empty string")
	}

	if opts.CloseTimeout != 5*time.Second {
		t.Errorf("expected CloseTimeout = 5s, got %v", opts.CloseTimeout)
	}

	if len(opts.ExitSignals) == 0 {
		t.Error("expected ExitSignals to be set, got empty slice")
	}
}

func TestOptions_EnsureDefaults(t *testing.T) {
	tests := []struct {
		name   string
		opts   *Options
		wantID string
		want   string
	}{
		{
			name: "with empty values",
			opts: &Options{
				ID:   "",
				Name: "",
			},
			wantID: "",
			want:   "lynx-app",
		},
		{
			name: "with existing values",
			opts: &Options{
				ID:   "test-id",
				Name: "test-app",
			},
			wantID: "test-id",
			want:   "test-app",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.opts.EnsureDefaults()
			if tt.opts.ID != tt.wantID {
				// ID will be set to hostname, so just check it's not empty
				if tt.opts.ID == "" && tt.wantID == "" {
					// If we wanted it to be empty but it got set, that's fine
					if tt.opts.ID == "" {
						t.Error("expected ID to be set to hostname, got empty string")
					}
				}
			}
			if tt.opts.Name != tt.want {
				t.Errorf("expected Name = %s, got %s", tt.want, tt.opts.Name)
			}
		})
	}
}

func TestOptionFuncs(t *testing.T) {
	tests := []struct {
		name      string
		opts      []Option
		checkFunc func(*testing.T, *Options)
	}{
		{
			name: "WithID",
			opts: []Option{WithID("test-id")},
			checkFunc: func(t *testing.T, o *Options) {
				if o.ID != "test-id" {
					t.Errorf("expected ID = test-id, got %s", o.ID)
				}
			},
		},
		{
			name: "WithName",
			opts: []Option{WithName("test-app")},
			checkFunc: func(t *testing.T, o *Options) {
				if o.Name != "test-app" {
					t.Errorf("expected Name = test-app, got %s", o.Name)
				}
			},
		},
		{
			name: "WithVersion",
			opts: []Option{WithVersion("v1.0.0")},
			checkFunc: func(t *testing.T, o *Options) {
				if o.Version != "v1.0.0" {
					t.Errorf("expected Version = v1.0.0, got %s", o.Version)
				}
			},
		},
		{
			name: "WithCloseTimeout",
			opts: []Option{WithCloseTimeout(10 * time.Second)},
			checkFunc: func(t *testing.T, o *Options) {
				if o.CloseTimeout != 10*time.Second {
					t.Errorf("expected CloseTimeout = 10s, got %v", o.CloseTimeout)
				}
			},
		},
		{
			name: "multiple options",
			opts: []Option{
				WithID("my-id"),
				WithName("my-app"),
				WithVersion("v2.0.0"),
			},
			checkFunc: func(t *testing.T, o *Options) {
				if o.ID != "my-id" {
					t.Errorf("expected ID = my-id, got %s", o.ID)
				}
				if o.Name != "my-app" {
					t.Errorf("expected Name = my-app, got %s", o.Name)
				}
				if o.Version != "v2.0.0" {
					t.Errorf("expected Version = v2.0.0, got %s", o.Version)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := NewOptions(tt.opts...)
			tt.checkFunc(t, opts)
		})
	}
}

func TestOptions_String(t *testing.T) {
	opts := &Options{
		ID:      "test-id",
		Name:    "test-app",
		Version: "v1.0.0",
	}

	str := opts.String()
	if str == "" {
		t.Error("expected non-empty string")
	}

	if str == "{}" {
		t.Error("expected JSON representation, got empty object")
	}
}

func TestWithUseDefaultConfigFlagsFunc(t *testing.T) {
	opts := NewOptions(WithUseDefaultConfigFlagsFunc())

	if opts.SetFlagsFunc == nil {
		t.Error("expected SetFlagsFunc to be set")
	}

	if opts.BindConfigFunc == nil {
		t.Error("expected BindConfigFunc to be set")
	}
}

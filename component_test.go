package lynx

import (
	"context"
	"testing"
)

func TestBuildOptions_EnsureDefaults(t *testing.T) {
	tests := []struct {
		name     string
		options  *BuildOptions
		expected int
	}{
		{
			name:     "default instances",
			options:  &BuildOptions{},
			expected: 1,
		},
		{
			name:     "specified instances",
			options:  &BuildOptions{Instances: 5},
			expected: 5,
		},
		{
			name:     "zero instances",
			options:  &BuildOptions{Instances: 0},
			expected: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.options.ensureDefaults()
			if tt.options.Instances != tt.expected {
				t.Errorf("expected Instances = %d, got %d", tt.expected, tt.options.Instances)
			}
		})
	}
}

type mockComponent struct {
	name string
}

func (m *mockComponent) Name() string {
	return m.name
}

func (m *mockComponent) Init(app Lynx) error {
	return nil
}

func (m *mockComponent) Start(ctx context.Context) error {
	return nil
}

func (m *mockComponent) Stop(ctx context.Context) {
}

type mockComponentBuilder struct {
	instances int
}

func (m *mockComponentBuilder) Build() Component {
	return &mockComponent{name: "mock"}
}

func (m *mockComponentBuilder) Options() BuildOptions {
	return BuildOptions{Instances: m.instances}
}

func TestComponentSet(t *testing.T) {
	components := ComponentSet{
		&mockComponent{name: "comp1"},
		&mockComponent{name: "comp2"},
	}

	if len(components) != 2 {
		t.Errorf("expected 2 components, got %d", len(components))
	}

	if components[0].Name() != "comp1" {
		t.Errorf("expected comp1, got %s", components[0].Name())
	}
}

func TestComponentBuilder(t *testing.T) {
	builder := &mockComponentBuilder{instances: 3}
	component := builder.Build()

	if component == nil {
		t.Fatal("expected non-nil component")
	}

	if component.Name() != "mock" {
		t.Errorf("expected mock, got %s", component.Name())
	}

	options := builder.Options()
	if options.Instances != 3 {
		t.Errorf("expected 3 instances, got %d", options.Instances)
	}
}

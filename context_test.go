package lynx

import (
	"context"
	"testing"
)

func TestFromContextEmpty(t *testing.T) {
	ctx := context.Background()
	if got := IDFromContext(ctx); got != "" {
		t.Errorf("IDFromContext() = %q, want empty", got)
	}
	if got := NameFromContext(ctx); got != "" {
		t.Errorf("NameFromContext() = %q, want empty", got)
	}
	if got := VersionFromContext(ctx); got != "" {
		t.Errorf("VersionFromContext() = %q, want empty", got)
	}
}

func TestFromContextWrongType(t *testing.T) {
	ctx := context.Background()
	ctx = context.WithValue(ctx, keyId, 42)
	ctx = context.WithValue(ctx, keyName, 42)
	ctx = context.WithValue(ctx, keyVersion, 42)
	if got := IDFromContext(ctx); got != "" {
		t.Errorf("IDFromContext() = %q, want empty for wrong type", got)
	}
	if got := NameFromContext(ctx); got != "" {
		t.Errorf("NameFromContext() = %q, want empty for wrong type", got)
	}
	if got := VersionFromContext(ctx); got != "" {
		t.Errorf("VersionFromContext() = %q, want empty for wrong type", got)
	}
}

func TestFromContextSet(t *testing.T) {
	ctx := context.Background()
	ctx = context.WithValue(ctx, keyId, "id-1")
	ctx = context.WithValue(ctx, keyName, "svc")
	ctx = context.WithValue(ctx, keyVersion, "v1")
	if got := IDFromContext(ctx); got != "id-1" {
		t.Errorf("IDFromContext() = %q, want %q", got, "id-1")
	}
	if got := NameFromContext(ctx); got != "svc" {
		t.Errorf("NameFromContext() = %q, want %q", got, "svc")
	}
	if got := VersionFromContext(ctx); got != "v1" {
		t.Errorf("VersionFromContext() = %q, want %q", got, "v1")
	}
}

func TestAppContextCarriesOptions(t *testing.T) {
	app, err := newLynx(NewOptions(
		WithID("id-1"),
		WithName("svc"),
		WithVersion("v1.2.3"),
	))
	if err != nil {
		t.Fatalf("newLynx() error = %v", err)
	}
	ctx := app.Context()
	if got := IDFromContext(ctx); got != "id-1" {
		t.Errorf("IDFromContext() = %q, want %q", got, "id-1")
	}
	if got := NameFromContext(ctx); got != "svc" {
		t.Errorf("NameFromContext() = %q, want %q", got, "svc")
	}
	if got := VersionFromContext(ctx); got != "v1.2.3" {
		t.Errorf("VersionFromContext() = %q, want %q", got, "v1.2.3")
	}
}

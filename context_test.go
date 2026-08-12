package lynx

import (
	"context"
	"testing"
)

func TestMetaEmpty(t *testing.T) {
	ctx := context.Background()
	if got := Meta(ctx); got != (Metadata{}) {
		t.Errorf("Meta() = %+v, want zero", got)
	}
}

func TestMetaWrongType(t *testing.T) {
	ctx := context.WithValue(context.Background(), keyMeta, 42)
	if got := Meta(ctx); got != (Metadata{}) {
		t.Errorf("Meta() = %+v, want zero for wrong type", got)
	}
}

func TestMetaSet(t *testing.T) {
	ctx := context.Background()
	ctx = context.WithValue(ctx, keyMeta, Metadata{ID: "id-1", Name: "svc", Version: "v1"})
	want := Metadata{ID: "id-1", Name: "svc", Version: "v1"}
	if got := Meta(ctx); got != want {
		t.Errorf("Meta() = %+v, want %+v", got, want)
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
	want := Metadata{ID: "id-1", Name: "svc", Version: "v1.2.3"}
	if got := Meta(app.Context()); got != want {
		t.Errorf("Meta() = %+v, want %+v", got, want)
	}
}

package lynx

import "testing"

func TestSetGetAppContext(t *testing.T) {
	Set(nil)
	defer Set(nil)

	if got := Get(); got != nil {
		t.Fatalf("Get() = %v, want nil before Set", got)
	}

	app, err := newLynx(NewOptions())
	if err != nil {
		t.Fatalf("newLynx: %v", err)
	}
	if got := Get(); got != app {
		t.Fatalf("Get() = %v, want app after newLynx", got)
	}

	Set(nil)
	if got := Get(); got != nil {
		t.Fatalf("Get() = %v, want nil after Set(nil)", got)
	}

	Set(app)
	if got := Get(); got != app {
		t.Fatalf("Get() = %v, want app after Set", got)
	}
}

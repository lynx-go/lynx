package schedule

import (
	"testing"
	"time"
)

func TestFireIdentityEveryWallClock(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 7, 0, time.UTC)
	name, ttl, err := fireIdentity("billing", "@every 5s", now, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	wantSlot := time.Date(2026, 1, 1, 0, 0, 5, 0, time.UTC).Format(time.RFC3339)
	if name != "billing@"+wantSlot {
		t.Fatalf("name = %q, want billing@%s", name, wantSlot)
	}
	if ttl != 6*time.Second {
		t.Fatalf("ttl = %s, want 6s", ttl)
	}
	later := now.Add(2 * time.Second) // still in same 5s bucket
	name2, _, err := fireIdentity("billing", "@every 5s", later, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if name2 != name {
		t.Fatalf("same slot: %q vs %q", name, name2)
	}
}

func TestFireIdentityCronHourly(t *testing.T) {
	// 6-field: second minute hour ...
	now := time.Date(2026, 1, 1, 10, 0, 0, 50e6, time.UTC)
	name, ttl, err := fireIdentity("hourly", "0 0 * * * *", now, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	want := "hourly@" + time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC).Format(time.RFC3339)
	if name != want {
		t.Fatalf("name = %q, want %q", name, want)
	}
	if ttl != time.Hour+time.Second {
		t.Fatalf("ttl = %s, want 1h1s", ttl)
	}
}

func TestParseEvery(t *testing.T) {
	d, ok := parseEvery("@every 5s")
	if !ok || d != 5*time.Second {
		t.Fatalf("got %v %v", d, ok)
	}
	if _, ok := parseEvery("0 0 * * * *"); ok {
		t.Fatal("cron spec should not parse as @every")
	}
}

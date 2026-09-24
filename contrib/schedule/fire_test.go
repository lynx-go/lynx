package schedule

import (
	"strings"
	"testing"
	"time"

	"github.com/robfig/cron/v3"
)

// testParser 与内置引擎的默认 parser 同形（6 字段 + Descriptor），
// 供直接调用 fireIdentity 的用例构造 schedule。
var testParser = cron.NewParser(
	cron.Second | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
)

func mustSchedule(t *testing.T, spec string) cron.Schedule {
	t.Helper()
	sched, err := testParser.Parse(spec)
	if err != nil {
		t.Fatalf("Parse(%q): %v", spec, err)
	}
	return sched
}

func TestFireIdentityEveryWallClock(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 7, 0, time.UTC)
	name, ttl, err := fireIdentity("billing", mustSchedule(t, "@every 5s"), now, time.UTC)
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
	name2, _, err := fireIdentity("billing", mustSchedule(t, "@every 5s"), later, time.UTC)
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
	name, ttl, err := fireIdentity("hourly", mustSchedule(t, "0 0 * * * *"), now, time.UTC)
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

// TestFireIdentityCustomParserSchedule 回归：身份计算与引擎实际 parser
// 同源——5 字段自定义 parser 的 schedule 也能工作（旧实现用私有 6 字段
// parser 重新解析 spec，会直接报错）。
func TestFireIdentityCustomParserSchedule(t *testing.T) {
	sched, err := cron.ParseStandard("*/5 * * * *") // 5 字段，无秒
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 10, 3, 12, 0, time.UTC)
	name, ttl, err := fireIdentity("custom", sched, now, time.UTC)
	if err != nil {
		t.Fatalf("custom parser schedule must work: %v", err)
	}
	if !strings.Contains(name, "custom@") || ttl <= 0 {
		t.Fatalf("name = %q, ttl = %v", name, ttl)
	}
}

// TestFireIdentityNilSchedule：nil schedule 返回明确错误。
func TestFireIdentityNilSchedule(t *testing.T) {
	if _, _, err := fireIdentity("x", nil, time.Now(), time.UTC); err == nil {
		t.Fatal("nil schedule must error")
	}
}

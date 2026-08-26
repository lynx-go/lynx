package schedule

import (
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

var exclusiveParser = cron.NewParser(
	cron.Second | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
)

const fireSkew = time.Second

func fireIdentity(taskName, spec string, now time.Time, loc *time.Location) (string, time.Duration, error) {
	if loc != nil {
		now = now.In(loc)
	} else {
		now = now.In(time.Local)
	}
	var slot time.Time
	var interval time.Duration
	if d, ok := parseEvery(spec); ok {
		slot = wallSlot(now, d)
		interval = d
	} else {
		sched, err := exclusiveParser.Parse(spec)
		if err != nil {
			return "", 0, err
		}
		slot, interval = cronSlot(sched, now)
	}
	ttl := interval + fireSkew
	if ttl < time.Second {
		ttl = time.Second
	}
	return taskName + "@" + slot.UTC().Format(time.RFC3339), ttl, nil
}

func parseEvery(spec string) (time.Duration, bool) {
	const prefix = "@every "
	if !strings.HasPrefix(spec, prefix) {
		return 0, false
	}
	d, err := time.ParseDuration(strings.TrimSpace(spec[len(prefix):]))
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}

func wallSlot(now time.Time, d time.Duration) time.Time {
	now = now.UTC()
	ns := d.Nanoseconds()
	if ns <= 0 {
		return now.Truncate(time.Second)
	}
	unixNs := now.UnixNano()
	return time.Unix(0, unixNs-unixNs%ns).UTC()
}

func cronSlot(sched cron.Schedule, now time.Time) (time.Time, time.Duration) {
	next := sched.Next(now)
	next2 := sched.Next(next)
	interval := next2.Sub(next)
	if interval <= 0 {
		interval = time.Second
	}
	candidate := sched.Next(now.Add(-time.Second))
	if !candidate.IsZero() && !candidate.After(now) {
		return candidate, interval
	}
	t := now.Add(-interval - time.Second)
	var prev time.Time
	for range 64 {
		n := sched.Next(t)
		if n.IsZero() || n.After(now) {
			break
		}
		prev = n
		t = n
	}
	if prev.IsZero() {
		prev = now.Truncate(time.Second)
	}
	return prev, interval
}

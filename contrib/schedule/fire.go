package schedule

import (
	"errors"
	"time"

	"github.com/robfig/cron/v3"
)

const fireSkew = time.Second

// fireIdentity 计算一次 Exclusive 触发（格子）的名称与占位 TTL。
// sched 是引擎实际解析的 schedule（NewScheduler 注册时经 Entry 回读），
// 身份计算与引擎同源——WithCron 自定义 parser 时不再分叉：
//   - @every（ConstantDelaySchedule）：取 epoch 对齐的墙钟槽位——其 Next
//     相对调用时刻，各节点起点不同，不能用作跨节点格子名；
//   - 其余 cron 规格：由 schedule 的 Next/Next2 推格子与间隔。
func fireIdentity(taskName string, sched cron.Schedule, now time.Time, loc *time.Location) (string, time.Duration, error) {
	if sched == nil {
		return "", 0, errors.New("schedule: nil schedule")
	}
	if loc != nil {
		now = now.In(loc)
	} else {
		now = now.In(time.Local)
	}
	var slot time.Time
	var interval time.Duration
	if cd, ok := sched.(cron.ConstantDelaySchedule); ok {
		interval = cd.Delay
		slot = wallSlot(now, interval)
	} else {
		slot, interval = cronSlot(sched, now)
	}
	ttl := interval + fireSkew
	if ttl < time.Second {
		ttl = time.Second
	}
	return taskName + "@" + slot.UTC().Format(time.RFC3339), ttl, nil
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

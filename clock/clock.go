package clock

import (
	"context"
	"time"
)

type Clock interface {
	Now() time.Time
	Since(time.Time) time.Duration
	Sleep(time.Duration)
	WithTimeout(context.Context, time.Duration) (context.Context, context.CancelFunc)
}

// System is the default implementation of Clock based on real time.
var System Clock = systemClock{}

type systemClock struct{}

func (systemClock) Now() time.Time {
	return time.Now()
}

func (systemClock) Since(t time.Time) time.Duration {
	return time.Since(t)
}

func (systemClock) Sleep(d time.Duration) {
	time.Sleep(d)
}

func (systemClock) WithTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, d)
}

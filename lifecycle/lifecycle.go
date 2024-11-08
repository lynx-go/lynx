package lifecycle

import (
	"context"
	"github.com/lynx-go/lynx/clock"
	"log/slog"
)

type appState int

const (
	stopped appState = iota
	starting
	incompleteStart
	started
	stopping
)

func (as appState) String() string {
	switch as {
	case stopped:
		return "stopped"
	case starting:
		return "starting"
	case incompleteStart:
		return "incompleteStart"
	case started:
		return "started"
	case stopping:
		return "stopping"
	default:
		return "invalidState"
	}
}

type Lifecycle struct {
	clock  clock.Clock
	logger *slog.Logger
	state  appState
	hooks  []Hook
}

type Hook interface {
	OnStart(ctx context.Context) error
	OnStop(ctx context.Context) error
	Name() string
}

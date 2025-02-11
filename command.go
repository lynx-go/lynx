package lynx

import (
	"context"
	"github.com/lynx-go/lynx/hook"
)

type Command interface {
	Name() string
	Description() string
	Command(ctx context.Context, args []string) error
	Hooks() []hook.Hook
}

package lynx

import (
	"context"
	"github.com/lynx-go/lynx/lifecycle"
)

type Command interface {
	Use() string
	Desc() string
	Example() string
	Run(ctx context.Context, args []string) error
	Hooks() []lifecycle.Hook
	SubCommands() []Command
}

package lynx

import "context"

type Command interface {
	Name() string
	Description() string
	Command(ctx context.Context, args []string) error
	Servers() []Server
}

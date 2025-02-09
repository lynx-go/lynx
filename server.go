package lynx

import "context"

type Server interface {
	Name() string
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}

type NotForCLI interface {
	NotForCLI() bool
}

package lynx

import "github.com/lynx-go/lynx/hook"

type Server interface {
	hook.Hook
	Name() string
}

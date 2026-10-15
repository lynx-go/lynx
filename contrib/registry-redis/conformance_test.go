package registryredis

import (
	"testing"

	"github.com/lynx-go/lynx/contrib/registry"
	"github.com/lynx-go/lynx/contrib/registry/registrytest"
)

// TestConformance：Redis 后端接入 registrytest 契约套件（miniredis）。
func TestConformance(t *testing.T) {
	factory := func(t *testing.T) (registry.Registry, registry.Discovery) {
		c, _, _ := newTestBackend(t)
		return c, c
	}
	registrytest.TestRegistry(t, factory)
	registrytest.TestDiscovery(t, factory)
	registrytest.TestWatcher(t, factory)
}

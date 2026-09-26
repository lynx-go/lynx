package consul

import (
	"testing"

	"github.com/lynx-go/lynx/contrib/registry"
	"github.com/lynx-go/lynx/contrib/registry/registrytest"
)

// TestConformance 把 Consul 后端接入 registrytest 契约套件（fake agent）：
// Close 幂等与 post-close ErrClosed、空名 ErrBadName、Watch 首快照/Stop/取消。
func TestConformance(t *testing.T) {
	factory := func(t *testing.T) (registry.Registry, registry.Discovery) {
		_, srv := newFakeConsul(t)
		c := newTestClient(t, srv)
		return c, c
	}
	registrytest.TestRegistry(t, factory)
	registrytest.TestDiscovery(t, factory)
	registrytest.TestWatcher(t, factory)
}

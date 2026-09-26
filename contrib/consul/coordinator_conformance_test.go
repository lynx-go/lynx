package consul

import (
	"testing"

	"github.com/lynx-go/lynx/contrib/cluster"
	"github.com/lynx-go/lynx/contrib/cluster/clustertest"
)

// TestCoordinatorConformance：Consul Coordinator 接入 clustertest 契约套件
// （session fake：create/renew/destroy + KV acquire）。
func TestCoordinatorConformance(t *testing.T) {
	clustertest.TestCoordinator(t, func(t *testing.T) cluster.Coordinator {
		_, srv := newFakeLock(t)
		c := newTestClient(t, srv)
		return c.Coordinator(cluster.WithNamespace("conformance"))
	})
}

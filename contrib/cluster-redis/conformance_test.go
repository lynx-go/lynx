package clusterredis

import (
	"testing"

	"github.com/lynx-go/lynx/contrib/cluster"
	"github.com/lynx-go/lynx/contrib/cluster/clustertest"
)

// TestConformance：Redis Coordinator 接入 clustertest 契约套件（miniredis）。
func TestConformance(t *testing.T) {
	clustertest.TestCoordinator(t, func(t *testing.T) cluster.Coordinator {
		c, _ := newTestCoordinator(t, cluster.WithNamespace("conformance"))
		return c
	})
}

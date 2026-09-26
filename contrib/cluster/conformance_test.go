package cluster_test

import (
	"testing"

	"github.com/lynx-go/lynx/contrib/cluster"
	"github.com/lynx-go/lynx/contrib/cluster/clustertest"
)

// TestMemoryConformance：进程内 Coordinator 接入 clustertest 契约套件
// （consul / cluster-redis 在各自模块接入）。
func TestMemoryConformance(t *testing.T) {
	clustertest.TestCoordinator(t, func(t *testing.T) cluster.Coordinator {
		return cluster.NewMemory()
	})
}

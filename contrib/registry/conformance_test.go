package registry_test

import (
	"context"
	"net"
	"testing"

	"github.com/lynx-go/lynx/contrib/registry"
	"github.com/lynx-go/lynx/contrib/registry/registrytest"
)

// fakeDNS 是 DNSResolver 的测试替身：SRV 恒 NXDOMAIN，A/AAAA 返回固定 IP。
type fakeDNS struct {
	hosts []string
}

func (f *fakeDNS) LookupSRV(context.Context, string, string, string) (string, []*net.SRV, error) {
	return "", nil, &net.DNSError{IsNotFound: true, Name: "fake"}
}

func (f *fakeDNS) LookupHost(context.Context, string) ([]string, error) {
	return f.hosts, nil
}

// TestMemoryConformance / TestDNSConformance：把 registry 自有的两个后端
// 接入 registrytest 契约套件（consul 在自身模块接入）。DNS 是只读后端，
// 仅参与 Discovery/Watcher 套件。
func TestMemoryConformance(t *testing.T) {
	factory := func(t *testing.T) (registry.Registry, registry.Discovery) {
		m := registry.NewMemory()
		t.Cleanup(func() { _ = m.Close() })
		return m, m
	}
	registrytest.TestRegistry(t, factory)
	registrytest.TestDiscovery(t, factory)
	registrytest.TestWatcher(t, factory)
}

func TestDNSConformance(t *testing.T) {
	factory := func(t *testing.T) (registry.Registry, registry.Discovery) {
		return nil, registry.NewDNSDiscovery(
			registry.WithDNSResolver(&fakeDNS{hosts: []string{"10.0.0.1"}}))
	}
	registrytest.TestDiscovery(t, factory)
	registrytest.TestWatcher(t, factory)
}

//go:build integration

package consul_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/hashicorp/consul/api"
	consul "github.com/lynx-go/lynx/contrib/consul"
	"github.com/lynx-go/lynx/contrib/registry"
)

// TestIntegrationSmoke 对真实 Consul（localhost:8500）做冒烟：
// go test -tags integration ./...
func TestIntegrationSmoke(t *testing.T) {
	cfg := api.DefaultConfig()
	raw, err := api.NewClient(cfg)
	if err != nil {
		t.Skipf("consul client: %v", err)
	}
	if _, err := raw.Status().Leader(); err != nil {
		t.Skipf("no local consul at %s: %v", cfg.Address, err)
	}

	c, err := consul.New(cfg, consul.WithCheckType(consul.CheckTypeTTL))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	inst := registry.Instance{
		Name:   "consul-it",
		ID:     fmt.Sprintf("it-%d", time.Now().UnixNano()),
		Weight: 100,
		Endpoints: []registry.Endpoint{
			{Protocol: registry.ProtocolHTTP, Address: "127.0.0.1:18080"},
			{Protocol: registry.ProtocolGRPC, Address: "127.0.0.1:19090"},
		},
	}
	if err := c.Register(ctx, inst); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Deregister(context.Background(), inst.Name, inst.ID) })

	if err := c.Heartbeat(ctx, inst.Name, inst.ID); err != nil {
		t.Fatal(err)
	}

	got, err := c.GetService(ctx, inst.Name, registry.Filter{IncludeUnhealthy: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0].Endpoints) != 2 {
		t.Fatalf("smoke: %+v", got)
	}
}

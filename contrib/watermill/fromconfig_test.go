package watermill_test

import (
	"strings"
	"testing"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/contrib/watermill"
	"github.com/lynx-go/lynx/eventbus"
	"github.com/spf13/viper"
)

func TestNewFromConfigRoutesAndRejectsLifecycle(t *testing.T) {
	mem := watermill.NewMemoryTransport()
	v := viper.New()
	v.SetConfigType("yaml")
	_ = v.ReadConfig(strings.NewReader(`
bus:
  debug: false
  topics:
    order.created:
      group: order-svc
      route:
        transport: memory
        key: orders
`))
	cfg := lynx.NewViperConfig(v)
	bus, err := watermill.NewFromConfig(cfg, map[string]eventbus.Transport{"memory": mem})
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	if bus == nil {
		t.Fatal("nil bus")
	}
	if err := bus.Init(nil); err != nil {
		t.Fatalf("Init: %v", err)
	}

	v2 := viper.New()
	v2.SetConfigType("yaml")
	_ = v2.ReadConfig(strings.NewReader(`
bus:
  topics:
    lynx.app.started:
      route:
        transport: fake
`))
	fake := &nonMemoryTransport{}
	_, err = watermill.NewFromConfig(lynx.NewViperConfig(v2), map[string]eventbus.Transport{"fake": fake})
	if err == nil {
		t.Fatal("want error routing lynx.* to non-memory")
	}
}

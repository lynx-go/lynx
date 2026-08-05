package lynx

import (
	"strings"
	"testing"

	"github.com/spf13/viper"
)

func TestViperConfigUnmarshalKey(t *testing.T) {
	v := viper.New()
	v.SetConfigType("yaml")
	if err := v.ReadConfig(strings.NewReader(`
kafka:
  orders:
    brokers: ["127.0.0.1:19092"]
    topics: [topic_orders]
    consumer:
      group_id: orders-group
      instances: 3
`)); err != nil {
		t.Fatalf("ReadConfig: %v", err)
	}

	type consumerOptions struct {
		GroupID   string `mapstructure:"group_id"`
		Instances int    `mapstructure:"instances"`
	}
	type topicOptions struct {
		Brokers  []string         `mapstructure:"brokers"`
		Topics   []string         `mapstructure:"topics"`
		Consumer *consumerOptions `mapstructure:"consumer"`
	}
	var got struct {
		Orders topicOptions `mapstructure:"orders"`
	}
	if err := NewViperConfig(v).UnmarshalKey("kafka", &got); err != nil {
		t.Fatalf("UnmarshalKey: %v", err)
	}
	if len(got.Orders.Brokers) != 1 || got.Orders.Brokers[0] != "127.0.0.1:19092" {
		t.Fatalf("unexpected brokers: %+v", got.Orders.Brokers)
	}
	if got.Orders.Consumer == nil || got.Orders.Consumer.GroupID != "orders-group" {
		t.Fatalf("unexpected consumer: %+v", got.Orders.Consumer)
	}
	if got.Orders.Consumer.Instances != 3 {
		t.Fatalf("unexpected instances: %d", got.Orders.Consumer.Instances)
	}
}

func TestViperConfigUnmarshalKeyMissingPath(t *testing.T) {
	v := viper.New()
	v.SetConfigType("yaml")
	if err := v.ReadConfig(strings.NewReader("addr: \":9090\"\n")); err != nil {
		t.Fatalf("ReadConfig: %v", err)
	}
	var got map[string]string
	if err := NewViperConfig(v).UnmarshalKey("kafka", &got); err != nil {
		t.Fatalf("UnmarshalKey on missing path: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty map, got %+v", got)
	}
}

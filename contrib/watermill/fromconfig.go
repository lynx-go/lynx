package watermill

import (
	"fmt"
	"time"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/eventbus"
)

// busFileConfig 是 "bus" 段配置（NewFromConfig 使用）。
type busFileConfig struct {
	Debug      bool                       `mapstructure:"debug"`
	LogMessage *logMessageConfig          `mapstructure:"log_message"`
	Retry      *retryConfig               `mapstructure:"retry"`
	Topics     map[string]topicFileConfig `mapstructure:"topics"`
}

type topicFileConfig struct {
	Route           *routeConfig      `mapstructure:"route"`
	LogMessage      *logMessageConfig `mapstructure:"log_message"`
	AutoAck         bool              `mapstructure:"auto_ack"`
	ContinueOnError bool              `mapstructure:"continue_on_error"`
	Group           string            `mapstructure:"group"`
	Instances       int               `mapstructure:"instances"`
	Retry           *retryConfig      `mapstructure:"retry"`
}

type routeConfig struct {
	Transport string `mapstructure:"transport"`
	Key       string `mapstructure:"key"`
}

type logMessageConfig struct {
	Publish   bool `mapstructure:"publish"`
	Subscribe bool `mapstructure:"subscribe"`
}

func (l *logMessageConfig) toOptions() *eventbus.LogMessageOptions {
	if l == nil {
		return nil
	}
	return &eventbus.LogMessageOptions{Publish: l.Publish, Subscribe: l.Subscribe}
}

type retryConfig struct {
	MaxRetries int           `mapstructure:"max_retries"`
	Backoff    time.Duration `mapstructure:"backoff"`
}

func (r *retryConfig) toOptions() *eventbus.RetryOptions {
	if r == nil {
		return nil
	}
	return &eventbus.RetryOptions{MaxRetries: r.MaxRetries, Backoff: r.Backoff}
}

// NewFromConfig 从配置装配 Watermill Bus：
//   - "bus" 段 topics（逻辑 topic → 选项/route）；route 引用 transports 标识；
//   - lynx.* 禁止 route 到非内存 Transport（RouteKey 校验）；
//   - 标识 "memory" 的 transport 兼作 DefaultTransport；
//   - 不创建 Transport：由调用方创建并传入（生命周期可 Register 或由 Bus.Close 生命周期 Memory）。
func NewFromConfig(cfg lynx.Config, transports map[string]eventbus.Transport) (*Bus, error) {
	var file busFileConfig
	if err := cfg.UnmarshalKey("bus", &file); err != nil {
		return nil, err
	}
	opts := eventbus.Options{
		Debug:      file.Debug,
		LogMessage: file.LogMessage.toOptions(),
		Retry:      file.Retry.toOptions(),
		Topics:     map[string]eventbus.TopicConfig{},
	}
	for name, t := range transports {
		if t == nil {
			continue
		}
		opts.Transports = append(opts.Transports, t)
		if name == "memory" {
			opts.DefaultTransport = t
		}
	}
	for topic, tc := range file.Topics {
		opts.Topics[topic] = eventbus.TopicConfig{
			Group:           tc.Group,
			Instances:       tc.Instances,
			AutoAck:         tc.AutoAck,
			ContinueOnError: tc.ContinueOnError,
			Retry:           tc.Retry.toOptions(),
			LogMessage:      tc.LogMessage.toOptions(),
		}
	}
	bus := New(opts)
	for topic, tc := range file.Topics {
		if tc.Route == nil {
			continue
		}
		t, ok := transports[tc.Route.Transport]
		if !ok || t == nil {
			return nil, fmt.Errorf("bus: route %q references unknown transport %q", topic, tc.Route.Transport)
		}
		key := tc.Route.Key
		if key == "" {
			key = topic
		}
		if err := bus.RouteKey(topic, t, key); err != nil {
			return nil, err
		}
	}
	return bus, nil
}

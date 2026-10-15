package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	gohttp "net/http"
	"time"

	"github.com/lynx-go/lynx"
	clienthttp "github.com/lynx-go/lynx/client/http"
	"github.com/lynx-go/lynx/contrib/registry"
	registryredis "github.com/lynx-go/lynx/contrib/registry-redis"
	"github.com/lynx-go/lynx/server/http"
	"github.com/spf13/pflag"
)

func main() {
	runner := lynx.NewRunner(func(app lynx.App) error {
		logger := app.Logger()

		router := gohttp.NewServeMux()
		router.HandleFunc("/hello", func(rw gohttp.ResponseWriter, r *gohttp.Request) {
			meta := lynx.Meta(app.Context())
			out, _ := json.Marshal(map[string]any{
				"hello": "world",
				"from":  meta.Name,
				"id":    meta.ID,
			})
			_, _ = rw.Write(out)
		})

		hs := http.NewServer(router,
			http.WithAddr(app.Config().GetString("addr")),
			http.WithHealthCheckers(app.HealthCheckers),
		)

		// Redis 后端：registryredis.NewFromConfig 读 registry.redis.* 自建
		// 连接（Close 时一并关闭），*Client 同时实现 Registry 与 Discovery；
		// registry 段缺失 / enabled:false / backend 非 redis 时返回
		// (nil, nil)，回落 NewBackendFromConfig 的零依赖后端（memory/dns）。
		var wr registry.Registry
		var disc registry.Discovery
		if c, err := registryredis.NewFromConfig(app.Config()); err != nil {
			return err
		} else if c != nil {
			wr, disc = c, c
		} else if wr, disc, err = registry.NewBackendFromConfig(app.Config()); err != nil {
			return err
		}

		// Registrar：Redis 实例键 SET EX heartbeat_ttl + 索引 SADD；
		// 心跳循环按 heartbeat_interval 调 Heartbeat（Lua PEXPIRE 续命）。
		// registry.HTTP 把 HTTP 服务器包装为 Advertiser，Start 时等待真实
		// 监听地址出现后注册本进程实例。
		reg, err := registry.NewFromConfig(app.Config(), wr,
			registry.HTTP(hs, registry.ProtocolHTTP),
		)
		if err != nil {
			return err
		}
		// Apply = app.Register(reg) + 挂 OnDrain 注销钩子；reg 为 nil 时 no-op。
		// 注意：CLI（app.Command）的 setup 不要调用 Apply。
		registry.Apply(app, reg)
		app.Register(hs)

		// 客户端侧：Resolver（缓存 + watch）+ discovery:// Transport。
		// Redis 后端的 Watch 是 Pub/Sub 推送 + 兜底轮询，实例注册/注销
		// 毫秒级反映到本进程缓存。
		rslv := registry.NewResolver(disc)
		cli := clienthttp.New(clienthttp.WithClientOptions(func(c *gohttp.Client) {
			c.Transport = registry.NewHTTPTransport(rslv).Wrap(c.Transport)
		}))

		// 演示调用须等 HTTP 开始监听、Registrar 完成注册，而 OnPreStart 先于
		// 各服务 Start 执行，因此放后台 goroutine，不阻塞启动流程。
		app.OnPreStart(func(ctx context.Context) error {
			go demoClient(logger, wr, cli, hs)
			return nil
		})
		return nil
	},
		lynx.WithBindFlagsFunc(func(f *pflag.FlagSet) {
			f.StringP("config", "c", "./config.yaml", "config file path")
			f.String("addr", "", "http listen address")
		}),
		// Apply 挂了 OnDrain 注销钩子，必须启用排水窗口（窗口即钩子总预算，
		// v1.10.0 起两者合并）：关停时先摘流再注销，窗口内服务保持运行。
		lynx.WithDrainTimeout(3*time.Second),
		lynx.WithBindConfigFunc(func(f *pflag.FlagSet, c lynx.ConfigSource) error {
			if cf, _ := f.GetString("config"); cf != "" {
				c.SetFile(cf)
			}
			c.SetEnvPrefix("LYNX")
			c.AutomaticEnv()
			if err := c.BindEnv("addr", "LYNX_ADDR"); err != nil {
				return err
			}
			if err := c.BindEnv("registry.redis.address", "LYNX_REGISTRY_REDIS_ADDRESS"); err != nil {
				return err
			}
			return nil
		}),
	)
	runner.Run()
}

// demoClient 等服务就绪后，向同一目录再写入「另一个服务」的记录（指向
// 本进程端口），然后以 discovery:// URI 按服务名调用两个服务。真实部署
// 中对端记录由对端进程自己的 Registrar 写入，客户端在另一个进程里。
func demoClient(logger *slog.Logger, wr registry.Registry, cli *clienthttp.Client, hs *http.Server) {
	// 固定等待仅用于演示：HTTP 监听 + Registrar 注册都在 Start 阶段并发完成。
	time.Sleep(3 * time.Second)
	ctx := context.Background()

	if err := wr.Register(ctx, registry.Instance{
		Name:   "registry-redis-peer",
		ID:     "registry-redis-peer-1",
		Status: registry.StatusPassing,
		Endpoints: []registry.Endpoint{
			{Protocol: registry.ProtocolHTTP, Address: hs.Addr()},
		},
	}); err != nil {
		logger.Error("register peer failed", "error", err)
		return
	}

	for _, name := range []string{"registry-redis-demo", "registry-redis-peer"} {
		resp, err := cli.Get(ctx, "discovery://"+name+"/hello")
		if err != nil {
			logger.Error("discovery:// call failed", "service", name, "error", err)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		logger.Info("discovery:// call ok", "service", name, "status", resp.StatusCode, "body", string(body))
	}
}

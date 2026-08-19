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

		// memory 后端同时作为 Registry 与 Discovery（仅单进程/测试场景；
		// 生产用 consul.NewFromConfig，见 docs/07-registry.md 7.5 节）。
		wr, disc, err := registry.NewBackendFromConfig(app.Config())
		if err != nil {
			return err
		}

		// Registrar：registry.HTTP 把 HTTP 服务器包装为 Advertiser，
		// Start 时等待真实监听地址出现后注册本进程实例。
		reg, err := registry.NewFromConfig(app.Config(), wr,
			registry.HTTP(hs, registry.ProtocolHTTP),
		)
		if err != nil {
			return err
		}
		// Bind = app.Register(reg) + 挂 OnDrain 注销钩子；reg 为 nil 时 no-op。
		// 注意：CLI（app.Command）的 setup 不要调用 Bind。
		registry.Bind(app, reg)
		app.Register(hs)

		// 客户端侧：Resolver（缓存 + watch）+ registry:// Transport。
		rslv := registry.NewResolver(disc)
		cli := clienthttp.New(clienthttp.WithClientOptions(func(c *gohttp.Client) {
			c.Transport = registry.NewHTTPTransport(rslv).Wrap(c.Transport)
		}))

		// 演示调用须等 HTTP 开始监听、Registrar 完成注册，而 OnStart 先于
		// 各服务 Start 执行，因此放后台 goroutine，不阻塞启动流程。
		app.OnStart(func(ctx context.Context) error {
			go demoClient(logger, wr, cli, hs)
			return nil
		})
		return nil
	},
		lynx.WithSetFlagsFunc(func(f *pflag.FlagSet) {
			f.StringP("config", "c", "./config.yaml", "config file path")
			f.String("addr", "", "http listen address")
		}),
		lynx.WithBindConfigFunc(func(f *pflag.FlagSet, c lynx.ConfigSource) error {
			if cf, _ := f.GetString("config"); cf != "" {
				c.SetFile(cf)
			}
			c.SetEnvPrefix("LYNX")
			c.AutomaticEnv()
			if err := c.BindEnv("addr", "LYNX_ADDR"); err != nil {
				return err
			}
			return nil
		}),
	)
	runner.Run()
}

// demoClient 等服务就绪后，向同一目录再写入「另一个服务」的记录（指向
// 本进程端口），然后以 registry:// URI 按服务名调用两个服务。真实部署
// 中对端记录由对端进程自己的 Registrar 写入，客户端在另一个进程里。
func demoClient(logger *slog.Logger, wr registry.Registry, cli *clienthttp.Client, hs *http.Server) {
	// 固定等待仅用于演示：HTTP 监听 + Registrar 注册都在 Start 阶段并发完成。
	time.Sleep(3 * time.Second)
	ctx := context.Background()

	if err := wr.Register(ctx, registry.Instance{
		Name:   "registry-peer",
		ID:     "registry-peer-1",
		Status: registry.StatusPassing,
		Endpoints: []registry.Endpoint{
			{Protocol: registry.ProtocolHTTP, Address: hs.Addr()},
		},
	}); err != nil {
		logger.Error("register peer failed", "error", err)
		return
	}

	for _, name := range []string{"registry-demo", "registry-peer"} {
		resp, err := cli.Get(ctx, "registry://"+name+"/hello")
		if err != nil {
			logger.Error("registry:// call failed", "service", name, "error", err)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		logger.Info("registry:// call ok", "service", name, "status", resp.StatusCode, "body", string(body))
	}
}

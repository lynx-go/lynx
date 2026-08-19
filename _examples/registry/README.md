# registry 示例

用 `contrib/registry` 的 memory 后端演示服务注册与发现的最小闭环：
HTTP 服务注册自身 → 客户端用 `registry://` URI 按服务名调用。

memory 后端是**进程内**目录，仅用于测试与单进程演示；生产环境用
`contrib/consul`（配置与接入方式见 `docs/07-registry.md` 7.5 节）。

## 运行

```bash
go run .
# 或指定监听地址
go run . --addr=127.0.0.1:9090
LYNX_ADDR=127.0.0.1:9090 go run .
```

启动约 3 秒后可在日志中看到两次 `registry:// call ok`：

- `registry-demo`：本进程 Registrar 注册的服务（`config.yaml` 的
  `service.name`）；
- `registry-peer`：演示代码手动写入目录的「另一个服务」，指向同一端口，
  展示目录内多服务共存与按名寻址。

访问 http://127.0.0.1:8080/hello 直接查看 HTTP 响应；
http://127.0.0.1:8080/healthz/readiness 查看聚合健康状态（含 Registrar）。

## 关键代码点

- `main.go` `registry.NewBackendFromConfig`：按 `registry.backend` 构造
  后端，memory 同时实现 `Registry` 与 `Discovery`；未启用时返回 nil。
- `main.go` `registry.NewFromConfig` + `registry.HTTP(hs, ...)`：构造
  Registrar，HTTP 服务器作为 Advertiser 提供宣告地址（Start 后读
  `Addr()`，Registrar 最多等 `advertise_timeout`）。
- `main.go` `registry.Bind(app, reg)`：注册 Registrar 服务并挂
  `OnDrain` 注销钩子（排水开始即从目录删除实例）；nil 时 no-op。
  CLI（`app.Command`）的 setup 约定**不要**调用 `Bind`。
- `main.go` `registry.NewResolver` + `NewHTTPTransport(rslv).Wrap(...)`：
  客户端发现。`registry://<service>/<path>` 由 Transport 改写为具体
  实例地址，每次请求（含重试）重新解析选实例。
- `main.go` `demoClient`：等服务就绪后写入 peer 记录并发起两次
  `registry://` 调用。真实部署中 peer 记录由对端进程自己的 Registrar
  写入。

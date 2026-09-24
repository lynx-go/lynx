# 08 测试

Lynx 为应用提供两层开箱测试辅助（`lynxtest` 包）与一层集成测试约定：

| 层 | 测什么 | 工具 | 外部依赖 |
| --- | --- | --- | --- |
| L1 单元 | 纯业务逻辑、单个 Service 的 Init/Start/Stop | `lynxtest.NewContext()` | 无 |
| L2 组装测试 | 完整应用：路由、中间件链、健康检查、注册、事件 | `lynxtest.Run()` | 无（回环 TCP） |
| L3 集成 | consul/kafka 等真实后端 | `//go:build integration` + testcontainers | 真实容器 |

完整设计见 [design-testkit.md](design-testkit.md)；框架侧启动期竞态的修复计划见 [design-startup-race.md](design-startup-race.md)。

## 核心原则：复用生产组装路径，只换环境

测试和生产走**同一个 Setup 函数**，环境差异（地址、注册中心后端）全部通过配置注入表达，组装代码里不写 `if isTest` 分支：

```go
// internal/bootstrap/setup.go —— 生产与测试共用
var httpServer *lynxhttp.Server // 暴露给测试取实际地址

func Setup(a lynx.App) error {
    mux := http.NewServeMux()
    mux.HandleFunc("/hello", ...)
    httpServer = lynxhttp.NewServer(mux,
        lynxhttp.WithAddr(a.Config().GetString("http.addr")), // 地址来自配置
        lynxhttp.WithHealthCheckers(a.HealthCheckers),
    )
    a.Register(httpServer)
    return nil
}

// cmd/server/main.go —— main 只留三行
func main() {
    lynx.NewRunner(Setup, lynx.WithName("myapp")).Run()
}
```

注：包级变量捕获 server 引用只适用于串行用例；并行子测试用闭包工厂捕获
（见 `lynxtest` 自测的 `newHTTPSetup` 模式）。测试与 Setup 需同包（或经
导出符号）访问该引用。

## L2：组装测试（`lynxtest.Run`）

```go
func TestHelloEndpoint(t *testing.T) {
    app := lynxtest.Run(t, bootstrap.Setup, lynxtest.WithConfigMap(map[string]any{
        "service.name": "myapp-test",
        "http.addr":    ":0",              // 随机端口，经 httpServer.Addr() 取实际值
        "registry.backend": "memory",      // 内存注册中心，单进程闭环
    }))

    client := lynxtest.HTTPClient(t, bootstrap.httpServer)
    resp, err := client.Get("http://" + bootstrap.httpServer.Addr() + "/hello?name=lynx")
    // ...断言业务语义
}
```

`Run` 的行为：

- **配置默认封闭**：总是注入内存配置，不解析 os.Args、不搜索工作目录——测试二进制的参数和包目录里的 `config.yaml` 不会隐式生效；
- 配置可**分层叠加**（低→高）：`WithConfigBaseline(path)`（生产 yaml 基线）→ `WithConfigYAML` → `WithConfigMap`（最常用，覆盖少数键即可，不必复制整份配置）；
- 基线超时压到下限（Stop/Shutdown/BusReady/Cleanup 各 1s、Drain 100ms），只约束最坏情况。**关闭真实资源（DB 连接池等）的 OnPostStop 钩子在 1s CleanupTimeout 下可能被截断**，重服务用 `WithOptions(lynx.WithCleanupTimeout(...))` 放宽；慢启动总线（Kafka）同理放宽 `WithBusReadyTimeout`；
- 后台 `Run`，`t.Cleanup` 里 `Close` 并等待退出——走与生产信号关停**完全相同的序列**；`Run` 返回错误会以 `t.Errorf` 报告；setup 失败或 panic 同样释放应用并恢复全局；
- 进程级全局（`lynx.Set`/`eventbus.SetDefault`/`slog.SetDefault`）在清理时恢复先前值；
- 已知取舍：基线 DrainTimeout=100ms 使测试态 `HealthCheckers()` 聚合恒含 drainChecker（生产默认 0 时没有）。

**注意：环境变量绑定不生效。** `WithConfig` 注入路径不执行 `BindFlagsFunc`/`BindConfigFunc`（含 `AutomaticEnv`/`BindEnv`）——生产里靠环境变量覆盖的键，测试里必须显式写进 ConfigMap/YAML。

就绪与拨号（server 引用从 setup 闭包捕获）：

```go
// 推荐：句柄版 WaitReady——应用启动失败时立即带出 Run 的实际错误，
// 而不是干等超时报 "not ready"
app.WaitReady(t, 5*time.Second, httpServer, grpcServer)

client := lynxtest.HTTPClient(t, httpServer)   // 已等待就绪，直连回环（绕过代理环境变量）
conn := lynxtest.GRPCConn(t, grpcServer)       // insecure 回环连接，t.Cleanup 自动关闭
conn := lynxtest.GRPCConn(t, grpcServer,       // 可覆盖：TLS 凭据、放宽就绪预算
    lynxtest.WithGRPCDialOptions(grpc.WithTransportCredentials(creds)),
    lynxtest.WithReadyTimeout(10*time.Second))
```

应用日志接测试输出（`-v` 可见、随用例关联；缺省不开，走 stderr 与生产一致）：

```go
lynxtest.Run(t, setup, lynxtest.WithTBLogger(), lynxtest.WithConfigMap(...))
```

需要免 TCP 端口的服务级测试可用监听器注入 + bufconn：

```go
ln := bufconn.Listen(64 * 1024)
srv := lynxhttp.NewServer(handler, lynxhttp.WithListener(ln))
// ...启动后：
client := lynxtest.BufconnHTTPClient(t, ln)   // URL host 任意：http://bufconn/path
conn := lynxtest.BufconnGRPCConn(t, gln)      // gRPC 同款（lynxgrpc.WithListener）
```

### 并行限制

- **同包内存在任何 `t.Parallel()` 用例时，`Run` 管理的用例必须显式
  `WithOptions(lynx.WithIsolated())`**——并行用例在 Run 用例运行期间会看到
  被改写的 `slog.Default`/`lynx.Get`/`eventbus.Default`。纯串行包无此约束；
- 一个用例一个 App（`Run` 是单次语义，不可重启）；
- `WithIsolated` 的代价：依赖 `lynx.Get()`/`eventbus.Default()` 的业务代码
  在测试里取不到实例（应改用注入的 AppContext/Bus）。

## L1：服务单元测试（`lynxtest.NewContext`）

给单个 Service 提供可用的 `AppContext`：真内存总线（已启动，可立即订阅/发布、ctx 内嵌可供 `eventbus.BusFromContext` 使用）、注入配置、接测试输出的日志、应用元数据（`lynx.Meta` 可见，缺省 `{test-service, test-instance}`）与健康检查器快照：

```go
func TestGreeterService(t *testing.T) {
    actx := lynxtest.NewContext(t,
        lynxtest.ContextWithConfigMap(map[string]any{"greeter.name": "test"}),
        lynxtest.ContextWithMeta(lynx.Metadata{Name: "greeter"}), // 可选：默认 test-service
        lynxtest.ContextWithCheckers(dep))                        // 可选：HealthCheckers() 快照
    // ...
}
```

仍然手写 AppContext 的例外：测试 **App 级注册协议**的替身（hook/服务注册调用记录，如 boot/fromconfig 的 fakeApp）与 debug 的 `/loglevel` 控制面（需要 App 级 `SetLogLevel`）——`NewContext` 面向服务级 Init/Start/Stop。
自定义 `Config` 实现经 `ContextWithConfig` 注入；慢总线经
`ContextWithBusReadyTimeout` 放宽就绪预算（缺省 2s）。

## L3：集成测试

沿用 build tag 约定（见 `contrib/consul/integration_test.go`）：文件头 `//go:build integration`，本地无后端时 `t.Skip`。本地与 CI 统一经 mise：

```bash
mise run test               # 单元 + 组装（-race -shuffle=on，全部 workspace 模块）
mise run test-integration   # 集成（-tags integration，需要本地/容器后端）
```

## 框架侧可测性 API 一览

| API | 用途 |
| --- | --- |
| `lynx.NewApp(opts...)` | 不经 Runner 直接构造 App（测试/宿主内嵌），opts 语义与 `NewRunner` 严格一致 |
| `lynx.WithConfig(cfg)` | 注入配置实例，跳过 flags/文件装配（env 绑定随之不生效） |
| `lynx.WithIsolated()` | 不触碰进程级全局（并行多 App） |
| `lynxhttp.WithListener(ln)` / `lynxgrpc.WithListener(ln)` | 注入监听器（bufconn） |
| `lynx.Server` 接口 | `Addr()/AdvertiseAddr()/Ready()` 泛化（两个 server 均实现） |
| `lynx.ContextWithMeta(ctx, meta)` | 给任意 ctx 附加应用元数据（`lynx.Meta` 的对称写入口） |
| `lynx.Clock` + `internal/clock.Fake` | 时间源接缝：`cluster.WithClock` / `registry.WithResolverClock` 注入假时钟，TTL / 续约 / stale 边界确定性断言（不再 sleep） |

完整可运行示例见 `_examples/testing`。

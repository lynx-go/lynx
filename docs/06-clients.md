# 6. 客户端

服务间调用侧组件。客户端与[第 5 章](./05-servers.md)的服务器组件相对，
共同构成 lynx 的传播闭环：客户端从 ctx 提取日志属性写入请求头/metadata，
服务端中间件透传/还原，使全链路共享同一 request_id。

- [6.1 HTTP 客户端](#61-http-客户端clienthttp)
- [6.2 传播闭环](#62-传播闭环)
- [6.3 gRPC 客户端](#63-grpc-客户端clientgrpc)

## 6.1 HTTP 客户端（client/http）

`client/http` 提供框架的 HTTP 客户端：OpenTelemetry 插装、trace 与日志
属性传播、整体超时与可配置重试。零配置可用。

```go
import clienthttp "github.com/lynx-go/lynx/client/http"

client := clienthttp.New() // 整体超时 30s、无重试、otel 插装就绪

// 携带请求级日志属性（logging.WithAttrs 预置）：
// request_id → X-Request-Id、user_id → X-User-Id 自动写入请求头。
ctx := logging.WithAttrs(context.Background(), slog.String(logging.FieldRequestID, rid))
resp, err := client.Get(ctx, "http://user-service/users/1")
if err != nil { ... }
defer resp.Body.Close() // 响应体由调用方读取与关闭
```

### 超时

`WithTimeout` 设置整体超时（缺省 30s）：自 `Do` 发起起覆盖全部尝试
（含重试），`Do` 返回后仍约束响应体读取（ctx 到期后读取返回错误）。
调用方 ctx 已带 deadline 时不叠加，以调用方为准；传 0 表示无超时。
v1.6 起该语义真正生效：取消时机绑定在响应体 `Close()`/读到 EOF 上
（仿标准库 `cancelTimerBody`），`Do` 返回不再立即取消 ctx——大响应、
分块与流式 body 可在超时窗口内正常读取完毕。

```go
client := clienthttp.New(clienthttp.WithTimeout(5 * time.Second))
```

### 重试

`WithRetry` 启用重试（缺省不重试）：

```go
client := clienthttp.New(clienthttp.WithRetry(3,
    clienthttp.WithRetryInitialInterval(200*time.Millisecond),
    clienthttp.WithRetryMaxInterval(2*time.Second)))
```

- `maxAttempts` 为总尝试次数（含首次）。
- 重试条件：传输层错误（调用方 ctx 取消/超时除外）或状态码
  429/502/503/504。
- 429/503 响应携带 `Retry-After` 时，等待至少其指示的时长（秒数或
  HTTP-date）；等待上限为 min(Retry-After, 整体超时剩余, 2 分钟)，
  且钳制后的等待已覆盖全部剩余预算时直接以超时返回、不再发起注定
  无法完成的重试（v1.6 起，防止极端 `Retry-After: 86400` 挂死）。
- **非幂等警示**：传输层错误重试不区分请求方法——"请求已达对端但
  响应丢失"的场景下重试会重复副作用。非幂等请求（POST 等）应
  `WithRetry(0)` 关闭重试，或由调用方保证幂等（幂等键等）。
- **可重放约束**：带请求体（`req.Body` 非 nil）且不可重放
  （`req.GetBody` 为 nil）的请求只发送一次、不重试，并记 debug 日志。
  `Get`/`Post` 传 `*bytes.Buffer`/`*bytes.Reader`/`*strings.Reader` 时
  自动可重放。
- 每次重试前自动关闭上一次尝试的响应体，不会泄漏连接。

### 逃生口

`WithClientOptions(fn func(*http.Client))` 可透传配置底层 `*http.Client`
（如整体替换 Transport、设置 Client 级字段），在内部默认 transport
（otel 插装的 `http.DefaultTransport` 浅克隆，不修改全局）装配之后应用。

### Do 的行为约定

- **传播**：将 `req.Context()` 的日志属性写入请求头（`request_id` →
  `X-Request-Id`、`user_id` → `X-User-Id`），**已存在的同名请求头不覆盖**
  （显式设置的头部优先）；otel 插装同时注入 `traceparent` 等传播上下文。
- **Do/Get/Post 不读取、不关闭响应体**：调用方负责读取并关闭
  （重试中途丢弃的响应体由客户端内部关闭）。

## 6.2 传播闭环

HTTP 链路（client/http → server/http）的完整时序：

1. 调用方在 ctx 预置日志属性：`logging.WithAttrs(ctx, slog.String("request_id", rid))`。
2. `client.Do` 从 ctx 提取属性写入请求头（`X-Request-Id`、`X-User-Id`，
   已存在的头部不覆盖）；otelhttp transport 同时注入 `traceparent`。
3. 服务端 `server/http.WithRequestID` 中间件透传 `X-Request-Id`
   （未携带则生成 UUID），回写响应头，并经 `logging.WithAttrs` 还原进
   请求 ctx。
4. 服务端请求链内所有日志自动携带同一 `request_id`；响应头中的
   `X-Request-Id` 可供调用方取回继续下传，全链路同 id。

```
调用方 ──logging.WithAttrs──▶ client.Do
                                  │  X-Request-Id / X-User-Id / traceparent
                                  ▼
                            server.WithRequestID（透传/生成 → 回写响应头）
                                  │  logging.WithAttrs 还原
                                  ▼
                            业务 handler 日志（同 request_id）
```

## 6.3 gRPC 客户端（client/grpc）

`client/grpc` 提供框架的 gRPC 客户端：otel 插装（otelgrpc client stats
handler）、trace 与日志属性传播（写入 outgoing metadata）、默认调用
超时。

```go
import clientgrpc "github.com/lynx-go/lynx/client/grpc"

conn, err := clientgrpc.Dial("user-service:9090")
if err != nil { ... }
defer conn.Close()

// 惰性连接：Dial 不发起连接，首次 RPC 时才建立，返回 nil error
// 不代表对端可达。

// ctx 预置日志属性 → 自动写入 outgoing metadata（key 同日志字段名）：
// request_id / user_id。
ctx := logging.WithAttrs(context.Background(), slog.String(logging.FieldRequestID, rid))
resp, err := client.NewUserClient(conn).GetUser(ctx, &GetUserRequest{Id: 1})
```

### Dial 的默认装配

- **传播**：unary/stream 拦截器把 ctx 的日志属性（`request_id`/`user_id`）
  写入 outgoing metadata，key 与日志字段同名；**已存在的 metadata key
  不被覆盖**（显式设置优先）。otelgrpc stats handler 同时注入 trace
  传播上下文。
- **超时**：`WithTimeout` 设置默认调用超时（per-RPC context deadline，
  缺省 30s），在 RPC 发起时注入 ctx deadline；调用方 ctx 已带 deadline
  时不叠加。流式 RPC 的定时器在流结束时释放。
- **传输**：未配置 TLS 时使用明文凭据；`WithTLSConfig(cfg)` 启用 TLS，
  与 server/grpc 侧同名同义（TLSConfig 与
  `WithDialOptions(grpc.WithTransportCredentials(...))` 同传时 TLSConfig
  优先）。

```go
conn, err := clientgrpc.Dial("user-service:9090",
    clientgrpc.WithTimeout(5*time.Second),
    clientgrpc.WithTLSConfig(tlsCfg),
)
```

`WithDialOptions(opts ...grpc.DialOption)` 是逃生口，透传额外的
`grpc.DialOption`（消息大小限制、keepalive 等）。

### 传播边界（gRPC）

**当前边界**：服务端（`server/grpc`）暂不把 incoming metadata 中的
`request_id`/`user_id` 还原为日志属性——gRPC 链路只做客户端写入，
**未形成** HTTP 侧的 request_id 闭环（对端服务内部可自行从
`metadata.FromIncomingContext(ctx)` 读取）。服务端还原入 v1.2 backlog
（届时 client → server 全链路日志同 id）。HTTP 链路（6.2 节）已闭环。

## 下一步

- [第 7 章：服务注册与发现](./07-registry.md) - 客户端如何用 `registry://` URI 经注册发现寻址

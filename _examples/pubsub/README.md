# pubsub 示例

基于 Kafka Transport 的发布/订阅示例：HTTP 端点触发发布，consumer group 消费事件。
Kafka 配置驱动：`config.yaml` 的 `kafka` 段定义逻辑 topic（brokers、物理 topics、consumer/producer 参数），`provides.go` 的 `NewKafkaTransport` 经 `kafka.NewFromConfig` 从 `app.Config()` 加载。

## 依赖

需要本地 Kafka：broker `127.0.0.1:19092`，topic `topic_hello`（地址与 topic 配置在 `config.yaml`）。`notify` 路由走内存 transport，不依赖 Kafka。

## 运行

```bash
go generate .   # Wire 生成依赖图（wire_gen.go；wire 未安装时可跳过，wire_gen.go 已提交）
go run . --config=config.yaml
# 另开终端触发发布（hello 走 Kafka；notify 走内存 transport）
curl http://127.0.0.1:7071/hello
curl http://127.0.0.1:7071/notify
```

## 关键代码点

- `provides.go` `ProviderSet`：Wire 依赖集合——`kafka.NewFromConfig`/`pubsub.NewFromConfig` 等配置驱动构造函数直接作为 provider。
- `provides.go` `NewKafkaTransport`：从 `app.Config()` 的 `kafka` 段加载配置创建 Kafka Transport（订阅按消费组 × 物理 topic × 实例数展开）；段缺失/为空时返回 nil，Wire 注入 nil 指针、`NewBroker` 过滤，kafka 未启用。
- `provides.go` `NewBroker`：经 `pubsub.NewFromConfig` 装配——加载 `pubsub` 段路由表逐条应用 `RouteKey`（引用未知 transport 标识时构建期报错）；`memory` 标识兼作默认回退。
- `provides.go` `NewMemoryTransport`：显式创建内存 Transport（kafka 与 memory 对称，均由调用方创建并注册）。
- `provides.go` `NewComponents`：聚合 memT/kafkaT、Broker、Router、HTTP Server 供 `boot.Bootstrap.Bind` 注册。
- `wire.go`：`//go:build wireinject` 注入器 stub，`go generate` 生成 `wire_gen.go`。
- `handlers.go` `helloHandler`/`notifyHandler`：实现 `pubsub.Handler`，分别消费 `hello`（Kafka）与 `notify`（内存）事件并记录 payload。
- `main.go` `/hello`、`/notify` HTTP 端点（`:7071`）分别发布 JSON 事件，带 UUID message key。

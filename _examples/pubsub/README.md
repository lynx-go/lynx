# pubsub 示例

基于 Kafka Binder 的发布/订阅示例：HTTP 端点触发发布，consumer group 消费事件。

## 依赖

需要本地 Kafka：broker `127.0.0.1:19092`，topic `topic_hello`（地址与 topic 硬编码在 `main.go` 中）。

## 运行

```bash
go run .
# 另开终端触发一次发布
curl http://127.0.0.1:7071/hello
```

## 关键代码点

- `main.go:28-50 kafka.NewBinder`：配置 consumer（consumer group `consumer_hello`，3 实例）与 producer。
- `main.go:51-61`：Broker、Binder 与 `ComponentBuilders` 的注册顺序 —— Binder 需先 `Init` 才能取到 `ConsumerBuilders()`。
- `main.go:62 pubsub.NewRouter`：将 `helloHandler` 绑定到 Broker。
- `main.go:69-72`：`/hello` HTTP 端点（`:7071`）发布事件，带 UUID message key。
- `main.go:83 helloHandler`：实现 `pubsub.Handler`，消费 `hello` 事件并记录 payload。

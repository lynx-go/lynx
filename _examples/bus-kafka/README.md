# bus-kafka 示例：watermill + kafka 跨进程 EventBus

演示 v1.12.0 的 `lynx.WithBusProvider`：总线依赖配置（`bus:`/`kafka:`
段），由框架在配置装配完成后调用 provider 构造。kafka 版装配入口
`wmkafka.NewBusFromConfig` 直接匹配 provider 签名：kafka Transport 作为
配套服务返回，生命周期由框架托管——**不需要**在 `NewRunner` 之前自行
读配置再 `WithBus` 注入。

## 前置：本地 kafka

```bash
docker run -d --name kafka -p 127.0.0.1:9092:9092 apache/kafka:latest
```

## 运行

```bash
go run .          # 终端 1：每 2 秒发布一条订单事件，并订阅消费
go run .          # 终端 2：再跑一个实例
```

两个实例的 `consumer.group_id` 相同（`bus-kafka-example`），构成一个
消费组：**每条消息只投递给其中一个实例**（组内竞争消费）——观察两个
终端的 `audit received order` 日志互补。要广播给所有实例，把第二个实例
的 `group_id` 改成不同值即可。

无 broker 直接运行会在启动期报 `connection refused` 退出——刻意的
快失败。另外注意工作目录须在本示例目录（框架默认搜索 `.` 下的
config.yaml）；在别处运行会找不到 `kafka:` 段，Transport 不加入、
总线退回纯内存（配置即开关）。

## 关键代码点

- `wmkafka.NewBusFromConfig`（`WithBusProvider` 的 provider，`main.go`
  里只有一行）：`kafka:` 段启用时构建 Transport 并作为配套服务返回，
  框架 Register 托管其 Start/Stop 与健康聚合（命令的依赖等待因此能等它
  就绪）；段缺失时为纯内存总线——配置即开关。memory 兜底与自定义
  transport 的手工装配路径见
  [watermill-kafka README](../../contrib/watermill-kafka/README.md)。
- `OrderCreatedTopic`：类型化主题，路由由 `bus.topics.order.created.route`
  声明（`route.key` 映射到 `kafka:` 段的同名条目，物理 topic 与客户端
  配置都在那里）；逐消息的 Kafka 分区键由 `WithMessageKey` 决定
  （同键分区有序）。
- `auditService`：Init 期 `Topic.Subscribe`，消费组参数来自
  `kafka.order.created.consumer`（group_id/instances）。
- 注意：`lynx.*` 内建生命周期事件强制内存 transport，不能 route 到
  kafka（Init 期报错）。

## 对照

- `_examples/bus`：默认内存 Bus 的同构用法（Topic/Subscribe/Publish），
  业务代码零差别——跨进程只是配置与 provider 的事。

# bus 示例

进程内 EventBus 示例：类型化 Topic 发布/订阅、原始事件订阅与内建生命周期事件的组件协同，无需外部中间件。

## 运行

```bash
go run .
```

启动后日志依次出现生命周期协调器捕获的 App/Service 事件、audit/inventory 收到的订单事件。

## 关键代码点

- `main.go:18 eventbus.NewTopic[OrderCreated]`：类型化主题，编译期绑定 Payload 类型。
- `main.go:40 OrderCreatedTopic.Subscribe`：订阅时 Bus 从 Context 解析，Payload 自动反序列化。
- `main.go:54 ctx.Bus().Subscribe`：原始事件订阅（`eventbus.RawEvent`），字符串 topic。
- `main.go:68-91 lifecycleCoordinator`：订阅内建 `lynx.*` 生命周期事件（AppStarted/ServiceRegistered/ServiceStarted/HTTPListening），组件可据此协同启停。
- `main.go:100`：`lifecycleCoordinator` 需最先注册才能捕获后续服务的注册/启动事件。
- `main.go:103-109 OnPreStart`：两种发布方式——`Topic.Publish` 类型化发布与 `app.Bus().Publish` 原始发布。
- Bus 开箱即用（默认内存实现），无需 Register。

跨进程（Kafka）总线见 `_examples/bus-kafka`；EventBus 设计见 `docs/design-eventbus.md`。

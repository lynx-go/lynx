# Changelog

## v1.0.0 (2026-08-05)

Lynx 首个稳定版本。核心生命周期、组件系统、配置系统 API 冻结，此后保持向后兼容。

### 破坏性变更（v1.0 前的最后机会）

- `pubsub.Transport.Publish` 增加 `ctx` 参数（trace/元数据传播）
- `pubsub.NewBroker` 返回 `Broker` 接口而非未导出类型
- 删除遗留 Deprecated API：`SetMessageKey` / `GetMessageKey` / `SetMessageID` / `GetMessageID`
- `command` 重试耗尽错误文案调整为 `timed out waiting for dependencies to be healthy`

### 修复

- **Kafka**：修复真实发布 100% 失败的双重缺陷（缺省 Marshaler 与
  `Producer.Return.Successes` 未设置）；新增 SASL（PLAIN/SCRAM-SHA-256/512）
  与 TLS（CA/SNI/skip-verify）认证配置；consumer/producer 参数按侧独立缓存
- **PubSub**：`Broker.Start` 两阶段提交，部分注册失败后补充 Route 重试不再
  panic；订阅 handler 重名在缓冲期即报错；重试次数/退避可配置
- **核心**：组件 Stop 有界超时（`Options.StopTimeout`）；`Init` 在锁外执行
  （Init 内调用 App 方法不再死锁）；Init/OnStart 失败逆序清理已初始化组件；
  OnStop 错误随 `Run()` 上抛；退出信号提前注册
- **Schedule**：Stop/Start 竞态导致的关闭永久挂起修复；新增时区与任务错误回调
- **HTTP**：脱钩 gocloud.dev/server（显式注入 otel provider，消除进程全局副作用）；
  新增 TLS、IdleTimeout 与 `*http.Server` 逃生口
- **gRPC**：app 级健康检查轮询同步到 `grpc.health.v1`；Recovery 移至拦截器链
  最外层；新增流式拦截器入口
- **Metrics**：重复注册报错；支持注入 OTel Resource
- **Zap**：`NewLogger`/`NewSyncableLogger` 去重；级别键与框架统一

### 新增

- **PubSub 透明序列化**：`Publish` 直接接受业务对象自动序列化（默认 JSON，
  可注入自定义 `Marshaler`）；`pubsub.Subscribe[T]` 类型化订阅自动反序列化；
  字节级 `*Message` 语义保留

### 其他

- 各 contrib 模块独立 LICENSE
- 发布流程：`task release-all --Version=v1.0.0`（见 RELEASE.md）

## v0.7.2 及之前

见 git 历史（未维护独立 changelog）。

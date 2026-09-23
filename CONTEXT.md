# Lynx

Lynx 是 Go 微服务框架：应用生命周期管理、服务编排、消息总线与注册发现。这份文件只定义本上下文的领域语言——不是规格，也不是实现说明。

## 服务与生命周期

**服务（Service）**:
受 App 托管的最小生命周期单元：有名字，经历 Init → Start → Stop 三个阶段。
_Avoid_: 组件（component）、模块（module，指架构概念时）

**应用（App）**:
服务的宿主与生命周期编排者：注册服务、运行、关停。
_Avoid_: 容器

**就绪信号（Ready）**:
服务声明的单调边沿信号：成功跨过启动门槛后关闭，失败不得关闭。表达「可以开始服务了」，不表达「仍然健康」。
_Avoid_: 就绪回调、onReady、健康信号

**健康检查器（Checker）**:
回答「服务此刻是否健康」的探测点，可被反复调用；与就绪信号不同，健康状态可以来回变化。
_Avoid_: 探针、health handler

## 就绪（readiness）

**就绪（Readiness）**:
服务已跨过启动门槛、可以接受流量（或依赖它的服务可以继续）的状态。

**探测（Probe）**:
一次就绪判定动作：一次 Ready 等待，或一次 CheckHealth 调用。探测有界——任何一次探测都不得越过调用方给出的预算。
_Avoid_: 健康检查（那是 Checker 层探测的具体形式）、轮询（那是消费模式）

**等待总预算（Wait budget）**:
一次就绪等待允许消耗的墙钟硬上界；耗尽即判未就绪，由消费方决定重试还是失败。
_Avoid_: timeout（单次上界与总预算都叫 timeout 会混）、readyTimeout（实现字段名）

**单次调用上界（Per-call bound）**:
单次探测调用允许消耗的上限。预算循环模式下取「剩余总预算」：总预算永远是硬上界。
_Avoid_: 超时时间

## 消息总线（eventbus）

**总线（Bus）**:
应用级消息通道：业务对象按 topic 发布/订阅；实现决定投递语义（内存 at-most-once、持久化 at-least-once）。
_Avoid_: 消息队列、broker（那是 Bus 后面的 Transport）

**传输（Transport）**:
Bus 背后可插拔的后端：topic 一律为 Transport 侧键；投递模式（广播 / 消费组）是每个后端的必答属性。
_Avoid_: 驱动、连接器

**解析器（Resolver）**:
Bus 配置解析的唯一归属：marshaler / retry / 收发日志 / 传播键的查找与 Topic 级合并都在此；适配器只消费结果。
_Avoid_: 配置管理器

**投递执行（Invoke）**:
一次订阅投递的语义执行：构建 handler 上下文、固定退避重试、AutoAck / ContinueOnError 裁决；不接触消息确认（ack 时序归适配器）。
_Avoid_: 消费循环（那是适配器的调度）

## 请求标识

**传播键（Propagation key）**:
request_id / user_id 在传输层（HTTP 头与 gRPC metadata）的共享 wire 键 `x-request-id` / `x-user-id`；日志字段名仍是 `request_id` / `user_id`。两侧同源，同一标识不因传输换名字。
_Avoid_: header 名、metadata key（分开命名会让两侧漂移）

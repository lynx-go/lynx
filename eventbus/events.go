// Package eventbus 核心生命周期事件：供内建组件（App/Service/Server/Drain）发布、其他组件订阅以实现协同。
package eventbus

import "time"

// 内建主题常量：以 lynx. 为前缀，避免与业务 topic 冲突。
const (
	TopicAppStarting = "lynx.app.starting"
	TopicAppStarted  = "lynx.app.started"
	TopicAppStopping = "lynx.app.stopping"
	TopicAppStopped  = "lynx.app.stopped"

	TopicServiceRegistered = "lynx.service.registered"
	TopicServiceStarting   = "lynx.service.starting"
	TopicServiceStarted    = "lynx.service.started"
	TopicServiceStopping   = "lynx.service.stopping"
	TopicServiceStopped    = "lynx.service.stopped"
	TopicServiceFailed     = "lynx.service.failed"

	TopicDrainStarting  = "lynx.drain.starting"
	TopicDrainCompleted = "lynx.drain.completed"

	TopicHTTPListening = "lynx.http.listening"
	TopicHTTPStopping  = "lynx.http.stopping"
	TopicHTTPStopped   = "lynx.http.stopped"

	TopicGRPCListening = "lynx.grpc.listening"
	TopicGRPCStopping  = "lynx.grpc.stopping"
	TopicGRPCStopped   = "lynx.grpc.stopped"
)

// AppEvent 是 App 级事件的负载。
type AppEvent struct {
	Name    string    `json:"name"`
	ID      string    `json:"id"`
	Version string    `json:"version"`
	Time    time.Time `json:"time"`
}

// ServiceEvent 是 Service 级事件的负载。
type ServiceEvent struct {
	Service string    `json:"service"`
	Time    time.Time `json:"time"`
	Error   string    `json:"error,omitempty"`
}

// ServerEvent 是 Server（HTTP/gRPC）事件的负载。
type ServerEvent struct {
	Service       string    `json:"service"`
	Addr          string    `json:"addr"`
	AdvertiseAddr string    `json:"advertise_addr,omitempty"`
	Time          time.Time `json:"time"`
	Error         string    `json:"error,omitempty"`
}

// DrainEvent 是排水事件的负载。
type DrainEvent struct {
	Timeout time.Duration `json:"timeout"`
	Time    time.Time     `json:"time"`
}

// 预定义类型化 Topic，便于编译期约束：业务侧可直接 SubscribeTyped。
var (
	AppStartingTopic = NewTopic[AppEvent](TopicAppStarting)
	AppStartedTopic  = NewTopic[AppEvent](TopicAppStarted)
	AppStoppingTopic = NewTopic[AppEvent](TopicAppStopping)
	AppStoppedTopic  = NewTopic[AppEvent](TopicAppStopped)

	ServiceRegisteredTopic = NewTopic[ServiceEvent](TopicServiceRegistered)
	ServiceStartingTopic   = NewTopic[ServiceEvent](TopicServiceStarting)
	ServiceStartedTopic    = NewTopic[ServiceEvent](TopicServiceStarted)
	ServiceStoppingTopic   = NewTopic[ServiceEvent](TopicServiceStopping)
	ServiceStoppedTopic    = NewTopic[ServiceEvent](TopicServiceStopped)
	ServiceFailedTopic     = NewTopic[ServiceEvent](TopicServiceFailed)

	DrainStartingTopic  = NewTopic[DrainEvent](TopicDrainStarting)
	DrainCompletedTopic = NewTopic[DrainEvent](TopicDrainCompleted)

	HTTPListeningTopic = NewTopic[ServerEvent](TopicHTTPListening)
	HTTPStoppingTopic  = NewTopic[ServerEvent](TopicHTTPStopping)
	HTTPStoppedTopic   = NewTopic[ServerEvent](TopicHTTPStopped)

	GRPCListeningTopic = NewTopic[ServerEvent](TopicGRPCListening)
	GRPCStoppingTopic  = NewTopic[ServerEvent](TopicGRPCStopping)
	GRPCStoppedTopic   = NewTopic[ServerEvent](TopicGRPCStopped)
)

// Package registry 提供服务注册与发现的数据模型、接口与进程内后端。
//
// 本包只包含类型（Instance/Endpoint/Filter）、Registry/Discovery/Watcher/
// Advertiser 接口、Picker 与 memory 后端；Registrar、Resolver、DNS 与
// Consul 网络后端在后续 PR 中加入。发现是可选能力，类型刻意不放进根包
// lynx：根包继续只有 Service / Checker / Config。
package registry

import "context"

// Protocol 是 Endpoint 的应用层协议。未知值按 opaque 处理，不拒绝。
const (
	ProtocolHTTP  = "http"
	ProtocolHTTPS = "https"
	ProtocolGRPC  = "grpc"
)

// Status 是实例的健康状态。
type Status int

// Status 实例的健康状态枚举：Unknown 为零值，Passing 为健康。
const (
	StatusUnknown Status = iota
	StatusPassing
	StatusWarning
	StatusCritical
	// v1 不定义 StatusDraining：排水时直接 Deregister（目录删除），
	// 客户端不会看到「正在排水」的中间态。若以后要先改状态再删，另开 PR。
)

// Endpoint 是一条可拨号地址。Address 必须是 host:port（禁止裸 ":8080"）。
type Endpoint struct {
	Protocol string `json:"protocol"` // http / https / grpc
	Address  string `json:"address"`  // 192.168.1.10:8080
}

// Instance 是一条目录记录：一进程一条，可挂多个 Endpoint。
type Instance struct {
	Name      string            `json:"name"`    // = lynx.Meta.Name = service.name
	ID        string            `json:"id"`      // = lynx.Meta.ID，集群内唯一
	Version   string            `json:"version"` // = lynx.Meta.Version
	Endpoints []Endpoint        `json:"endpoints"`
	Status    Status            `json:"status"`
	Tags      []string          `json:"tags"`
	Meta      map[string]string `json:"meta"`
	Weight    int               `json:"weight"` // 缺省 100；v1 内置 Picker 忽略此字段
}

// Filter 是 Get/Watch 的可选过滤。零值即安全默认（只返回 Passing）。
type Filter struct {
	Protocol         string   // 只保留含该协议 Endpoint 的实例；空 = 不过滤协议
	Tags             []string // 必须同时具备；空 = 不过滤
	IncludeUnhealthy bool     // 零值 false：丢掉非 StatusPassing。不用 Passing bool，避免零值踩坑
}

// Registry 是写接口。实现必须并发安全。Deregister / Close 必须幂等。
type Registry interface {
	Register(ctx context.Context, inst Instance) error
	Deregister(ctx context.Context, serviceName, instanceID string) error
	// Heartbeat 刷新 TTL。后端若只用 HTTP/gRPC 被动探针，可返回 nil。
	Heartbeat(ctx context.Context, serviceName, instanceID string) error
	Close() error
}

// Discovery 是读接口。Get 必须返回快照副本，调用方可原地改。
type Discovery interface {
	GetService(ctx context.Context, name string, filter Filter) ([]Instance, error)
	Watch(ctx context.Context, name string, filter Filter) (Watcher, error)
}

// Watcher 推送实例集合变化：Next 阻塞至集合变化、ctx 取消或不可恢复错误。
// 实现应在首次调用 Next 时立即推送当前快照（含空列表）。
type Watcher interface {
	Next() ([]Instance, error)
	Stop() error
}

// Advertiser 由对外监听的服务实现。Start 前 Endpoints 可返回 nil。
// Registrar 在注册前最多等待 advertise_timeout 直到至少一条 Endpoint。
type Advertiser interface {
	Endpoints() []Endpoint
}

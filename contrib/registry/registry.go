// Package registry 提供服务注册与发现的数据模型、接口与进程内后端。
//
// 本包只包含类型（Instance/Endpoint/Filter）、Registry/Discovery/Watcher/
// Advertiser 接口、Picker 与 memory 后端；Registrar、Resolver、DNS 与
// Consul 网络后端在后续 PR 中加入。发现是可选能力，类型刻意不放进根包
// lynx：根包继续只有 Service / Checker / Config。
package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
)

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

// statusNames 是 Status 枚举到小写字符串的映射（String 与 MarshalJSON 共用）。
var statusNames = [...]string{
	StatusUnknown:  "unknown",
	StatusPassing:  "passing",
	StatusWarning:  "warning",
	StatusCritical: "critical",
}

// String 返回 Status 的小写字符串形式（unknown/passing/warning/critical），
// 未知枚举值（手工构造的越界值，含负数）返回带数值的形式，便于日志定位。
func (s Status) String() string {
	// 边界两头都要防：负数下标与超上界一样会让数组访问越界 panic。
	if s >= 0 && int(s) < len(statusNames) && statusNames[s] != "" {
		return statusNames[s]
	}
	return fmt.Sprintf("status(%d)", int(s))
}

// MarshalJSON 把 Status 序列化为小写字符串（"passing" 等），调试端点与
// JSON 消费方可读。注意：v1.0 起 Status 序列化为 JSON 数字（0/1/2/3），
// 本方法改变了序列化形态——按数字解码的消费者需依赖 UnmarshalJSON 的
// 数字兼容；新增方法属增量 API，视为可接受的破坏（与 Consul 健康状态
// 的字符串语义对齐）。
func (s Status) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.String())
}

// UnmarshalJSON 是 MarshalJSON 的对侧：接受小写字符串（本版本序列化
// 形态）与 JSON 数字（v1.0 的既有形态，向后兼容）。否则同版本 JSON
// 往返（marshal 再 unmarshal）会失败。越界数字、未知字符串与非
// number/string 形态返回错误；JSON null 按惯例为 no-op。
func (s *Status) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		return nil
	}
	var n int
	if err := json.Unmarshal(data, &n); err == nil {
		if n < 0 || n >= len(statusNames) {
			return fmt.Errorf("registry: status value %d out of range", n)
		}
		*s = Status(n)
		return nil
	}
	var name string
	if err := json.Unmarshal(data, &name); err != nil {
		return fmt.Errorf("registry: status must be number or string, got %s", data)
	}
	for i, want := range statusNames {
		if name == want {
			*s = Status(i)
			return nil
		}
	}
	return fmt.Errorf("registry: unknown status %q", name)
}

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

// MatchFilter 应用 Filter 的匹配语义，是全部后端（memory / DNS / Consul）
// 与 Resolver 读路径的唯一实现：默认只保留 StatusPassing；Protocol 要求
// 实例至少有一条该协议 Endpoint；Tags 必须全匹配。此前各模块各持一份
// 副本实现，Filter 增字段必然漂移（RC-08），故导出收敛到本包。
func MatchFilter(f Filter, i Instance) bool {
	if !f.IncludeUnhealthy && i.Status != StatusPassing {
		return false
	}
	if f.Protocol != "" {
		found := false
		for _, ep := range i.Endpoints {
			if ep.Protocol == f.Protocol {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	for _, tag := range f.Tags {
		if !slices.Contains(i.Tags, tag) {
			return false
		}
	}
	return true
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

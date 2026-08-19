package registry

import (
	lynxgrpc "github.com/lynx-go/lynx/server/grpc"
	lynxhttp "github.com/lynx-go/lynx/server/http"
)

// HTTP 返回 HTTP 服务器的 Advertiser：AdvertiseAddr 非空优先，否则用
// Addr()（Start 前 Addr 为空，Endpoints 返回 nil，Registrar 会轮询等待）。
// protocol 必填 http 或 https；空视为 http。适配器绝不读取服务器 TLS
// 配置猜测 https——协议由调用方显式传入。
func HTTP(s *lynxhttp.Server, protocol string) Advertiser {
	if protocol == "" {
		protocol = ProtocolHTTP
	}
	return &serverAdvertiser{protocol: protocol, addr: serverAddr(s)}
}

// GRPC 返回 gRPC 服务器的 Advertiser，协议固定为 grpc。
func GRPC(s *lynxgrpc.Server) Advertiser {
	return &serverAdvertiser{protocol: ProtocolGRPC, addr: serverAddr(s)}
}

// Static 返回固定地址的 Advertiser（如生产环境用 Downward API 填好的
// host:port）。hostPort 为空时 Endpoints 返回 nil。
func Static(protocol, hostPort string) Advertiser {
	return &serverAdvertiser{protocol: protocol, addr: func() string { return hostPort }}
}

// addresser 是 server/http 与 server/grpc 共有的访问器子集。
type addresser interface {
	Addr() string
	AdvertiseAddr() string
}

// serverAdvertiser 是对服务器访问器的薄适配，不含任何协议猜测。
type serverAdvertiser struct {
	protocol string
	addr     func() string
}

// serverAddr 适配 s：AdvertiseAddr 非空优先，否则回落 Addr()。
func serverAddr(s addresser) func() string {
	return func() string {
		if advertise := s.AdvertiseAddr(); advertise != "" {
			return advertise
		}
		return s.Addr()
	}
}

func (a *serverAdvertiser) Endpoints() []Endpoint {
	addr := a.addr()
	if addr == "" {
		return nil
	}
	return []Endpoint{{Protocol: a.protocol, Address: addr}}
}

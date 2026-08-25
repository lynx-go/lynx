package registry

import (
	"errors"
	"fmt"
	"net"
	"net/http"
)

// ErrBadProtocol 表示 registry:// URI 的 protocol 查询参数非法。
// HTTP 侧只允许 http/https；gRPC 侧只允许 grpc（见 Builder）。
var ErrBadProtocol = errors.New("registry: unsupported protocol")

// HTTPTransport 把 registry:// 请求改写为具体实例地址后交给内层
// Transport。构造必须吃 *Resolver（而非 raw Discovery），保证与 gRPC
// 路径共享同一套缓存 / stale 上限 / 默认 Filter。
//
// 接入形状（otelhttp 包在内层，span 看到改写后的目标）：
//
//	cli := clienthttp.New(clienthttp.WithClientOptions(func(c *http.Client) {
//		// c.Transport 此时已是 otelhttp.Transport
//		c.Transport = registry.NewHTTPTransport(rslv).Wrap(c.Transport)
//	}))
type HTTPTransport struct {
	rslv *Resolver
}

// NewHTTPTransport 返回基于 rslv 的 HTTPTransport。
func NewHTTPTransport(rslv *Resolver) *HTTPTransport {
	return &HTTPTransport{rslv: rslv}
}

// Wrap 返回包在 base 外层的 RoundTripper；base 为 nil 时退到
// http.DefaultTransport。
func (t *HTTPTransport) Wrap(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &registryRoundTripper{rslv: t.rslv, base: base}
}

// registryRoundTripper 是 Wrap 返回的 RoundTripper 实现。
type registryRoundTripper struct {
	rslv *Resolver
	base http.RoundTripper
}

// RoundTrip 解析 registry://<service-name>[:<port>]/<path>[?<query>]：
//
//  1. 非 registry scheme：原样交给内层 Transport。
//  2. Clone 请求后改写 clone，绝不修改调用方的 *http.Request/URL——
//     client/http 的 WithRetry 循环复用同一 request，每次 RoundTrip
//     必须重新解析，否则重试会钉死在第一次选中的实例上。
//  3. Host = 服务名（可选带 :port）；默认 Filter.Protocol=http；查询键
//     protocol 只允许 http/https，是保留键，从 clone 的 RawQuery 删除
//     后再写回，不漏给业务。
//  4. URL.Scheme = 选中 Endpoint 的协议，URL.Host = Endpoint.Address，
//     Path 保持；Host 头与 URL.Host 一致（调用方显式改成其它值的
//     Host 保留；http.NewRequest 预填的 Host 等于原 URL.Host，会随之改写）。
func (t *registryRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "registry" {
		return t.base.RoundTrip(req)
	}
	// 用 URL.Host（而非 Hostname()）拆出可选端口：Hostname() 会静默丢弃
	// registry://svc:8080 的端口，改写后拨到错误端口（RC-12）。端口
	// 非空时要求目录 Endpoint 端口与之匹配，不匹配报错而非静默丢。
	// SplitHostPort 成功但端口为空（"svc:"，registry://svc:/path 的
	// URL.Host）视为无端口：name 取 host 部分，不带尾部冒号——否则
	// "svc:" 会整串被当服务名去查目录，报出费解的解析错误（复审 D）。
	name := req.URL.Host
	port := ""
	if hostOnly, portOnly, err := net.SplitHostPort(req.URL.Host); err == nil {
		name = hostOnly
		port = portOnly
	}
	if name == "" {
		return nil, ErrBadName
	}

	clone := req.Clone(req.Context())
	q := clone.URL.Query()
	protocol := q.Get("protocol")
	q.Del("protocol")
	clone.URL.RawQuery = q.Encode()
	if protocol != "" && protocol != ProtocolHTTP && protocol != ProtocolHTTPS {
		return nil, fmt.Errorf("registry: %w %q (http/https only)", ErrBadProtocol, protocol)
	}

	ep, err := t.pickHTTPEndpoint(clone, name, port, protocol)
	if err != nil {
		return nil, fmt.Errorf("registry: resolve %q: %w", name, err)
	}
	clone.URL.Scheme = ep.Protocol
	clone.URL.Host = ep.Address
	// Host 头与 URL.Host 一致。注意 http.NewRequest 会把 Host 预填为
	// 原 URL.Host，因此仅当 Host 为空或仍等于原 registry URL 的 Host
	// 时才改写；调用方显式改成其它值的 Host 保留。
	if clone.Host == "" || clone.Host == req.URL.Host {
		clone.Host = ep.Address
	}
	return t.base.RoundTrip(clone)
}

// pickHTTPEndpoint 选中实例并按协议取 Endpoint。未指定 protocol 时默认
// http；实例没有任何 http Endpoint 但有 https 时回落第一条 https
// （稳定顺序）；两者都有只用 http，避免明文/TLS 混选。
// port 非空（registry://svc:8081 形式）时在选中实例的同协议 Endpoint 中
// 要求端口精确匹配，无匹配返回错误。
func (t *registryRoundTripper) pickHTTPEndpoint(req *http.Request, name, port, protocol string) (Endpoint, error) {
	inst, ep, err := t.selectInstance(req, name, protocol)
	if err != nil {
		return Endpoint{}, err
	}
	if port == "" {
		return ep, nil
	}
	for _, cand := range inst.Endpoints {
		if cand.Protocol != ep.Protocol {
			continue
		}
		if _, candPort, err := net.SplitHostPort(cand.Address); err == nil && candPort == port {
			return cand, nil
		}
	}
	return Endpoint{}, fmt.Errorf("no %s endpoint on port %s (instance %s)", ep.Protocol, port, inst.ID)
}

// selectInstance 按 protocol 选实例与 Endpoint，返回实例以便调用方在
// 其 Endpoints 中做进一步匹配（见 pickHTTPEndpoint 的端口校验）。
func (t *registryRoundTripper) selectInstance(req *http.Request, name, protocol string) (Instance, Endpoint, error) {
	if protocol != "" {
		inst, err := t.rslv.Get(req.Context(), name, Filter{Protocol: protocol})
		if err != nil {
			return Instance{}, Endpoint{}, err
		}
		ep, err := EndpointOf(inst, protocol)
		return inst, ep, err
	}
	inst, err := t.rslv.Get(req.Context(), name, Filter{Protocol: ProtocolHTTP})
	if err == nil {
		ep, epErr := EndpointOf(inst, ProtocolHTTP)
		return inst, ep, epErr
	}
	if !errors.Is(err, ErrNoInstance) {
		return Instance{}, Endpoint{}, err
	}
	// 无 http Endpoint：尝试 https 回落。
	httpErr := err // 保留 http 侧首次错误（通常含 stale/空快照的真实原因）
	inst, err = t.rslv.Get(req.Context(), name, Filter{Protocol: ProtocolHTTPS})
	if err != nil {
		// errors.Join 同时保留两侧错误：此前只返回 https 侧错误，
		// 「http 因快照 stale 被丢」的真实原因被 https 上下文掩盖
		// （RC-13）。joined 错误对 errors.Is 透明。
		return Instance{}, Endpoint{}, errors.Join(httpErr, fmt.Errorf("https fallback: %w", err))
	}
	ep, epErr := EndpointOf(inst, ProtocolHTTPS)
	return inst, ep, epErr
}

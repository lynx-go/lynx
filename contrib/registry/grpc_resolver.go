package registry

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/attributes"
	"google.golang.org/grpc/resolver"
)

// grpcDefaultPollInterval 是 gRPC resolver 轮询 Resolver 缓存的默认间隔。
// Resolver 内部已有 Watch 推送缓存，这里只需按较慢周期把缓存变化
// 翻译成 resolver.State。
const grpcDefaultPollInterval = 5 * time.Second

// grpcBuilder 实现 resolver.Builder，scheme 为 "registry"。
type grpcBuilder struct {
	rslv *Resolver
	// pollInterval 是轮询 Resolver 缓存的间隔；不暴露为 Option，
	// 测试可直接调小。
	pollInterval time.Duration
}

// NewGRPCBuilder 返回 scheme 为 "registry" 的 gRPC resolver.Builder。
// 必须吃 *Resolver（而非 raw Discovery），保证与 HTTP 路径共享同一套
// 缓存 / stale 上限 / 默认 Filter。
//
// 只支持 target `registry:///<service-name>` 与
// `registry:///<service-name>?protocol=grpc`（Host 必须为空，服务名在
// path；Host 非空时 Build 返回 error）。默认 protocol=grpc。
//
// 推荐每条连接经 grpc.WithResolvers 接入（不改 client/grpc 源码）：
//
//	b := registry.NewGRPCBuilder(rslv)
//	conn, err := clientgrpc.Dial("registry:///user-service",
//		clientgrpc.WithDialOptions(
//			grpc.WithResolvers(b),
//			grpc.WithDefaultServiceConfig(`{"loadBalancingConfig":[{"round_robin":{}}]}`),
//		),
//	)
//
// resolver.Register(b) 是进程全局副作用（测试与多 resolver 进程会撞
// scheme），仅作可选便利，不作为唯一入口。
func NewGRPCBuilder(rslv *Resolver) resolver.Builder {
	return &grpcBuilder{rslv: rslv, pollInterval: grpcDefaultPollInterval}
}

// Scheme 返回 "registry"。
func (b *grpcBuilder) Scheme() string { return "registry" }

// Build 解析 target 并启动后台 goroutine 跟踪实例变化。
func (b *grpcBuilder) Build(target resolver.Target, cc resolver.ClientConn, _ resolver.BuildOptions) (resolver.Resolver, error) {
	u := target.URL
	if u.Host != "" {
		// 不支持 registry://user-service/grpc 的 authority 形式，
		// 避免两种写法并存。
		return nil, fmt.Errorf("registry: grpc target must be registry:///<service-name>, got host %q", u.Host)
	}
	name := strings.TrimPrefix(u.Path, "/")
	if name == "" || strings.Contains(name, "/") {
		return nil, fmt.Errorf("registry: %w %q", ErrBadName, u.Path)
	}
	protocol := u.Query().Get("protocol")
	if protocol == "" {
		protocol = ProtocolGRPC
	}
	if protocol != ProtocolGRPC {
		return nil, fmt.Errorf("registry: %w %q (grpc only)", ErrBadProtocol, protocol)
	}
	gr := &grpcResolver{
		rslv:         b.rslv,
		cc:           cc,
		name:         name,
		filter:       Filter{Protocol: protocol},
		pollInterval: b.pollInterval,
		resolveNow:   make(chan struct{}, 1),
		done:         make(chan struct{}),
	}
	gr.wg.Add(1)
	go gr.loop()
	return gr, nil
}

// grpcResolver 是 Build 返回的 resolver.Resolver：轮询 Resolver 缓存，
// 把实例的 grpc Endpoint 翻译成 resolver.Address 后 UpdateState。
type grpcResolver struct {
	rslv         *Resolver
	cc           resolver.ClientConn
	name         string
	filter       Filter
	pollInterval time.Duration

	resolveNow chan struct{}
	done       chan struct{}
	once       sync.Once
	wg         sync.WaitGroup
}

// loop 立即解析一次，随后按 pollInterval 或 ResolveNow 触发再解析。
func (gr *grpcResolver) loop() {
	defer gr.wg.Done()
	gr.resolve()
	t := time.NewTicker(gr.pollInterval)
	defer t.Stop()
	for {
		select {
		case <-gr.done:
			return
		case <-t.C:
			gr.resolve()
		case <-gr.resolveNow:
			gr.resolve()
		}
	}
}

// resolve 经 Resolver.GetAll 取实例（同一套空快照 / stale 上限 /
// 默认 Filter），翻译为 resolver.Address 后 UpdateState。
// 解析出错（如快照超 stale 上限被丢弃）时保留上一次状态、不清空地址，
// 这是 gRPC resolver 对暂态错误的惯例。
func (gr *grpcResolver) resolve() {
	insts, err := gr.rslv.GetAll(context.Background(), gr.name, gr.filter)
	if err != nil {
		slog.Debug("registry: grpc resolve failed, keeping last state",
			"service", gr.name, "error", err)
		return
	}
	// 空列表同样 UpdateState：服务下线立即生效（空快照语义）。
	addrs := make([]resolver.Address, 0, len(insts))
	for _, inst := range insts {
		for _, ep := range inst.Endpoints {
			if ep.Protocol != gr.filter.Protocol {
				continue
			}
			// weight/version 进 Attributes；v1 官方 round_robin 不读。
			addrs = append(addrs, resolver.Address{
				Addr: ep.Address,
				Attributes: attributes.New(
					"weight", inst.Weight,
				).WithValue("version", inst.Version),
			})
		}
	}
	if err := gr.cc.UpdateState(resolver.State{Addresses: addrs}); err != nil {
		slog.Debug("registry: grpc UpdateState failed",
			"service", gr.name, "error", err)
	}
}

// ResolveNow 触发一次立即再解析（非阻塞）。
func (gr *grpcResolver) ResolveNow(resolver.ResolveNowOptions) {
	select {
	case gr.resolveNow <- struct{}{}:
	default:
	}
}

// Close 停掉后台 goroutine；幂等。
func (gr *grpcResolver) Close() {
	gr.once.Do(func() {
		close(gr.done)
		gr.wg.Wait()
	})
}

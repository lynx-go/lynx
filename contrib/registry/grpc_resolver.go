package registry

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/attributes"
	"google.golang.org/grpc/resolver"
)

// grpcDefaultPollInterval 是 gRPC resolver 轮询 Resolver 缓存的默认间隔。
// Resolver 内部已有 Watch 推送缓存，这里只需按较慢周期把缓存变化
// 翻译成 resolver.State。
//
// 已知缺口（后续工作，不在 v1 修复）：gRPC 侧没有订阅 Resolver 缓存变化
// 的 API，只能 5s 轮询——实例增删最多延迟一个轮询周期才反映到连接地址。
// 完整方案是给 Resolver 增加OnChange 回调（cacheEntry 变更通知），届时
// 轮询仅作兜底。
const grpcDefaultPollInterval = 5 * time.Second

// grpcResolveTimeout 是单次 GetAll 的预算。Resolver 缓存命中时 GetAll
// 无网络 IO，但缓存未填充（或已 stale 被丢弃）时会同步走 Discovery 的
// GetService——Discovery 卡住时若无预算，轮询 goroutine 将无限期阻塞
// （RC-07）。3s 与 Registrar 侧 rpcTimeout 对齐。
const grpcResolveTimeout = 3 * time.Second

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

	// lastAddrs 是上一次 UpdateState 的地址集（已排序），hasState 标记
	// 是否已建立基线：无变化的轮询不再 UpdateState（RC-07）。
	lastAddrs []resolver.Address
	hasState  bool
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
// 解析出错（如快照超 stale 上限被丢弃、GetAll 超时）时保留上一次状态、
// 不清空地址，这是 gRPC resolver 对暂态错误的惯例。
//
// GetAll 自带 grpcResolveTimeout 预算：Discovery 网络调用挂死时本方法
// 在预算内返回错误，轮询 goroutine 不会无限期阻塞（RC-07）。
// 地址集与上次相同（排序后比较）则跳过 UpdateState：Resolver 缓存快照
// 顺序不稳定（map 遍历），无 diff 时每次轮询都会触发无意义的
// UpdateState / 重新建连。首个快照（含空列表）始终发布，建立基线。
func (gr *grpcResolver) resolve() {
	ctx, cancel := context.WithTimeout(context.Background(), grpcResolveTimeout)
	insts, err := gr.rslv.GetAll(ctx, gr.name, gr.filter)
	cancel()
	if err != nil {
		gr.rslv.logger.Debug("registry: grpc resolve failed, keeping last state",
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
	// 排序后比较：快照顺序不稳定，集合相同即视为无变化。
	slices.SortFunc(addrs, func(a, b resolver.Address) int {
		return strings.Compare(a.Addr, b.Addr)
	})
	if gr.hasState && equalAddresses(gr.lastAddrs, addrs) {
		return
	}
	if err := gr.cc.UpdateState(resolver.State{Addresses: addrs}); err != nil {
		gr.rslv.logger.Debug("registry: grpc UpdateState failed",
			"service", gr.name, "error", err)
		return
	}
	gr.lastAddrs = addrs
	gr.hasState = true
}

// equalAddresses 比较两个已排序的地址集（Addr 与 Attributes 全等）。
func equalAddresses(a, b []resolver.Address) bool {
	return slices.EqualFunc(a, b, func(x, y resolver.Address) bool {
		return x.Addr == y.Addr && x.Attributes.Equal(y.Attributes)
	})
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

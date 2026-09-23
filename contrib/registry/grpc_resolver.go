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

// grpcFallbackPollInterval 是订阅链路失效时的兜底轮询间隔。
// 正常路径由 Resolver.Subscribe 的缓存订阅驱动：实例变化在一个信号
// 内反映到 UpdateState（毫秒级）。兜底轮询仅覆盖订阅终止后的退化场景
// （理论仅 Resolver 关闭），30s 内自愈。
const grpcFallbackPollInterval = 30 * time.Second

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
	return &grpcBuilder{rslv: rslv, pollInterval: grpcFallbackPollInterval}
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
		subCh:        make(chan subEvent, 1),
	}
	if sub, err := b.rslv.Subscribe(name); err != nil {
		// Subscribe 仅在 Resolver 关闭后失败：退化为纯兜底轮询，
		// Build 不因此失败（连接仍可由 lastState 服务）。
		b.rslv.logger.Debug("registry: grpc subscribe failed, falling back to polling only",
			"service", name, "error", err)
	} else {
		gr.sub = sub
	}
	gr.wg.Add(1)
	go gr.loop()
	return gr, nil
}

// grpcResolver 是 Build 返回的 resolver.Resolver：订阅 Resolver 缓存变化
// （Subscribe），把实例的 grpc Endpoint 翻译成 resolver.Address 后
// UpdateState；订阅终止后退回兜底轮询。
type grpcResolver struct {
	rslv         *Resolver
	cc           resolver.ClientConn
	name         string
	filter       Filter
	pollInterval time.Duration

	// sub 是缓存订阅（Build 时建立）；nil 表示纯兜底轮询模式。
	sub   Watcher
	subCh chan subEvent

	resolveNow chan struct{}
	done       chan struct{}
	once       sync.Once
	wg         sync.WaitGroup

	// lastAddrs 是上一次 UpdateState 的地址集（已排序），hasState 标记
	// 是否已建立基线：无变化不再 UpdateState（RC-07）。
	lastAddrs []resolver.Address
	hasState  bool
}

// subEvent 是订阅消费 goroutine 向主循环投递的事件：insts 为新快照，
// done 标记订阅终止（退回兜底轮询，主循环不退出）。
type subEvent struct {
	insts []Instance
	done  bool
}

// loop 建立基线后由订阅推送驱动；兜底轮询与 ResolveNow 走 GetAll 路径。
func (gr *grpcResolver) loop() {
	defer gr.wg.Done()
	if gr.sub != nil {
		gr.wg.Add(1)
		go gr.consumeSub()
	}
	gr.resolveViaGetAll()
	t := time.NewTicker(gr.pollInterval)
	defer t.Stop()
	for {
		select {
		case <-gr.done:
			return
		case <-t.C:
			gr.resolveViaGetAll()
		case <-gr.resolveNow:
			gr.resolveViaGetAll()
		case ev := <-gr.subCh:
			if ev.done {
				continue
			}
			gr.resolveFromSnapshot(ev.insts)
		}
	}
}

// consumeSub 消费缓存订阅直至订阅终止或 resolver 关闭。
func (gr *grpcResolver) consumeSub() {
	defer gr.wg.Done()
	for {
		insts, err := gr.sub.Next()
		if err != nil {
			select {
			case <-gr.done:
			default:
				// 订阅终止（Stop/Resolver 关闭）：通知主循环退回兜底轮询。
				select {
				case gr.subCh <- subEvent{done: true}:
				case <-gr.done:
				}
			}
			return
		}
		select {
		case gr.subCh <- subEvent{insts: insts}:
		case <-gr.done:
			return
		}
	}
}

// resolveViaGetAll 经 Resolver.GetAll 取实例（基线建立、ResolveNow、
// 兜底轮询路径），随后与订阅推送共用翻译逻辑。
// GetAll 自带 grpcResolveTimeout 预算：Discovery 网络调用挂死时本方法
// 在预算内返回错误，轮询 goroutine 不会无限期阻塞（RC-07）。
// 解析出错（如快照超 stale 上限被丢弃、超时）时保留上一次状态、
// 不清空地址，这是 gRPC resolver 对暂态错误的惯例。
func (gr *grpcResolver) resolveViaGetAll() {
	ctx, cancel := context.WithTimeout(context.Background(), grpcResolveTimeout)
	insts, err := gr.rslv.GetAll(ctx, gr.name, gr.filter)
	cancel()
	if err != nil {
		gr.rslv.logger.Debug("registry: grpc resolve failed, keeping last state",
			"service", gr.name, "error", err)
		return
	}
	gr.resolveFromSnapshot(insts)
}

// resolveFromSnapshot 把实例快照（订阅推送为全量含非 Passing，GetAll
// 为已过滤集——统一先过 MatchFilter）翻译为地址集并按需 UpdateState。
// 空列表同样 UpdateState：服务下线立即生效（空快照语义）。
// 地址集与上次相同（排序后比较）则跳过 UpdateState：Resolver 缓存快照
// 顺序不稳定（map 遍历），无 diff 时重复 UpdateState 会触发无意义的
// 重新建连。首个快照（含空列表）始终发布，建立基线。
func (gr *grpcResolver) resolveFromSnapshot(all []Instance) {
	addrs := make([]resolver.Address, 0, len(all))
	for _, inst := range all {
		if !MatchFilter(gr.filter, inst) {
			continue
		}
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

// Close 停掉后台 goroutine 并退订缓存订阅（任何退出路径都 Stop，
// RC-04 教训）；幂等。
func (gr *grpcResolver) Close() {
	gr.once.Do(func() {
		close(gr.done)
		if gr.sub != nil {
			_ = gr.sub.Stop() // 解除 consumeSub 的 Next 阻塞
		}
		gr.wg.Wait()
	})
}

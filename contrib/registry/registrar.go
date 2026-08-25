package registry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lynx-go/lynx"
)

const (
	defaultAdvertiseTimeout  = 5 * time.Second
	defaultHeartbeatInterval = 10 * time.Second
	defaultWeight            = 100

	// rpcTimeout 是 Register/Deregister/Heartbeat 单次调用的超时预算。
	rpcTimeout = 3 * time.Second
	// heartbeatFailLimit 是 CheckHealth 判定 ErrHeartbeatFailed 的连续失败阈值。
	heartbeatFailLimit = 3
	// advertiseHostEnv 是 advertise host 的环境变量来源（直读，不经 Viper）。
	advertiseHostEnv = "LYNX_ADVERTISE_HOST"
)

// namePattern 是 Registrar 的服务名校验：单段 DNS 标签，长度 1–63。
// 严于核心 Options.Validate（后者只查长度 ≤63）。
var namePattern = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?$`)

// RegistrarOption 配置 Registrar。
type RegistrarOption func(*registrarOptions)

type registrarOptions struct {
	advertisers       []Advertiser
	endpoints         []Endpoint // 静态配置 endpoints
	failFast          bool
	affectReadiness   bool
	heartbeatInterval time.Duration
	advertiseTimeout  time.Duration
	advertiseHost     string
	tags              []string
	meta              map[string]string
	weight            int
	serviceName       string // 覆盖 lynx.Meta.Name
	instanceID        string // 覆盖 lynx.Meta.ID
}

func defaultRegistrarOptions() registrarOptions {
	return registrarOptions{
		failFast:          true,
		affectReadiness:   true,
		heartbeatInterval: defaultHeartbeatInterval,
		advertiseTimeout:  defaultAdvertiseTimeout,
		weight:            defaultWeight,
	}
}

// WithAdvertisers 挂载对外服务的 Advertiser（HTTP/gRPC/Static）。
func WithAdvertisers(advertisers ...Advertiser) RegistrarOption {
	return func(o *registrarOptions) { o.advertisers = append(o.advertisers, advertisers...) }
}

// WithStaticEndpoints 设置静态配置 endpoints（可含裸 ":port"，由 advertise
// host 在 Init 补全）。
func WithStaticEndpoints(endpoints ...Endpoint) RegistrarOption {
	return func(o *registrarOptions) { o.endpoints = append(o.endpoints, endpoints...) }
}

// WithFailFast 设置首次 Register 失败时 Start 是否立即返回错误（默认 true）。
func WithFailFast(v bool) RegistrarOption {
	return func(o *registrarOptions) { o.failFast = v }
}

// WithAffectReadiness 设置 Registrar 的健康状态是否参与 readiness 聚合
// （默认 true）。false 时 CheckHealth 恒返回 nil。
func WithAffectReadiness(v bool) RegistrarOption {
	return func(o *registrarOptions) { o.affectReadiness = v }
}

// WithHeartbeatInterval 设置心跳间隔（默认 10s）。
func WithHeartbeatInterval(d time.Duration) RegistrarOption {
	return func(o *registrarOptions) { o.heartbeatInterval = d }
}

// WithAdvertiseTimeout 设置等待 Advertiser 出现非空 Endpoints 的上限
// （默认 5s）。
func WithAdvertiseTimeout(d time.Duration) RegistrarOption {
	return func(o *registrarOptions) { o.advertiseTimeout = d }
}

// WithAdvertiseHost 设置宣告 host（对应 registry.advertise.host）。
func WithAdvertiseHost(host string) RegistrarOption {
	return func(o *registrarOptions) { o.advertiseHost = host }
}

// WithTags 设置实例标签。
func WithTags(tags ...string) RegistrarOption {
	return func(o *registrarOptions) { o.tags = append(o.tags, tags...) }
}

// WithMeta 设置实例元数据。
func WithMeta(meta map[string]string) RegistrarOption {
	return func(o *registrarOptions) { o.meta = meta }
}

// WithWeight 设置实例权重（默认 100；v1 内置 Picker 忽略）。
func WithWeight(w int) RegistrarOption {
	return func(o *registrarOptions) { o.weight = w }
}

// WithServiceName 覆盖服务名（默认取 lynx.Meta.Name）。
func WithServiceName(name string) RegistrarOption {
	return func(o *registrarOptions) { o.serviceName = name }
}

// WithInstanceID 覆盖实例 ID（默认取 lynx.Meta.ID）。
func WithInstanceID(id string) RegistrarOption {
	return func(o *registrarOptions) { o.instanceID = id }
}

// Registrar 是服务注册的生命周期服务：Init 解析身份与宣告地址，Start
// 注册并维持心跳，Stop 幂等注销。实现 lynx.Service 与 lynx.Checker。
//
// Registrar 不是对外服务：心跳连续失败只影响 readiness（affect_readiness
// 为 true 时），永远不影响 liveness——框架的 /healthz/liveness 本就不
// 消费 Checker。
type Registrar struct {
	reg  Registry
	opts registrarOptions

	logger         *slog.Logger
	healthCheckers lynx.HealthCheckersFunc

	mu            sync.Mutex
	name          string
	id            string
	version       string
	advertiseHost string
	static        []Endpoint // Init 补全后的静态 endpoints

	registered        atomic.Bool // 目录中当前有记录（Deregister 幂等的判定）
	heartbeatFailures atomic.Int32
	stopping          atomic.Bool
	stopCh            chan struct{}
	stopOnce          sync.Once
	heartbeatOnce     sync.Once
}

var (
	_ lynx.Service = (*Registrar)(nil)
	_ lynx.Checker = (*Registrar)(nil)
)

// NewRegistrar 用给定 Registry 构造 Registrar。
func NewRegistrar(r Registry, opts ...RegistrarOption) *Registrar {
	o := defaultRegistrarOptions()
	for _, opt := range opts {
		opt(&o)
	}
	return &Registrar{
		reg:    r,
		opts:   o,
		logger: slog.Default(),
		stopCh: make(chan struct{}),
	}
}

// Name 实现 lynx.Service。
func (r *Registrar) Name() string { return "registry" }

// Init 从 AppContext 解析实例身份与宣告地址。只读 AppContext，不注册任何
// 钩子（docs/03-core-concepts.md 3.6 的硬边界）。
func (r *Registrar) Init(ctx lynx.AppContext) error {
	meta := lynx.Meta(ctx.Context())

	r.mu.Lock()
	defer r.mu.Unlock()

	r.name = firstNonEmpty(r.opts.serviceName, meta.Name)
	if len(r.name) < 1 || len(r.name) > 63 || !namePattern.MatchString(r.name) {
		return fmt.Errorf("%w: %q (must be a single DNS label, 1-63 chars)", ErrBadName, r.name)
	}
	r.id = firstNonEmpty(r.opts.instanceID, meta.ID)
	r.version = meta.Version

	// advertise host 解析：配置 → LYNX_ADVERTISE_HOST（直读环境，不经
	// Viper）→ 所有 Endpoint 已是 host:port 则跳过 → 否则失败。
	// 不做「第一块非回环网卡 / hostname」猜测。
	r.advertiseHost = firstNonEmpty(r.opts.advertiseHost, os.Getenv(advertiseHostEnv))

	static, err := r.completeEndpoints(r.opts.endpoints)
	if err != nil {
		return err
	}
	r.static = static

	r.logger = ctx.Logger("service", "registry")
	r.healthCheckers = ctx.HealthCheckers
	return nil
}

// completeEndpoints 用 advertise host 把裸 ":port" 补成 host:port；host
// 缺失且存在裸端口时返回错误。一律 net.JoinHostPort（IPv6 安全）。
func (r *Registrar) completeEndpoints(endpoints []Endpoint) ([]Endpoint, error) {
	out := make([]Endpoint, 0, len(endpoints))
	for _, ep := range endpoints {
		host, port, err := net.SplitHostPort(ep.Address)
		if err != nil {
			return nil, fmt.Errorf("registry: invalid endpoint address %q: %w", ep.Address, err)
		}
		if host == "" {
			if r.advertiseHost == "" {
				return nil, fmt.Errorf("registry: advertise host required to complete endpoint %q "+
					"(set registry.advertise.host or $%s)", ep.Address, advertiseHostEnv)
			}
			ep.Address = net.JoinHostPort(r.advertiseHost, port)
		}
		out = append(out, ep)
	}
	return out, nil
}

// Start 注册实例并维持心跳，随后阻塞至 Stop（或 ctx 取消）。
//
// 除 fail_fast=true 且首次 Register 失败外，Start 不得提前返回：框架用
// oklog/run 管理服务，Start 返回任意值（含 nil）都会拆掉整个 group，
// HTTP/gRPC 随之停止。
func (r *Registrar) Start(ctx context.Context) error {
	r.mu.Lock()
	if r.stopping.Load() {
		r.mu.Unlock()
		return nil // Stop-before-Start 竞态：直接退出
	}
	r.mu.Unlock()

	if err := r.waitForEndpoints(ctx); err != nil {
		if errors.Is(err, errStartAborted) {
			// ctx 已取消或 Stop 先行：注册注定失败，短路掉 tryRegister
			// 与后台重试（fail_fast=false 时 retryLoop 会在进程退出
			// 路径上继续空转），直接进入阻塞等待（随即返回，RC-21）。
			select {
			case <-ctx.Done():
			case <-r.stopCh:
			}
			return nil
		}
		return err
	}

	if err := r.tryRegister(); err != nil {
		if r.opts.failFast {
			return err // Start 唯一的提前返回路径
		}
		r.logger.Warn("registry: initial register failed, retrying in background", "error", err)
		go r.retryLoop()
	} else {
		r.startHeartbeat()
	}

	r.startWatchDrain()

	// 阻塞到停止：框架在 Stop 之后才 cancel 该 ctx，故同时等内部 stopCh。
	select {
	case <-ctx.Done():
	case <-r.stopCh:
	}
	return nil
}

// errStartAborted 表示等待 Endpoint 期间 ctx 已取消或 Stop 已先行：
// 注册注定失败，Start 应跳过 tryRegister / retryLoop 直接进入阻塞等待。
var errStartAborted = errors.New("registry: start aborted before register")

// waitForEndpoints 轮询 Advertiser 直到出现非空 Endpoints 或到达
// advertise_timeout。超时且没有静态 endpoints 时返回错误；ctx 取消或
// Stop 先行时返回 errStartAborted（而非 nil），调用方据此短路注册
// （RC-21）。
func (r *Registrar) waitForEndpoints(ctx context.Context) error {
	if len(r.opts.advertisers) == 0 {
		return nil
	}
	deadline := time.NewTimer(r.opts.advertiseTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		if r.advertisedEndpointsReady() {
			return nil
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			if len(r.static) > 0 {
				r.logger.Warn("registry: advertise timeout, registering static endpoints only")
				return nil
			}
			return fmt.Errorf("registry: no endpoints after %s (advertise_timeout)", r.opts.advertiseTimeout)
		case <-ctx.Done():
			return errStartAborted
		case <-r.stopCh:
			return errStartAborted
		}
	}
}

func (r *Registrar) advertisedEndpointsReady() bool {
	for _, adv := range r.opts.advertisers {
		if len(adv.Endpoints()) > 0 {
			return true
		}
	}
	return false
}

// instance 组装当前目录记录：静态 endpoints + 各 Advertiser 的 endpoints
// （裸端口用 advertise host 补全；无法补全的跳过并告警）。
//
// 跳过而非报错的原因：静态路径（completeEndpoints）在 Init 即可判定
// 配置错误，适合 fail-fast；Advertiser 的地址来自运行中的监听器，
// 裸端口只意味着「此刻没有可宣告的 host」，跳过 + Warn 让其余 Endpoint
// 继续注册，不违反 Endpoint 的 host:port 契约（registry.go，禁止裸
// ":8080" 入目录）。
func (r *Registrar) instance() Instance {
	endpoints := append([]Endpoint(nil), r.static...)
	for _, adv := range r.opts.advertisers {
		for _, ep := range adv.Endpoints() {
			host, port, err := net.SplitHostPort(ep.Address)
			if err != nil {
				r.logger.Warn("registry: skip advertiser endpoint with malformed address",
					"endpoint", ep.Address, "protocol", ep.Protocol)
				continue
			}
			if host == "" {
				if r.advertiseHost == "" {
					r.logger.Warn("registry: skip bare-port advertiser endpoint without advertise host",
						"endpoint", ep.Address, "protocol", ep.Protocol)
					continue
				}
				ep.Address = net.JoinHostPort(r.advertiseHost, port)
			}
			endpoints = append(endpoints, ep)
		}
	}
	return Instance{
		Name:      r.name,
		ID:        r.id,
		Version:   r.version,
		Endpoints: endpoints,
		Status:    StatusPassing,
		Tags:      r.opts.tags,
		Meta:      r.opts.meta,
		Weight:    r.opts.weight,
	}
}

// tryRegister 执行一次 Register（单次 3s 超时），成功后更新健康状态。
func (r *Registrar) tryRegister() error {
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	if err := r.reg.Register(ctx, r.instance()); err != nil {
		return err
	}
	r.registered.Store(true)
	r.heartbeatFailures.Store(0)
	return nil
}

// retryLoop 是 fail_fast=false 时的后台注册重试：1s→2s→4s…上限 10s。
// 退出条件是内部 stopping 旗标（Stop 置位），不是 Start 的 ctx——该 ctx
// 在 Stop 之后才取消。
func (r *Registrar) retryLoop() {
	backoff := time.Second
	for {
		select {
		case <-time.After(backoff):
		case <-r.stopCh:
			return
		}
		if r.stopping.Load() {
			return
		}
		if err := r.tryRegister(); err == nil {
			r.logger.Info("registry: registered after retry")
			r.startHeartbeat()
			return
		} else {
			r.logger.Warn("registry: register retry failed", "error", err, "next", backoff*2)
		}
		backoff = min(backoff*2, 10*time.Second)
	}
}

// startHeartbeat 在首次 Register 成功后启动心跳 goroutine（只启动一次）。
func (r *Registrar) startHeartbeat() {
	r.heartbeatOnce.Do(func() {
		go r.heartbeatLoop()
	})
}

func (r *Registrar) heartbeatLoop() {
	tick := time.NewTicker(r.opts.heartbeatInterval)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
		case <-r.stopCh:
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
		err := r.reg.Heartbeat(ctx, r.name, r.id)
		cancel()
		if err != nil {
			n := r.heartbeatFailures.Add(1)
			r.logger.Warn("registry: heartbeat failed", "error", err, "consecutive", n)
		} else {
			r.heartbeatFailures.Store(0)
		}
	}
}

// startWatchDrain 启动排水观察（安全网）：用户忘了 Bind 挂 OnDrain、只
// Register(reg) 且 DrainTimeout > 0（drainChecker 进入 HealthCheckers）
// 时，50ms 轮询到 errors.Is(err, lynx.ErrDraining) 即注销。没有
// drainChecker 时该循环永远空转，不产生任何副作用。
func (r *Registrar) startWatchDrain() {
	if r.healthCheckers == nil || len(r.healthCheckers()) == 0 {
		return
	}
	go func() {
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
			case <-r.stopCh:
				return
			}
			for _, checker := range r.healthCheckers() {
				if errors.Is(checker.CheckHealth(), lynx.ErrDraining) {
					r.logger.Info("registry: drain detected, deregistering (watchDrain safety net)")
					_ = r.deregister(context.Background())
					return
				}
			}
		}
	}()
}

// Stop 停掉心跳/重试/排水观察，幂等注销并关闭 Registry。
// 容忍 Stop-before-Start（Lifecycle 契约）；多次调用只执行一次。
func (r *Registrar) Stop(ctx context.Context) error {
	var err error
	r.stopOnce.Do(func() {
		r.stopping.Store(true)
		close(r.stopCh)
		err = r.deregister(ctx)
		// 已注销：此后 CheckHealth 返回 ErrNotRegistered，readiness 保持不健康。
		if cerr := r.reg.Close(); err == nil {
			err = cerr
		}
	})
	return err
}

// deregister 幂等注销：只有「目录中有记录」的那一个调用真正执行 RPC。
// 单次预算 min(ctx deadline, 3s)。
func (r *Registrar) deregister(ctx context.Context) error {
	if !r.registered.CompareAndSwap(true, false) {
		return nil
	}
	ctx, cancel := deregisterContext(ctx)
	defer cancel()
	if err := r.reg.Deregister(ctx, r.name, r.id); err != nil {
		r.logger.Warn("registry: deregister failed", "error", err)
		return err
	}
	return nil
}

// deregisterContext 取 min(ctx 既有 deadline, 3s) 的注销预算。
func deregisterContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < rpcTimeout {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, rpcTimeout)
}

// DeregisterHook 返回供 app.OnDrain 挂载的注销钩子：排水开始时（而非
// Stop 时）从目录摘除实例。
func (r *Registrar) DeregisterHook() lynx.HookFunc {
	return func(ctx context.Context) error {
		return r.deregister(ctx)
	}
}

// CheckHealth 实现 lynx.Checker 状态机：未成功注册（含后台重试中）或已
// 注销 → ErrNotRegistered；已注册且心跳连续失败 < 3 → nil；≥ 3 →
// ErrHeartbeatFailed。affect_readiness=false 时恒返回 nil（不参与
// readiness 聚合的实际效果）。
func (r *Registrar) CheckHealth() error {
	if !r.opts.affectReadiness {
		return nil
	}
	if !r.registered.Load() {
		return ErrNotRegistered
	}
	if r.heartbeatFailures.Load() >= heartbeatFailLimit {
		return ErrHeartbeatFailed
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

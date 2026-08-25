// Package consul 提供服务注册发现的 Consul 生产后端：*Client 同时实现
// registry.Registry 与 registry.Discovery。
//
// 多 Endpoint 编码只在本模块内：主端口（匹配 check 协议的第一条
// Endpoint）写入 Consul Port/Address，其余写入 Meta 键 lynx_endpoints
// （JSON 数组 [{"protocol":"grpc","address":"10.0.0.1:9090"}]），读取时
// 还原，主端口对应 Endpoint 在切片首位。通用 Resolver / memory / DNS
// 从不解析该键。
//
// Token 只从配置或 CONSUL_HTTP_TOKEN 读取，禁止打进日志。
package consul

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/consul/api"
	"github.com/lynx-go/lynx/contrib/registry"
)

const (
	// metaEndpointsKey 是其余 Endpoint 的 Meta 编码键（仅本模块解析）。
	metaEndpointsKey = "lynx_endpoints"
	// metaMainProtocolKey 记录主端口 Endpoint 的协议（读取还原用）。
	metaMainProtocolKey = "lynx_main_protocol"
	metaVersionKey      = "version"
	metaWeightKey       = "weight"
)

// 健康检查类型：ttl / http / grpc。
const (
	CheckTypeTTL  = "ttl"
	CheckTypeHTTP = "http"
	CheckTypeGRPC = "grpc"
)

const (
	defaultHeartbeatTTL    = 30 * time.Second
	defaultDeregisterAfter = 60 * time.Second
	defaultCheckPath       = "/healthz/readiness"
	defaultCheckInterval   = 10 * time.Second
	defaultCheckTimeout    = 3 * time.Second

	// watchWaitTime 是 blocking query 的服务端等待上限。
	watchWaitTime = 5 * time.Minute
	// watchBackoffMin/Max 是 Watch 错误退避重连区间。
	watchBackoffMin = 1 * time.Second
	watchBackoffMax = 30 * time.Second

	// tokenEnv 是 Consul 官方 token 环境变量：配置 token 为空时直读，
	// 优先于配置文件（空 token 才回落配置）。
	tokenEnv = "CONSUL_HTTP_TOKEN"
)

// errClosed 在 Close 之后的读写操作上返回。
var errClosed = errors.New("consul: client closed")

// errWatcherStopped 在 Watcher.Stop 之后阻塞中的 Next 上返回。
var errWatcherStopped = errors.New("consul: watcher stopped")

// Option 配置 Client。
type Option func(*Client)

// WithCheckType 设置健康检查类型：ttl / http / grpc。
// grpc 摘流最多滞后一个 HealthCheckPeriod（server/grpc 默认 10s），
// 不得作为 gRPC-only 进程的唯一摘流手段（gRPC-only 必须用 ttl +
// OnDrain Deregister）。
func WithCheckType(t string) Option {
	return func(c *Client) {
		if t != "" {
			c.checkType = t
		}
	}
}

// WithCheckPath 设置 http 检查路径（默认 /healthz/readiness）。
func WithCheckPath(path string) Option {
	return func(c *Client) {
		if path != "" {
			c.checkPath = path
		}
	}
}

// WithCheckInterval 设置 http/grpc 检查间隔（默认 10s）。
func WithCheckInterval(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.checkInterval = d
		}
	}
}

// WithCheckTimeout 设置 http/grpc 检查超时（默认 3s）。
func WithCheckTimeout(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.checkTimeout = d
		}
	}
}

// WithHeartbeatTTL 设置 ttl 检查的 TTL（默认 30s）。
func WithHeartbeatTTL(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.heartbeatTTL = d
		}
	}
}

// WithDeregisterAfter 设置 DeregisterCriticalServiceAfter（默认 60s）。
func WithDeregisterAfter(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.deregisterAfter = d
		}
	}
}

// WithAllowStale 允许 Watch/Get 陈旧读（默认 false：consistent read）。
func WithAllowStale(v bool) Option {
	return func(c *Client) { c.allowStale = v }
}

// WithLogger 注入 Client 内部日志（目录项回落、Meta 解码失败等 Warn）。
// 缺省回退 slog.Default()。Token 绝不进入日志（包级约定）。
func WithLogger(l *slog.Logger) Option {
	return func(c *Client) {
		if l != nil {
			c.logger = l
		}
	}
}

// Client 是 Consul 后端，同时实现 registry.Registry 与 registry.Discovery。
type Client struct {
	agent  *api.Agent
	health *api.Health
	logger *slog.Logger

	checkType       string
	checkPath       string
	checkInterval   time.Duration
	checkTimeout    time.Duration
	heartbeatTTL    time.Duration
	deregisterAfter time.Duration
	allowStale      bool

	mu       sync.Mutex
	closed   bool
	watchers map[*watcher]struct{}
}

var (
	_ registry.Registry  = (*Client)(nil)
	_ registry.Discovery = (*Client)(nil)
)

// New 用 Consul API 配置构造 Client。config.Token 为空时直读
// CONSUL_HTTP_TOKEN（官方环境变量，优先于配置文件）。
func New(config *api.Config, opts ...Option) (*Client, error) {
	if config == nil {
		config = api.DefaultConfig()
	}
	if config.Token == "" {
		config.Token = os.Getenv(tokenEnv)
	}
	apiClient, err := api.NewClient(config)
	if err != nil {
		return nil, err
	}
	c := &Client{
		agent:           apiClient.Agent(),
		health:          apiClient.Health(),
		logger:          slog.Default(),
		checkType:       CheckTypeHTTP,
		checkPath:       defaultCheckPath,
		checkInterval:   defaultCheckInterval,
		checkTimeout:    defaultCheckTimeout,
		heartbeatTTL:    defaultHeartbeatTTL,
		deregisterAfter: defaultDeregisterAfter,
		watchers:        make(map[*watcher]struct{}),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// Register 注册实例：ServiceID=inst.ID，Tags/Meta（含 version、weight）
// 透传，Port/Address 取第一个匹配 check 协议的 Endpoint，其余 Endpoint
// 写入 Meta 键 lynx_endpoints；同时写 Weights.Passing（v1 Lynx Picker
// 不读）。同 ID 重复注册是 last-write-wins upsert（Consul 语义）。
func (c *Client) Register(ctx context.Context, inst registry.Instance) error {
	if err := c.checkOpen(); err != nil {
		return err
	}
	if inst.Name == "" || inst.ID == "" {
		return registry.ErrBadName
	}
	main, rest, err := c.pickMainEndpoint(inst)
	if err != nil {
		return err
	}
	host, port, err := splitHostPort(main.Address)
	if err != nil {
		return fmt.Errorf("consul: main endpoint %q: %w", main.Address, err)
	}

	meta := maps.Clone(inst.Meta)
	if meta == nil {
		meta = make(map[string]string)
	}
	if inst.Version != "" {
		meta[metaVersionKey] = inst.Version
	}
	if inst.Weight != 0 {
		meta[metaWeightKey] = strconv.Itoa(inst.Weight)
	}
	meta[metaMainProtocolKey] = main.Protocol
	if len(rest) > 0 {
		encoded, err := json.Marshal(rest)
		if err != nil {
			return err
		}
		meta[metaEndpointsKey] = string(encoded)
	}

	// Weight 零值规格化为 Passing=1：Consul 语义里 Weights.Passing=0 表示
	// 「不可用」，而调用方漏配 Weight 多为「用默认」而非「主动摘流」，
	// 与 registrar 侧 defaultWeight 呼应（RC-18）。
	passingWeight := inst.Weight
	if passingWeight == 0 {
		passingWeight = 1
	}
	reg := &api.AgentServiceRegistration{
		ID:      inst.ID,
		Name:    inst.Name,
		Tags:    inst.Tags,
		Meta:    meta,
		Address: host,
		Port:    port,
		Weights: &api.AgentWeights{Passing: passingWeight, Warning: 1},
		Check:   c.buildCheck(main),
	}
	// consul/api v1.34.4 的 ServiceRegisterOpts 支持 WithContext：ctx 必须
	// 传入，否则 Registrar 侧 3s 预算对注册完全失效——Agent 不可达时
	// fail_fast 卡死 Start、后台重试 goroutine 永久挂起（RC-01）。
	return c.agent.ServiceRegisterOpts(reg, api.ServiceRegisterOpts{}.WithContext(ctx))
}

// pickMainEndpoint 选主端口：http 检查取第一条 http/https Endpoint，
// grpc 检查取第一条 grpc Endpoint，ttl 检查取第一条 Endpoint。
func (c *Client) pickMainEndpoint(inst registry.Instance) (registry.Endpoint, []registry.Endpoint, error) {
	if len(inst.Endpoints) == 0 {
		return registry.Endpoint{}, nil, fmt.Errorf("consul: instance %s/%s has no endpoints", inst.Name, inst.ID)
	}
	want := func(protocol string) bool {
		switch c.checkType {
		case CheckTypeHTTP:
			return protocol == registry.ProtocolHTTP || protocol == registry.ProtocolHTTPS
		case CheckTypeGRPC:
			return protocol == registry.ProtocolGRPC
		default: // ttl
			return true
		}
	}
	for i, ep := range inst.Endpoints {
		if want(ep.Protocol) {
			rest := make([]registry.Endpoint, 0, len(inst.Endpoints)-1)
			rest = append(rest, inst.Endpoints[:i]...)
			rest = append(rest, inst.Endpoints[i+1:]...)
			return ep, rest, nil
		}
	}
	return registry.Endpoint{}, nil, fmt.Errorf(
		"consul: instance %s/%s has no endpoint matching check type %q", inst.Name, inst.ID, c.checkType)
}

// buildCheck 按 checkType 构造健康检查。排水写路径是 delete（Deregister），
// 不是改 draining 状态。
func (c *Client) buildCheck(main registry.Endpoint) *api.AgentServiceCheck {
	check := &api.AgentServiceCheck{
		DeregisterCriticalServiceAfter: c.deregisterAfter.String(),
	}
	switch c.checkType {
	case CheckTypeTTL:
		check.TTL = c.heartbeatTTL.String()
	case CheckTypeGRPC:
		check.GRPC = main.Address
		check.Interval = c.checkInterval.String()
	default: // http
		scheme := "http"
		if main.Protocol == registry.ProtocolHTTPS {
			scheme = "https"
		}
		check.HTTP = scheme + "://" + main.Address + c.checkPath
		check.Interval = c.checkInterval.String()
		check.Timeout = c.checkTimeout.String()
	}
	return check
}

// Deregister 注销实例；Consul 服务端对不存在的 ID 返回成功，天然幂等。
func (c *Client) Deregister(ctx context.Context, _, instanceID string) error {
	if err := c.checkOpen(); err != nil {
		return err
	}
	return c.agent.ServiceDeregisterOpts(instanceID, writeCtx(ctx))
}

// Heartbeat 刷新 TTL（check 类型为 ttl 时 UpdateTTL）；http/grpc 被动
// 探针时为 no-op。
func (c *Client) Heartbeat(ctx context.Context, _, instanceID string) error {
	if c.checkType != CheckTypeTTL {
		return nil
	}
	if err := c.checkOpen(); err != nil {
		return err
	}
	return c.agent.UpdateTTLOpts("service:"+instanceID, "heartbeat ok", api.HealthPassing, writeCtx(ctx))
}

// Close 停止全部 Watcher 并拒绝后续读写；幂等。
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	watchers := make([]*watcher, 0, len(c.watchers))
	for w := range c.watchers {
		watchers = append(watchers, w)
	}
	c.mu.Unlock()
	for _, w := range watchers {
		_ = w.Stop()
	}
	return nil
}

// GetService 查询健康服务目录并还原多 Endpoint；应用 Filter。
func (c *Client) GetService(ctx context.Context, name string, filter registry.Filter) ([]registry.Instance, error) {
	if err := c.checkOpen(); err != nil {
		return nil, err
	}
	if name == "" {
		return nil, registry.ErrBadName
	}
	instances, _, err := c.query(ctx, name, filter, 0, 0)
	return instances, err
}

// Watch 返回 blocking-query Watcher：首次 Next 立即推当前快照（含空
// 列表），之后 WaitIndex 推进、集合变化即推送；错误按 1s–30s 退避重连。
func (c *Client) Watch(ctx context.Context, name string, filter registry.Filter) (registry.Watcher, error) {
	if name == "" {
		return nil, registry.ErrBadName
	}
	w := &watcher{
		c:      c,
		name:   name,
		filter: filter,
		ctx:    ctx,
		ch:     make(chan []registry.Instance, 1),
		done:   make(chan struct{}),
	}
	w.first.Store(true)
	// closed 检查必须与注册同临界区（RC-09）：此前 checkOpen 释放锁后才
	// 注册 watcher，Close 若落在窗口内，该 watcher 无人停（直连使用时
	// goroutine 泄漏）。
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errClosed
	}
	c.watchers[w] = struct{}{}
	c.mu.Unlock()
	go w.loop()
	return w, nil
}

// query 执行一次健康目录查询：waitIndex=0 为非阻塞读，否则 blocking。
func (c *Client) query(ctx context.Context, name string, filter registry.Filter, waitIndex uint64, waitTime time.Duration) ([]registry.Instance, *api.QueryMeta, error) {
	qo := (&api.QueryOptions{
		WaitIndex:  waitIndex,
		AllowStale: c.allowStale,
	}).WithContext(ctx)
	if waitTime > 0 {
		qo.WaitTime = waitTime
	}
	entries, meta, err := c.health.Service(name, "", false, qo)
	if err != nil {
		return nil, nil, err
	}
	instances := make([]registry.Instance, 0, len(entries))
	for _, entry := range entries {
		inst, ok := c.convertEntry(entry)
		if !ok {
			continue // 无可拨号地址：已 Warn，跳过（RC-03）
		}
		if registry.MatchFilter(filter, inst) {
			instances = append(instances, inst)
		}
	}
	return instances, meta, nil
}

// convertEntry 把 Consul ServiceEntry 还原为 registry.Instance：主端口
// Endpoint 在切片首位，lynx_endpoints 解码追加；version/weight 回填字段；
// 内部 Meta 键（lynx_*、version、weight）不进入返回的 Meta。
//
// 地址回落（RC-03）：服务未带 Service.Address 注册时（Consul 生态常见，
// agent 会用 Node.Address 拨号）回落 entry.Node.Address；两者皆空返回
// ok=false，调用方跳过该条目并已在此处 Warn——绝不能产出裸 ":8080"
// Endpoint（违反 registry.Endpoint 契约）。
func (c *Client) convertEntry(entry *api.ServiceEntry) (registry.Instance, bool) {
	svc := entry.Service
	meta := maps.Clone(svc.Meta)

	host := svc.Address
	if host == "" && entry.Node != nil {
		host = entry.Node.Address
	}
	if host == "" {
		c.logger.Warn("consul: catalog entry has no dialable address, skipping",
			"service", svc.Service, "id", svc.ID)
		return registry.Instance{}, false
	}

	main := registry.Endpoint{
		Protocol: registry.ProtocolHTTP,
		Address:  net.JoinHostPort(host, strconv.Itoa(svc.Port)),
	}
	endpoints := make([]registry.Endpoint, 0, 4)
	if protocol, ok := meta[metaMainProtocolKey]; ok && protocol != "" {
		main.Protocol = protocol
	}
	endpoints = append(endpoints, main)
	if encoded := meta[metaEndpointsKey]; encoded != "" {
		var rest []registry.Endpoint
		// 解码失败必须 Warn（RC-17）：静默吞掉会让多 Endpoint 服务静默
		// 退化为单 Endpoint，排障无从下手。
		if err := json.Unmarshal([]byte(encoded), &rest); err != nil {
			c.logger.Warn("consul: lynx_endpoints decode failed, falling back to main endpoint only",
				"service", svc.Service, "id", svc.ID, "error", err)
		} else {
			endpoints = append(endpoints, rest...)
		}
	}

	inst := registry.Instance{
		Name:      svc.Service,
		ID:        svc.ID,
		Version:   meta[metaVersionKey],
		Endpoints: endpoints,
		Status:    aggregateStatus(entry.Checks),
		Tags:      svc.Tags,
		Weight:    0,
	}
	if w, err := strconv.Atoi(meta[metaWeightKey]); err == nil {
		inst.Weight = w
	}
	for _, key := range []string{metaEndpointsKey, metaMainProtocolKey, metaVersionKey, metaWeightKey} {
		delete(meta, key)
	}
	if len(meta) > 0 {
		inst.Meta = meta
	}
	return inst, true
}

// aggregateStatus 聚合 Consul Checks：任一 critical → Critical，任一
// warning → Warning，否则 Passing（无 check 视为 Passing）。
func aggregateStatus(checks api.HealthChecks) registry.Status {
	status := registry.StatusPassing
	for _, check := range checks {
		switch check.Status {
		case api.HealthCritical:
			return registry.StatusCritical
		case api.HealthWarning:
			status = registry.StatusWarning
		}
	}
	return status
}

func (c *Client) checkOpen() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errClosed
	}
	return nil
}

// splitHostPort 拆分 host:port（IPv6 安全）。
func splitHostPort(address string) (string, int, error) {
	host, portStr, err := net.SplitHostPort(address)
	if err != nil {
		return "", 0, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "", 0, err
	}
	return host, port, nil
}

// writeCtx 把 ctx 绑到 QueryOptions（consul/api 的写路径 ctx 载体）。
func writeCtx(ctx context.Context) *api.QueryOptions {
	return (&api.QueryOptions{}).WithContext(ctx)
}

// watcher 是 blocking-query Watcher。
type watcher struct {
	c      *Client
	name   string
	filter registry.Filter
	ctx    context.Context
	ch     chan []registry.Instance // 缓冲 1，最新替换
	done   chan struct{}
	once   sync.Once
	first  atomic.Bool

	mu        sync.Mutex
	lastIndex uint64
}

// Next 首次调用立即查询并返回当前快照（含空列表）；之后阻塞至集合变化、
// ctx 取消或 Stop。
func (w *watcher) Next() ([]registry.Instance, error) {
	if w.first.CompareAndSwap(true, false) {
		instances, meta, err := w.c.query(w.ctx, w.name, w.filter, 0, 0)
		if err != nil {
			return nil, err
		}
		// 顺序论证（RC-10，窗口测试不可行故以确定性顺序保证）：
		// 1) 先排空 ch——排空前 loop 推入的快照内容已包含在本次查询
		//    结果中（其触发变化的 index ≤ meta.LastIndex），丢弃是去重；
		// 2) 再写 lastIndex——排空之后、写入之前 loop 新推入的快照
		//    不会被本次排空波及，保留下一次 Next 消费；
		// 3) lastIndex 写的是本次查询的（可能较小的）index：若第 1 步
		//    丢掉了更新的快照，下一轮 blocking query 会以较小 WaitIndex
		//    立即重新取回该状态，自愈；不会出现「推送被丢且 lastIndex
		//    停在过期值」的永久丢失。
		// 已知边界：index 单调假设被破坏（stale 读到新 index + 旧数据）
		// 时仍可能丢一次推送，仅 allow_stale=true 且极小概率，接受。
		w.mu.Lock()
		select {
		case <-w.ch:
		default:
		}
		w.lastIndex = meta.LastIndex
		w.mu.Unlock()
		return instances, nil
	}
	select {
	case instances := <-w.ch:
		return instances, nil
	case <-w.ctx.Done():
		return nil, w.ctx.Err()
	case <-w.done:
		return nil, errWatcherStopped
	}
}

// Stop 停止 Watcher 并从 Client 注销；幂等，返回 nil。
func (w *watcher) Stop() error {
	w.once.Do(func() {
		close(w.done)
		w.c.mu.Lock()
		delete(w.c.watchers, w)
		w.c.mu.Unlock()
	})
	return nil
}

// loop 执行 blocking query：WaitIndex 推进，每次返回即推送（Consul 仅在
// 集合变化或超时后返回）；错误按 1s–30s 指数退避重连。
func (w *watcher) loop() {
	backoff := watchBackoffMin
	for {
		select {
		case <-w.ctx.Done():
			return
		case <-w.done:
			return
		default:
		}

		w.mu.Lock()
		index := w.lastIndex
		w.mu.Unlock()

		instances, meta, err := w.c.query(w.ctx, w.name, w.filter, index, watchWaitTime)
		if err != nil {
			if w.ctx.Err() != nil {
				return
			}
			timer := time.NewTimer(backoff)
			select {
			case <-w.done:
				timer.Stop()
				return
			case <-timer.C:
			}
			backoff = min(backoff*2, watchBackoffMax)
			continue
		}
		backoff = watchBackoffMin

		w.mu.Lock()
		// index 回绕 sanity check（RC-05，Consul 官方 blocking query 模式）：
		// Raft index 回退（leader 变更 / 快照恢复）后，以过期的 WaitIndex
		// 查询会整轮阻塞到 WaitTime 超时、且推重复快照。检测到
		// LastIndex < 本次使用的 WaitIndex 时重置为 0 立即重查（不 sleep）。
		if meta.LastIndex < index {
			w.lastIndex = 0
			w.mu.Unlock()
			continue
		}
		if meta.LastIndex == w.lastIndex {
			w.mu.Unlock()
			continue // WaitTime 超时返回同 index：无变化，不推送
		}
		w.lastIndex = meta.LastIndex
		w.mu.Unlock()
		select {
		case w.ch <- instances:
		default:
			select {
			case <-w.ch:
			default:
			}
			w.ch <- instances
		}
	}
}

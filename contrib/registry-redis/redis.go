// Package registryredis 提供服务注册发现的 Redis 生产后端：*Client 同时实现
// registry.Registry 与 registry.Discovery。存储模型（全部键携带 key_prefix
// 前缀，默认 lynx/registry/）：
//
//	instance/{service}/{id}  JSON 编码的 registry.Instance，TTL = heartbeat_ttl
//	                          （键过期即下线，崩溃实例无需人工清理）
//	service/{service}        实例 ID 的 SET（索引）；过期成员由读路径惰性 SREM
//	events/{service}         Pub/Sub 频道，Register/Deregister 后发一条通知
//
// Heartbeat 经 Lua 脚本 PEXPIRE 续命，实例键已过期时返回
// registry.ErrNotRegistered（与 Consul TTL check 失败同语义：Registrar 的心跳
// 循环只续命不重注册，持续失败由 readiness 以 ErrHeartbeatFailed 暴露）。
// Watch 订阅服务频道，消息到达即重拉全量快照提交；另按 watch_fallback_interval
// （默认 15s）兜底重拉，覆盖 Pub/Sub 的两个盲区——键静默过期不发事件、订阅
// 断线丢消息。快照规范相等去重由 registry.WatcherSession 承载。
//
// 导入建议：import registryredis "github.com/lynx-go/lynx/contrib/registry-redis"
package registryredis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/lynx-go/lynx/contrib/registry"
	"github.com/redis/go-redis/v9"
)

const (
	// defaultKeyPrefix 是键与频道的命名空间前缀。多套应用共享同一 Redis 时
	// 应配置不同 key_prefix，避免目录互相污染。
	defaultKeyPrefix = "lynx/registry/"
	// defaultTTL 是实例键的 TTL，对齐 consul 后端的 defaultHeartbeatTTL。
	// 必须大于 Registrar 的心跳间隔（registry.NewFromConfig 侧交叉校验）。
	defaultTTL = 30 * time.Second
	// defaultWatchFallbackInterval 是 Watch 兜底重拉间隔，对齐 Resolver 的
	// 轮询回退节奏（15s）。
	defaultWatchFallbackInterval = 15 * time.Second
	// watchQueryTimeout 是 Watcher 单次重拉快照的超时预算。
	watchQueryTimeout = 3 * time.Second
)

// expireScript 条件续命：键存在则 PEXPIRE 返回 1，否则返回 0（实例已下线）。
// 用 PEXPIRE 而非 EXPIRE：测试与紧凑配置会用亚秒级 TTL。
const expireScript = `if redis.call("PEXPIRE", KEYS[1], ARGV[1]) == 1 then return 1 else return 0 end`

// 事件载荷仅用于排障（op:id）；Watcher 收到消息后重拉全量快照，不解析载荷。
const (
	eventRegister   = "register"
	eventDeregister = "deregister"
)

// Option 配置 Client 的构造行为（函数式选项）。
type Option func(*Client)

// WithKeyPrefix 设置键与频道的前缀（默认 lynx/registry/）。
func WithKeyPrefix(prefix string) Option {
	return func(c *Client) {
		if prefix != "" {
			c.keyPrefix = prefix
		}
	}
}

// WithTTL 设置实例键的 TTL（默认 30s）。必须大于 Registrar 的心跳间隔
// （heartbeat_interval），否则键在两次心跳之间过期，实例在目录里持续闪断；
// 配置路径下该约束由 registry.NewFromConfig 交叉校验。
func WithTTL(ttl time.Duration) Option {
	return func(c *Client) {
		if ttl > 0 {
			c.ttl = ttl
		}
	}
}

// WithWatchFallbackInterval 设置 Watch 兜底重拉间隔（默认 15s）。间隔内
// Pub/Sub 推送失效（键过期 / 订阅断线）时，变更感知延迟不超过该值。
func WithWatchFallbackInterval(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.watchFallback = d
		}
	}
}

// WithLogger 注入内部日志（快照解码失败、兜底清理失败等 Warn）。
// 缺省回退 slog.Default()。
func WithLogger(l *slog.Logger) Option {
	return func(c *Client) {
		if l != nil {
			c.logger = l
		}
	}
}

// Client 是 Redis 后端，同时实现 registry.Registry 与 registry.Discovery。
type Client struct {
	// rdb 需要命令 + 订阅两种能力（Watch 走 Pub/Sub）：*redis.Client /
	// *redis.ClusterClient / *redis.UniversalClient（Sentinel）均满足。
	rdb redis.UniversalClient
	// ownCloser 非 nil（NewFromConfig 自建连接）时 Close 一并关闭；New 注入
	// 客户端时为 nil，连接归调用方（cluster-redis 的「谁构造谁负责」约定）。
	ownCloser io.Closer

	logger        *slog.Logger
	keyPrefix     string
	ttl           time.Duration
	watchFallback time.Duration

	mu       sync.Mutex
	closed   bool
	watchers map[*watcher]struct{}
}

var (
	_ registry.Registry  = (*Client)(nil)
	_ registry.Discovery = (*Client)(nil)
)

// New 用已有 Redis 客户端构造后端。rdb 通常是 *redis.Client（Sentinel /
// Cluster 场景用 *redis.UniversalClient / *redis.ClusterClient，Watch 的
// Pub/Sub 订阅两者都支持）；连接生命周期归调用方，Close 不关闭 rdb。
func New(rdb redis.UniversalClient, opts ...Option) *Client {
	c := &Client{
		rdb:           rdb,
		logger:        slog.Default(),
		keyPrefix:     defaultKeyPrefix,
		ttl:           defaultTTL,
		watchFallback: defaultWatchFallbackInterval,
		watchers:      make(map[*watcher]struct{}),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	return c
}

// Register 注册/更新实例：实例键 SET EX ttl + 索引 SADD + 发布变更事件，
// 同一 pipeline 内按序执行。同 ID 重复注册是 last-write-wins upsert
// （Consul 同语义）。
func (c *Client) Register(ctx context.Context, inst registry.Instance) error {
	if err := c.checkOpen(); err != nil {
		return err
	}
	if inst.Name == "" || inst.ID == "" {
		return registry.ErrBadName
	}
	data, err := json.Marshal(inst)
	if err != nil {
		return fmt.Errorf("registryredis: marshal instance %s/%s: %w", inst.Name, inst.ID, err)
	}
	pipe := c.rdb.Pipeline()
	pipe.Set(ctx, c.instanceKey(inst.Name, inst.ID), data, c.ttl)
	pipe.SAdd(ctx, c.indexKey(inst.Name), inst.ID)
	pipe.Publish(ctx, c.eventChannel(inst.Name), eventRegister+":"+inst.ID)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("registryredis: register %s/%s: %w", inst.Name, inst.ID, err)
	}
	return nil
}

// Deregister 注销实例；DEL/SREM 对不存在的成员天然 no-op，幂等。
func (c *Client) Deregister(ctx context.Context, serviceName, instanceID string) error {
	if err := c.checkOpen(); err != nil {
		return err
	}
	if serviceName == "" || instanceID == "" {
		return registry.ErrBadName
	}
	pipe := c.rdb.Pipeline()
	pipe.Del(ctx, c.instanceKey(serviceName, instanceID))
	pipe.SRem(ctx, c.indexKey(serviceName), instanceID)
	pipe.Publish(ctx, c.eventChannel(serviceName), eventDeregister+":"+instanceID)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("registryredis: deregister %s/%s: %w", serviceName, instanceID, err)
	}
	return nil
}

// Heartbeat 经 Lua 脚本续命实例键 TTL；键已过期（进程长暂停、Redis 清库等）
// 返回 registry.ErrNotRegistered——与 Consul TTL check 失败同语义。
func (c *Client) Heartbeat(ctx context.Context, serviceName, instanceID string) error {
	if err := c.checkOpen(); err != nil {
		return err
	}
	if serviceName == "" || instanceID == "" {
		return registry.ErrBadName
	}
	n, err := c.rdb.Eval(ctx, expireScript,
		[]string{c.instanceKey(serviceName, instanceID)}, c.ttl.Milliseconds()).Int()
	if err != nil {
		return fmt.Errorf("registryredis: heartbeat %s/%s: %w", serviceName, instanceID, err)
	}
	if n == 0 {
		return registry.ErrNotRegistered
	}
	return nil
}

// Close 停止全部 Watcher 并拒绝后续读写；幂等。NewFromConfig 构造（自建
// 连接）时连 Redis 客户端一并关闭；New 注入时不关闭（连接归调用方）。
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
	if c.ownCloser != nil {
		return c.ownCloser.Close()
	}
	return nil
}

// GetService 查询服务全量快照并应用 Filter（后置过滤，与 memory/DNS/Consul
// 返回形状一致）。
func (c *Client) GetService(ctx context.Context, name string, filter registry.Filter) ([]registry.Instance, error) {
	if err := c.checkOpen(); err != nil {
		return nil, err
	}
	if name == "" {
		return nil, registry.ErrBadName
	}
	instances, err := c.query(ctx, name)
	if err != nil {
		return nil, err
	}
	out := make([]registry.Instance, 0, len(instances))
	for _, inst := range instances {
		if registry.MatchFilter(filter, inst) {
			out = append(out, inst)
		}
	}
	return out, nil
}

// Watch 返回 Pub/Sub 推送式 Watcher：消息到达或兜底 tick 触发即重拉全量
// 快照提交会话核心（过滤/规范相等/首快照语义由 registry.WatcherSession
// 承载，与 consul 后端的 RunPull 长轮询同骨架）。
func (c *Client) Watch(ctx context.Context, name string, filter registry.Filter) (registry.Watcher, error) {
	if name == "" {
		return nil, registry.ErrBadName
	}
	pubsub := c.rdb.Subscribe(ctx, c.eventChannel(name))
	w := &watcher{c: c, name: name, pubsub: pubsub}
	w.session = registry.NewWatcherSession(ctx, name, filter, func() {
		c.mu.Lock()
		delete(c.watchers, w)
		c.mu.Unlock()
	})
	// closed 检查必须与注册同临界区（RC-09，与 consul 后端一致）：否则落在
	// Close 窗口内的 Watcher 无人停止（goroutine 与订阅泄漏）。
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		_ = pubsub.Close()
		return nil, registry.ErrClosed
	}
	c.watchers[w] = struct{}{}
	c.mu.Unlock()
	go w.loop()
	return w, nil
}

// query 取一次全量快照（GetService 与 Watch 重拉的共享读路径，不做 Filter）：
// SMEMBERS 读索引，pipeline GET 逐个取实例键；nil 成员已过期，惰性 SREM。
func (c *Client) query(ctx context.Context, name string) ([]registry.Instance, error) {
	ids, err := c.rdb.SMembers(ctx, c.indexKey(name)).Result()
	if err != nil {
		return nil, fmt.Errorf("registryredis: smembers %s: %w", name, err)
	}
	if len(ids) == 0 {
		return nil, nil
	}
	pipe := c.rdb.Pipeline()
	gets := make([]*redis.StringCmd, len(ids))
	for i, id := range ids {
		gets[i] = pipe.Get(ctx, c.instanceKey(name, id))
	}
	// 实例键过期在 pipeline 里以 redis.Nil 冒出（v9 的 Exec 会把首个命令错误
	// 透传，包括 Nil），此处与逐命令分支都按「已过期」容忍。
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, fmt.Errorf("registryredis: pipeline get %s: %w", name, err)
	}
	var expired []string
	instances := make([]registry.Instance, 0, len(ids))
	for i, get := range gets {
		data, err := get.Bytes()
		if errors.Is(err, redis.Nil) {
			expired = append(expired, ids[i])
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("registryredis: get %s/%s: %w", name, ids[i], err)
		}
		var inst registry.Instance
		if err := json.Unmarshal(data, &inst); err != nil {
			// 解码失败必须 Warn 并移出索引（RC-17 对齐）：静默跳过会让实例
			// 无痕消失且每次读重复失败；移出后重注册即可恢复。
			c.logger.Warn("registryredis: instance decode failed, removing from index",
				"service", name, "id", ids[i], "error", err)
			expired = append(expired, ids[i])
			continue
		}
		instances = append(instances, inst)
	}
	if len(expired) > 0 {
		// 尽力而为的惰性清理：失败只意味着下次读再清一次，不影响本次结果。
		if err := c.rdb.SRem(ctx, c.indexKey(name), toAny(expired)).Err(); err != nil {
			c.logger.Warn("registryredis: lazy index cleanup failed", "service", name, "error", err)
		}
	}
	return instances, nil
}

func (c *Client) checkOpen() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return registry.ErrClosed
	}
	return nil
}

func (c *Client) instanceKey(service, id string) string {
	return c.keyPrefix + "instance/" + service + "/" + id
}

func (c *Client) indexKey(service string) string {
	return c.keyPrefix + "service/" + service
}

func (c *Client) eventChannel(service string) string {
	return c.keyPrefix + "events/" + service
}

// toAny 展开 []string 为 []any（SRem 变参用），避免依赖驱动对切片成员的
// 隐式展平行为。
func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// watcher 是 Pub/Sub 推送式 Watcher：订阅服务频道，消息到达或兜底 tick
// 重拉快照提交会话核心。
type watcher struct {
	c       *Client
	name    string
	session *registry.WatcherSession

	pubsub *redis.PubSub
}

// Next 首次调用立即查询并返回当前快照（含空列表）；之后阻塞至集合变化、
// ctx 取消或 Stop。
func (w *watcher) Next() ([]registry.Instance, error) {
	return w.session.Next(w.firstSnapshot)
}

// firstSnapshot 是首次 Next 的查询闭包。无需 Drain：会话核心保证基线建立
// 前的 Submit 只更新相等基线、不推送（watcher.go），因此首次查询时通道里
// 不可能有积压推送；查询期间到达的 Submit 由基线去重或兜底 tick 自愈，
// 不存在「推送被丢且无后续事件」的永久丢失。
func (w *watcher) firstSnapshot(ctx context.Context) ([]registry.Instance, error) {
	return w.c.query(ctx, w.name)
}

// Stop 停止 Watcher 并从 Client 注销；幂等，返回 nil。
func (w *watcher) Stop() error {
	return w.session.Stop()
}

// loop 是 Watcher 的推送循环（Watch 启动）：消息到达或兜底 tick 重拉快照；
// Stop / ctx 取消退出并关闭订阅。订阅断线（消息通道关闭）不退出——退化为
// tick 驱动并在下个 tick 重建订阅，Pub/Sub 丢消息由兜底重拉兜住。
func (w *watcher) loop() {
	pubsub := w.pubsub
	msgCh := pubsub.Channel()
	ticker := time.NewTicker(w.c.watchFallback)
	defer ticker.Stop()
	defer func() { _ = pubsub.Close() }()
	for {
		select {
		case <-w.session.Done():
			return
		case <-w.session.Ctx().Done():
			return
		case _, ok := <-msgCh:
			if !ok {
				msgCh = nil // 关闭的通道在 select 里恒就绪，必须摘除防忙转
				continue
			}
			w.refresh()
		case <-ticker.C:
			if msgCh == nil {
				old := pubsub
				pubsub = w.c.rdb.Subscribe(w.session.Ctx(), w.c.eventChannel(w.name))
				msgCh = pubsub.Channel()
				_ = old.Close()
			}
			w.refresh()
		}
	}
}

// refresh 重拉一次全量快照并提交（规范相等由会话核心抑制重复推送）；查询
// 失败只 Warn 不退出（瞬时网络抖动），等下一次消息/tick 重试。
func (w *watcher) refresh() {
	if w.stopped() || w.session.Ctx().Err() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(w.session.Ctx(), watchQueryTimeout)
	defer cancel()
	instances, err := w.c.query(ctx, w.name)
	if err != nil {
		w.c.logger.Warn("registryredis: watch refresh failed", "service", w.name, "error", err)
		return
	}
	w.session.Submit(instances)
}

func (w *watcher) stopped() bool {
	select {
	case <-w.session.Done():
		return true
	default:
		return false
	}
}

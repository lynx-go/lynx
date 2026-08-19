package registry

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultDNSDomain       = "svc.cluster.local"
	defaultDNSNamespace    = "default"
	defaultDNSPollInterval = 15 * time.Second

	// negativeCacheMin / negativeCacheMax 是 NXDOMAIN 负缓存与查询失败后
	// 重试间隔的钳制区间（设计文档：钳制在 [5s, 30s]）。
	negativeCacheMin = 5 * time.Second
	negativeCacheMax = 30 * time.Second
)

// defaultDNSPorts 是无 SRV 记录时按协议补端口的缺省表。
var defaultDNSPorts = map[string]int{
	ProtocolHTTP:  8080,
	ProtocolHTTPS: 8443,
	ProtocolGRPC:  9090,
}

// lookupResolver 是 net.Resolver 的方法子集，便于测试注入 fake。
type lookupResolver interface {
	LookupSRV(ctx context.Context, service, proto, name string) (cname string, addrs []*net.SRV, err error)
	LookupHost(ctx context.Context, host string) ([]string, error)
}

// DNSOption 配置 DNS Discovery。
type DNSOption func(*dnsDiscovery)

// WithDNSDomain 设置查询域（默认 svc.cluster.local）。
func WithDNSDomain(domain string) DNSOption {
	return func(d *dnsDiscovery) {
		if domain != "" {
			d.domain = domain
		}
	}
}

// WithDNSNamespace 设置命名空间（默认 default）。
func WithDNSNamespace(namespace string) DNSOption {
	return func(d *dnsDiscovery) {
		if namespace != "" {
			d.namespace = namespace
		}
	}
}

// WithDNSPorts 覆盖无 SRV 时按协议补的端口表（在默认值上逐项覆盖）。
func WithDNSPorts(ports map[string]int) DNSOption {
	return func(d *dnsDiscovery) {
		for protocol, port := range ports {
			if port > 0 {
				d.ports[protocol] = port
			}
		}
	}
}

// WithDNSPollInterval 设置 Watch 的轮询间隔（默认 15s）。
func WithDNSPollInterval(interval time.Duration) DNSOption {
	return func(d *dnsDiscovery) {
		if interval > 0 {
			d.pollInterval = interval
		}
	}
}

// withDNSLookup 注入 DNS 查询实现（默认 net.DefaultResolver），仅测试使用。
func withDNSLookup(l lookupResolver) DNSOption {
	return func(d *dnsDiscovery) {
		if l != nil {
			d.lookup = l
		}
	}
}

// dnsDiscovery 是 DNS 后端：只实现 Discovery（不写目录）。
//
// 适用场景：K8s headless Service（clusterIP: None）——每个 Pod 一条 A
// 记录，Picker 才有意义。ClusterIP Service 只有一条 VIP A 记录，
// kube-proxy 已做负载均衡，Resolver+Picker 冗余，直接拨
// `http://{service}` 即可，不必开 DNS Discovery。
type dnsDiscovery struct {
	domain       string
	namespace    string
	ports        map[string]int
	pollInterval time.Duration
	lookup       lookupResolver
}

var _ Discovery = (*dnsDiscovery)(nil)

// NewDNSDiscovery 构造 DNS Discovery。查询名 {name}.{namespace}.{domain}；
// 端口先查 SRV（按 Filter.Protocol 选 _http/_https/_grpc 服务标签），无
// SRV 再查 A/AAAA 并按 ports 表补端口。实例无 version/tag/weight，状态
// 一律 StatusPassing（IncludeUnhealthy 对 DNS 无意义）。
func NewDNSDiscovery(opts ...DNSOption) Discovery {
	d := &dnsDiscovery{
		domain:       defaultDNSDomain,
		namespace:    defaultDNSNamespace,
		ports:        make(map[string]int, len(defaultDNSPorts)),
		pollInterval: defaultDNSPollInterval,
		lookup:       net.DefaultResolver,
	}
	for protocol, port := range defaultDNSPorts {
		d.ports[protocol] = port
	}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

// GetService 解析一次并返回当前快照；服务无记录（NXDOMAIN）时返回空切片
// 与 nil（DNS 无法区分「服务名不存在」与「零实例」）。
func (d *dnsDiscovery) GetService(ctx context.Context, name string, filter Filter) ([]Instance, error) {
	if name == "" {
		return nil, ErrBadName
	}
	instances, err := d.resolve(ctx, name, filter)
	if isNotFound(err) {
		return []Instance{}, nil
	}
	if err != nil {
		return nil, err
	}
	return instances, nil
}

// Watch 返回轮询式 Watcher：首次 Next 立即解析并推送当前快照（含空
// 列表），之后每 poll_interval 重新解析，集合变化才推送；NXDOMAIN 推送
// 空快照并按负缓存钳制 [5s, 30s] 放慢轮询，其它查询错误保留旧快照。
func (d *dnsDiscovery) Watch(ctx context.Context, name string, filter Filter) (Watcher, error) {
	if name == "" {
		return nil, ErrBadName
	}
	w := &dnsWatcher{
		d:      d,
		name:   name,
		filter: filter,
		ctx:    ctx,
		ch:     make(chan []Instance, 1),
		done:   make(chan struct{}),
	}
	w.first.Store(true)
	go w.loop()
	return w, nil
}

// queryName 拼接 {name}.{namespace}.{domain}（跳过空段）。
func (d *dnsDiscovery) queryName(name string) string {
	parts := make([]string, 0, 3)
	for _, p := range []string{name, d.namespace, d.domain} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, ".")
}

// protocols 返回本次解析要查的协议集合：Filter.Protocol 非空则只查它，
// 否则查端口表中的全部协议。
func (d *dnsDiscovery) protocols(filter Filter) []string {
	if filter.Protocol != "" {
		return []string{filter.Protocol}
	}
	out := make([]string, 0, len(d.ports))
	for protocol := range d.ports {
		out = append(out, protocol)
	}
	slices.Sort(out)
	return out
}

// resolve 执行一次完整解析：SRV 优先，无 SRV（NXDOMAIN）回落 A/AAAA +
// 端口表。返回的实例按 ID 排序，保证快照可比较。结果再过一遍
// matchFilter：DNS 实例无 Tags，带 Tags 的 Filter 恒不匹配（与 memory
// 后端语义一致）。
func (d *dnsDiscovery) resolve(ctx context.Context, name string, filter Filter) ([]Instance, error) {
	instances, err := d.resolveRaw(ctx, name, filter)
	if err != nil {
		return nil, err
	}
	out := instances[:0]
	for _, inst := range instances {
		if matchFilter(inst, filter) {
			out = append(out, inst)
		}
	}
	return out, nil
}

// resolveRaw 是不带 matchFilter 的解析实现。
func (d *dnsDiscovery) resolveRaw(ctx context.Context, name string, filter Filter) ([]Instance, error) {
	qname := d.queryName(name)
	protocols := d.protocols(filter)

	instances, err := d.resolveSRV(ctx, qname, protocols)
	if err == nil {
		return instances, nil
	}
	if !isNotFound(err) {
		return nil, err
	}
	return d.resolveHost(ctx, name, qname, protocols)
}

// resolveSRV 逐协议查 _{protocol}._tcp.{qname}；任一协议命中即采用 SRV
// 结果（按 target 分组为实例）。全部 NXDOMAIN 时返回 not-found 错误。
func (d *dnsDiscovery) resolveSRV(ctx context.Context, qname string, protocols []string) ([]Instance, error) {
	byTarget := make(map[string][]Endpoint)
	found := false
	var lastErr error
	for _, protocol := range protocols {
		_, records, err := d.lookup.LookupSRV(ctx, "_"+protocol, "tcp", qname)
		if err != nil {
			if !isNotFound(err) {
				lastErr = err
			}
			continue
		}
		for _, rec := range records {
			target := strings.TrimSuffix(rec.Target, ".")
			byTarget[target] = append(byTarget[target], Endpoint{
				Protocol: protocol,
				Address:  net.JoinHostPort(target, fmt.Sprintf("%d", rec.Port)),
			})
			found = true
		}
	}
	if !found {
		if lastErr != nil {
			return nil, lastErr
		}
		return nil, errDNSNotFound
	}
	targets := make([]string, 0, len(byTarget))
	for target := range byTarget {
		targets = append(targets, target)
	}
	slices.Sort(targets)
	instances := make([]Instance, 0, len(targets))
	for _, target := range targets {
		endpoints := byTarget[target]
		slices.SortFunc(endpoints, func(a, b Endpoint) int {
			return strings.Compare(a.Protocol+a.Address, b.Protocol+b.Address)
		})
		instances = append(instances, Instance{
			Name:      qname,
			ID:        target,
			Endpoints: endpoints,
			Status:    StatusPassing,
		})
	}
	return instances, nil
}

// resolveHost 查 A/AAAA，每个 IP 一条实例，端口来自端口表：一条 A 记录
// + 多协议 = 多条 Endpoint（同 host 不同 port）。
func (d *dnsDiscovery) resolveHost(ctx context.Context, name, qname string, protocols []string) ([]Instance, error) {
	ips, err := d.lookup.LookupHost(ctx, qname)
	if err != nil {
		return nil, err
	}
	slices.Sort(ips)
	instances := make([]Instance, 0, len(ips))
	for _, ip := range ips {
		endpoints := make([]Endpoint, 0, len(protocols))
		for _, protocol := range protocols {
			port, ok := d.ports[protocol]
			if !ok {
				continue // 未知协议无端口映射，跳过
			}
			endpoints = append(endpoints, Endpoint{
				Protocol: protocol,
				Address:  net.JoinHostPort(ip, fmt.Sprintf("%d", port)),
			})
		}
		if len(endpoints) == 0 {
			continue
		}
		instances = append(instances, Instance{
			Name:      name,
			ID:        ip,
			Endpoints: endpoints,
			Status:    StatusPassing,
		})
	}
	return instances, nil
}

// errDNSNotFound 是 SRV 全部未命中时的内部 sentinel，与 *net.DNSError
// 的 IsNotFound 同样走负缓存路径。
var errDNSNotFound = errors.New("registry: dns name not found")

// isNotFound 判定 DNS NXDOMAIN / 无记录。
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errDNSNotFound) {
		return true
	}
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr) && dnsErr.IsNotFound
}

// negativeCacheDelay 把失败后的重试间隔钳制到 [5s, 30s]。
func negativeCacheDelay(d time.Duration) time.Duration {
	return min(max(d, negativeCacheMin), negativeCacheMax)
}

// equalDNSSnapshots 比较两份 DNS 快照（实例已由 resolve 排序，DNS 实例
// 无 Tags/Meta/Weight，比较 Name/ID/Status/Endpoints 即可）。
func equalDNSSnapshots(a, b []Instance) bool {
	return slices.EqualFunc(a, b, func(x, y Instance) bool {
		return x.Name == y.Name && x.ID == y.ID && x.Status == y.Status &&
			slices.Equal(x.Endpoints, y.Endpoints)
	})
}

// dnsWatcher 是轮询式 Watcher（Watch = poll）。
type dnsWatcher struct {
	d      *dnsDiscovery
	name   string
	filter Filter
	ctx    context.Context
	ch     chan []Instance // 缓冲 1，最新替换
	done   chan struct{}
	once   sync.Once
	first  atomic.Bool

	mu   sync.Mutex
	last []Instance // 已投递的最新快照（首快照或轮询推送）
}

// Next 首次调用立即解析并返回当前快照（含空列表）；之后阻塞至集合变化、
// ctx 取消或 Stop。
func (w *dnsWatcher) Next() ([]Instance, error) {
	if w.first.CompareAndSwap(true, false) {
		snap, err := w.d.resolve(w.ctx, w.name, w.filter)
		if err != nil && !isNotFound(err) {
			return nil, err
		}
		if isNotFound(err) {
			snap = []Instance{}
		}
		w.mu.Lock()
		w.last = snap
		// 排空首快照前轮询 goroutine 可能已推入的重复快照。
		select {
		case <-w.ch:
		default:
		}
		w.mu.Unlock()
		return snap, nil
	}
	select {
	case snap := <-w.ch:
		return snap, nil
	case <-w.ctx.Done():
		return nil, w.ctx.Err()
	case <-w.done:
		return nil, errWatcherStopped
	}
}

// Stop 停止轮询；幂等，返回 nil。
func (w *dnsWatcher) Stop() error {
	w.once.Do(func() { close(w.done) })
	return nil
}

// loop 按 poll_interval 轮询：变化才推送；NXDOMAIN 推空快照并放慢到
// 负缓存钳制区间；其它查询错误保留旧快照、不推送。
func (w *dnsWatcher) loop() {
	delay := w.d.pollInterval
	for {
		timer := time.NewTimer(delay)
		select {
		case <-w.ctx.Done():
			timer.Stop()
			return
		case <-w.done:
			timer.Stop()
			return
		case <-timer.C:
		}

		snap, err := w.d.resolve(w.ctx, w.name, w.filter)
		notFound := isNotFound(err)
		if err != nil && !notFound {
			// 查询失败：保留旧快照，负缓存钳制后重试。
			delay = negativeCacheDelay(w.d.pollInterval)
			continue
		}
		if notFound {
			snap = []Instance{}
			delay = negativeCacheDelay(w.d.pollInterval)
		} else {
			delay = w.d.pollInterval
		}

		w.mu.Lock()
		if !equalDNSSnapshots(w.last, snap) {
			w.last = snap
			select {
			case w.ch <- snap:
			default:
				select {
				case <-w.ch:
				default:
				}
				w.ch <- snap
			}
		}
		w.mu.Unlock()
	}
}

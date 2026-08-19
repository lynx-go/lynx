package registry

import "errors"

var (
	// ErrNoInstance 表示过滤后没有可用实例（含空列表 Pick、Get 为空）。
	ErrNoInstance = errors.New("registry: no healthy instance")
	// ErrBadName 表示服务名为空或非法。
	ErrBadName = errors.New("registry: empty or invalid service name")
	// ErrNotRegistered 表示实例尚未成功注册或已注销（含 Stop 之后）。
	ErrNotRegistered = errors.New("registry: instance not registered")
	// ErrHeartbeatFailed 表示心跳连续失败达到阈值（3 次）。
	ErrHeartbeatFailed = errors.New("registry: heartbeat failed")
)

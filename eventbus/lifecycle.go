package eventbus

import "strings"

// LifecycleTopicPrefix 是生命周期 / 组件协同 topic 的强制前缀。
const LifecycleTopicPrefix = "lynx."

// IsLifecycleTopic 报告逻辑名是否为 lynx.*（必须走进程内内存 Transport）。
func IsLifecycleTopic(name string) bool {
	return strings.HasPrefix(name, LifecycleTopicPrefix)
}

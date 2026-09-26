package eventbus

import (
	"github.com/lynx-go/lynx/logging"
)

// isPropagationField 报告日志字段是否属于请求标识白名单（request_id /
// user_id）：其值需过共享校验（internal/propagation），非法值不传播/还原。
// 自定义 PropagateAttrs 键不做校验（信任边界不同，见 docs/05）。
func isPropagationField(key string) bool {
	return key == logging.FieldRequestID || key == logging.FieldUserID
}

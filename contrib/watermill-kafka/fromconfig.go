package kafka

import (
	"fmt"
	"strings"

	"github.com/lynx-go/lynx"
)

// NewFromConfig 从配置 "kafka" 段加载 Options 并创建 Transport。
// 段缺失或为空（无任何 topic）时返回 (nil, nil)，表示 Kafka 未启用；
// 调用方据此决定是否注册。段内字段类型非法（如 brokers 写成标量）时
// 返回错误。
func NewFromConfig(cfg lynx.Config) (*Transport, error) {
	// 类型校验需在弱类型反序列化之前做：mapstructure 会把 brokers: 42
	// 这类类型错误静默弱转为 []string{"42"}，此处从原始配置捕获。
	if err := validateKafkaSection(cfg.Get("kafka")); err != nil {
		return nil, err
	}
	var opts Options
	if err := cfg.UnmarshalKey("kafka", &opts); err != nil {
		return nil, err
	}
	if len(opts.Topics) == 0 {
		return nil, nil
	}
	return NewTransport(opts)
}

// validateKafkaSection 轻量校验 kafka 段的字段结构类型，拒绝 mapstructure
// 弱类型转换会静默掩盖的类型错误。仅在 cfg.Get 返回映射时生效；非映射
// 输入（如经 env 注入的扁平键）跳过，以 UnmarshalKey 结果为准。值为 nil
// 的字段（YAML null）视为未设置，跳过校验。列表的元素级类型不逐项检查：
// brokers: [42] 与标量形式同样弱转为字符串，为已知边界。
func validateKafkaSection(raw any) error {
	section, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	for topic, v := range section {
		tm, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("kafka: topic %q must be a mapping, got %T", topic, v)
		}
		for _, field := range []string{"brokers", "topics"} {
			fv, ok := foldGet(tm, field)
			// YAML null 与字段缺省等价（unmarshal 后 slice/指针保持 nil），
			// 视为未设置跳过；必填校验由 NewTransport/Init 负责。
			if !ok || fv == nil {
				continue
			}
			if !isStringList(fv) {
				return fmt.Errorf("kafka: topic %q field %q must be a list of strings, got %T", topic, field, fv)
			}
		}
		for _, field := range []string{"consumer", "producer", "sasl", "tls"} {
			fv, ok := foldGet(tm, field)
			// nil 表示 Consumer/Producer 未配置（只发布/只订阅）或 SASL/TLS 关闭。
			if !ok || fv == nil {
				continue
			}
			if _, ok := fv.(map[string]any); !ok {
				return fmt.Errorf("kafka: topic %q field %q must be a mapping, got %T", topic, field, fv)
			}
		}
	}
	return nil
}

// isStringList 判断值是否为字符串列表；字符串本身也接受（env 注入的
// 扁平值或单地址简写，弱类型转换下行为不变）。
func isStringList(v any) bool {
	switch v.(type) {
	case []any, []string, string:
		return true
	}
	return false
}

// foldGet 大小写不敏感地从映射取键（viper 对配置键大小写不敏感）。
func foldGet(m map[string]any, key string) (any, bool) {
	if v, ok := m[key]; ok {
		return v, true
	}
	for k, v := range m {
		if strings.EqualFold(k, key) {
			return v, true
		}
	}
	return nil, false
}

package lynx

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/go-viper/mapstructure/v2"
	"github.com/spf13/viper"
)

// applyUnmarshalOptions 归并选项为设置。
func applyUnmarshalOptions(opts []UnmarshalOption) UnmarshalOptions {
	var o UnmarshalOptions
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// viperOpts 把选项翻译为 viper 的 DecoderConfigOption（非结构体目标的
// 回落路径用）。StrictTypes 在既有钩子链之前追加拒绝钩子；ErrorUnused
// 透传 mapstructure.ErrorUnused。
func (o UnmarshalOptions) viperOpts() []viper.DecoderConfigOption {
	var opts []viper.DecoderConfigOption
	if o.TagName != "" {
		opts = append(opts, func(dc *mapstructure.DecoderConfig) {
			dc.TagName = o.TagName
		})
	}
	if o.StrictTypes {
		opts = append(opts, func(dc *mapstructure.DecoderConfig) {
			if dc.DecodeHook != nil {
				dc.DecodeHook = mapstructure.ComposeDecodeHookFunc(strictCollectionHook(), dc.DecodeHook)
			} else {
				dc.DecodeHook = strictCollectionHook()
			}
		})
	}
	if o.ErrorUnused {
		opts = append(opts, func(dc *mapstructure.DecoderConfig) {
			dc.ErrorUnused = true
		})
	}
	return opts
}

// strictCollectionHook 实现 WithStrictTypes：源为整数/浮点/布尔且目标为
// 切片/映射时报错（如 brokers: 42 → []string 的静默弱转错配）。字符串
// 来源不在此列——环境变量注入的值恒为字符串，"a,b" → 切表（逗号钩子）、
// "42" → int（弱转）均为合法路径，拒绝字符串会使 env 覆盖不可用。
func strictCollectionHook() mapstructure.DecodeHookFunc {
	return func(f reflect.Type, t reflect.Type, data any) (any, error) {
		switch f.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
			reflect.Float32, reflect.Float64, reflect.Bool:
			if t.Kind() == reflect.Slice || t.Kind() == reflect.Map {
				return nil, fmt.Errorf("lynx: cannot decode %s into %s: non-string scalar to collection（检查配置值的类型）", f, t)
			}
		}
		return data, nil
	}
}

// structTypeOf 返回 out 解引用后的结构体类型；out 不指向结构体时返回 nil。
func structTypeOf(out any) reflect.Type {
	t := reflect.TypeOf(out)
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct {
		return nil
	}
	return t
}

// structElemType 返回 t 解引用后的类型；非结构体返回 nil。
func structElemType(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	return t
}

// unmarshalByStruct 以 out 的结构体叶子为键集逐键 Get 取值，组装为按字段名
// 键控的嵌套 map 后经 mapstructure 解码，确保仅在环境变量中设置的键也参与
// 解码（v1.12 起为结构体目标的默认路径，原 WithEnvForAllKeys 选项转正）。
// prefix 非空时叶子键以其为前缀（UnmarshalKey）。解码语义对齐 viper 默认
// （WeaklyTypedInput + duration/逗号切分钩子；WithStrictTypes 追加拒绝钩子）。
func unmarshalByStruct(c Config, prefix string, out any, o UnmarshalOptions) error {
	tree := make(map[string]any)
	if err := collectEnvLeaves(reflect.TypeOf(out), prefix, c, o, tree); err != nil {
		return err
	}

	// 树按字段名键控；TagName 取内部保留值使 mapstructure 对所有字段
	// 一律按字段名（大小写不敏感）匹配，与各字段 tag 的选择解耦。
	// 结构体容器字段的原始子树已经 decodeRawContainer 预解码（mapstructure
	// 语义），此处对类型化值透传。
	decoder, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		Result:           out,
		TagName:          "lynx.field",
		WeaklyTypedInput: true,
		DecodeHook:       decodeHooks(o),
	})
	if err != nil {
		return err
	}
	if err := decoder.Decode(tree); err != nil {
		return err
	}
	if o.ErrorUnused {
		return checkUnusedKeys(c, prefix, reflect.TypeOf(out), o)
	}
	return nil
}

// checkUnusedKeys 实现 WithErrorUnused：报告 prefix 子树中未被目标结构体
// 消费的键。配置侧经 c.Get(prefix) 取原始映射（仅文件键——env-only 键
// 由结构体侧逐叶询问，不存在"未使用"的 env-only 键；扁平值无从枚举，
// 跳过）。比较大小写不敏感（viper 对配置键如此），内联匿名字段参与同层
// 匹配；动态键的 map/切片/叶子字段整体消费其子树。
func checkUnusedKeys(c Config, prefix string, t reflect.Type, o UnmarshalOptions) error {
	raw := c.Get(prefix)
	if raw == nil {
		return nil
	}
	section, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	var unused []string
	walkUnusedKeys(section, t, o, prefix, &unused)
	if len(unused) == 0 {
		return nil
	}
	sort.Strings(unused)
	return fmt.Errorf("lynx: 配置段 %q 存在未被目标结构体使用的键: %s（未知键多为拼写错误，或结构体缺字段）", prefix, strings.Join(unused, ", "))
}

func walkUnusedKeys(section map[string]any, t reflect.Type, o UnmarshalOptions, path string, unused *[]string) {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct {
		return
	}
	for key, val := range section {
		if val == nil {
			// YAML null 与未设置等价（对齐既有垫片语义）。
			continue
		}
		f, consumed := structConsumesKey(t, key, o.TagName)
		if !consumed {
			*unused = append(*unused, joinKeyPath(path, key))
			continue
		}
		// 嵌套结构体且配置值为映射 → 下钻；其余（remain/map/切片/叶子/
		// 无导出字段的结构体如 time.Duration）整体消费。
		if f.Name != "" {
			if ft := structElemType(f.Type); ft != nil && hasExportedFields(ft) {
				if sub, ok := val.(map[string]any); ok {
					walkUnusedKeys(sub, ft, o, joinKeyPath(path, key), unused)
				}
			}
		}
	}
}

// structConsumesKey 报告配置键是否被结构体某字段消费（键名匹配或落入
// remain 字段）；内联匿名字段参与同层匹配。键名语义与 structKey 一致。
func structConsumesKey(t reflect.Type, key string, tagName string) (reflect.StructField, bool) {
	sawRemain := false
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, inline, remain := structKey(f, tagName)
		if name == "-" {
			continue
		}
		if remain {
			sawRemain = true
			continue
		}
		if inline {
			if got, ok := structConsumesKey(f.Type, key, tagName); ok {
				return got, true
			}
			continue
		}
		if strings.EqualFold(name, key) {
			return f, true
		}
	}
	if sawRemain {
		return reflect.StructField{}, true
	}
	return reflect.StructField{}, false
}

func joinKeyPath(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

// collectEnvLeaves 递归枚举 t（应为结构体类型）的字段叶子：结构体字段下钻
// （叶子键按 tag 回退链解析，无导出字段的结构体按叶子处理），其余按叶子
// Get；值非 nil 才写入 tree，键为字段名。tag 为 "-" 的字段跳过；无键名的
// 匿名嵌入内联到当前层。结构体容器字段（map/切片且元素为结构体）经
// decodeRawContainer 按 mapstructure 语义预解码。
func collectEnvLeaves(t reflect.Type, prefix string, c Config, o UnmarshalOptions, tree map[string]any) error {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, inline, remain := structKey(f, o.TagName)
		if name == "-" {
			continue
		}
		if inline {
			if err := collectEnvLeaves(f.Type, prefix, c, o, tree); err != nil {
				return err
			}
			continue
		}
		if remain {
			// remain 字段吞并本层全部剩余键（动态 map，如 kafka 的
			// map[逻辑topic]TopicOptions）：整体取当前层子树，键名与
			// 元素解码交给 mapstructure 语义（见 decodeRawContainer）。
			v, err := rawContainerValue(f.Type, prefix, c, o)
			if err != nil {
				return err
			}
			if v != nil {
				tree[f.Name] = v
			}
			continue
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		if ft := structElemType(f.Type); ft != nil && hasExportedFields(ft) {
			child := make(map[string]any)
			if err := collectEnvLeaves(f.Type, path, c, o, child); err != nil {
				return err
			}
			if len(child) > 0 {
				tree[f.Name] = child
			}
			continue
		}
		if v := c.Get(path); v != nil {
			decoded, err := decodeRawContainer(f.Type, v, o)
			if err != nil {
				return err
			}
			tree[f.Name] = decoded
		}
	}
	return nil
}

func rawContainerValue(ft reflect.Type, prefix string, c Config, o UnmarshalOptions) (any, error) {
	v := c.Get(prefix)
	if v == nil {
		return nil, nil
	}
	return decodeRawContainer(ft, v, o)
}

// decodeRawContainer 处理结构体容器字段的原始子树：map/切片且元素为含
// 导出字段的结构体（如 kafka 的 map[逻辑topic]TopicOptions、watermill 的
// map[string]topicFileConfig、registry 的 []Endpoint）时，元素键是真实
// 配置键，无法按字段名匹配（主解码器的 lynx.field 保留 tag 只覆盖结构体
// 遍历层）——先按 mapstructure 语义（对齐 viper 的嵌套解码）预解出类型化
// 值放入树，主解码器对已类型化的值是透传。其余类型原样返回。
func decodeRawContainer(ft reflect.Type, raw any, o UnmarshalOptions) (any, error) {
	var elem reflect.Type
	switch ft.Kind() {
	case reflect.Map, reflect.Slice:
		elem = ft.Elem()
	default:
		return raw, nil
	}
	if se := structElemType(elem); se == nil || !hasExportedFields(se) {
		return raw, nil
	}
	out := reflect.New(ft)
	dec, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		Result:           out.Interface(),
		TagName:          "mapstructure",
		WeaklyTypedInput: true,
		DecodeHook:       decodeHooks(o),
	})
	if err != nil {
		return nil, err
	}
	if err := dec.Decode(raw); err != nil {
		return nil, err
	}
	return out.Elem().Interface(), nil
}

// decodeHooks 组装两条解码路径共用的钩子链（对齐 viper 默认 + 严格选项）。
func decodeHooks(o UnmarshalOptions) mapstructure.DecodeHookFunc {
	hooks := []mapstructure.DecodeHookFunc{
		mapstructure.StringToTimeDurationHookFunc(),
		stringToWeakSliceHookFunc(","),
	}
	if o.StrictTypes {
		hooks = append(hooks, strictCollectionHook())
	}
	return mapstructure.ComposeDecodeHookFunc(hooks...)
}

// hasExportedFields 报告结构体类型是否有可枚举的导出字段；无导出字段的
// 结构体（如 time.Time）按叶子处理，保留整体取值的可能。
func hasExportedFields(t reflect.Type) bool {
	for i := 0; i < t.NumField(); i++ {
		if t.Field(i).IsExported() {
			return true
		}
	}
	return false
}

// structKey 返回字段对应的配置键段、是否匿名内联、是否 remain（吞并
// 本层全部剩余键的动态 map，如 kafka Options.Topics 的
// mapstructure:",remain"）；"-" 表示跳过。tagName 非空时仅按该 tag 匹配
// （缺 tag 回退小写字段名）；空值按 mapstructure → json → 小写字段名回退。
func structKey(f reflect.StructField, tagName string) (key string, inline, remain bool) {
	tags := []string{tagName}
	if tagName == "" {
		tags = []string{"mapstructure", "json"}
	}
	for _, tn := range tags {
		if tn == "" {
			continue
		}
		tag, ok := f.Tag.Lookup(tn)
		if !ok {
			continue
		}
		name, opt, _ := strings.Cut(tag, ",")
		if name == "-" {
			return "-", false, false
		}
		if name == "" {
			if opt == "remain" {
				return "", false, true
			}
			if opt == "squash" || f.Anonymous {
				return "", true, false
			}
			return strings.ToLower(f.Name), false, false
		}
		return name, false, false
	}
	if f.Anonymous {
		return "", true, false
	}
	return strings.ToLower(f.Name), false, false
}

// stringToWeakSliceHookFunc 镜像 viper 内部的默认钩子：字符串到任意切片的
// 宽松转换（空串 → 空切片，其余按 sep 切分）。
func stringToWeakSliceHookFunc(sep string) mapstructure.DecodeHookFunc {
	return func(f reflect.Type, t reflect.Type, data any) (any, error) {
		if f.Kind() != reflect.String || t.Kind() != reflect.Slice {
			return data, nil
		}
		raw := data.(string)
		if raw == "" {
			return []string{}, nil
		}
		return strings.Split(raw, sep), nil
	}
}

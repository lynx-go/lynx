package lynx

import (
	"reflect"
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

// viperOpts 把选项翻译为 viper 的 DecoderConfigOption（默认路径用）。
func (o UnmarshalOptions) viperOpts() []viper.DecoderConfigOption {
	if o.TagName == "" {
		return nil
	}
	return []viper.DecoderConfigOption{func(dc *mapstructure.DecoderConfig) {
		dc.TagName = o.TagName
	}}
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
// 解码。prefix 非空时叶子键以其为前缀（UnmarshalKey）。解码语义对齐 viper
// 默认（WeaklyTypedInput + duration/逗号切分钩子）。
func unmarshalByStruct(c Config, prefix string, out any, o UnmarshalOptions) error {
	tree := make(map[string]any)
	collectEnvLeaves(reflect.TypeOf(out), prefix, c, o, tree)

	// 树按字段名键控；TagName 取内部保留值使 mapstructure 对所有字段
	// 一律按字段名（大小写不敏感）匹配，与各字段 tag 的选择解耦。
	decoder, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		Result:           out,
		TagName:          "lynx.field",
		WeaklyTypedInput: true,
		DecodeHook: mapstructure.ComposeDecodeHookFunc(
			mapstructure.StringToTimeDurationHookFunc(),
			stringToWeakSliceHookFunc(","),
		),
	})
	if err != nil {
		return err
	}
	return decoder.Decode(tree)
}

// collectEnvLeaves 递归枚举 t（应为结构体类型）的字段叶子：结构体字段下钻
// （叶子键按 tag 回退链解析，无导出字段的结构体按叶子处理），其余按叶子
// Get；值非 nil 才写入 tree，键为字段名。tag 为 "-" 的字段跳过；无键名的
// 匿名嵌入内联到当前层。
func collectEnvLeaves(t reflect.Type, prefix string, c Config, o UnmarshalOptions, tree map[string]any) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, inline := structKey(f, o.TagName)
		if name == "-" {
			continue
		}
		if inline {
			collectEnvLeaves(f.Type, prefix, c, o, tree)
			continue
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		if ft := structElemType(f.Type); ft != nil && hasExportedFields(ft) {
			child := make(map[string]any)
			collectEnvLeaves(f.Type, path, c, o, child)
			if len(child) > 0 {
				tree[f.Name] = child
			}
			continue
		}
		if v := c.Get(path); v != nil {
			tree[f.Name] = v
		}
	}
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

// structKey 返回字段对应的配置键段与是否匿名内联；"-" 表示跳过。
// tagName 非空时仅按该 tag 匹配（缺 tag 回退小写字段名）；空值按
// mapstructure → json → 小写字段名回退。
func structKey(f reflect.StructField, tagName string) (key string, inline bool) {
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
			return "-", false
		}
		if name == "" {
			if opt == "squash" || f.Anonymous {
				return "", true
			}
			return strings.ToLower(f.Name), false
		}
		return name, false
	}
	if f.Anonymous {
		return "", true
	}
	return strings.ToLower(f.Name), false
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

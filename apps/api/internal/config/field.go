package config

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// 按 YAML 路径定位配置字段。
//
// 路径是 yaml tag 的段名序列（如 ["core", "public_url"]），
// 与设置层的 SettingDef.YAMLPath 是同一套——
// 定位规则只有这一份，配置层与设置层不会各写一套而漂移。

var durationType = reflect.TypeOf(time.Duration(0))

// FieldByYAMLPath 在配置里按 YAML 路径定位字段。
//
// 找不到返回 false——调用方必须把它当错误处理，
// 不能当成"这项没配置"就跳过（那会让写错的路径静默失效）。
func FieldByYAMLPath(c *Config, path []string) (reflect.Value, bool) {
	if len(path) == 0 {
		return reflect.Value{}, false
	}
	v := reflect.ValueOf(c).Elem()
	for _, seg := range path {
		if v.Kind() != reflect.Struct {
			return reflect.Value{}, false
		}
		f, ok := fieldByYAMLTag(v, seg)
		if !ok {
			return reflect.Value{}, false
		}
		v = f
	}
	return v, true
}

// fieldByYAMLTag 找结构体里 yaml tag 名为 seg 的字段。
func fieldByYAMLTag(v reflect.Value, seg string) (reflect.Value, bool) {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ",")
		if name == seg {
			return v.Field(i), true
		}
	}
	return reflect.Value{}, false
}

// setFieldFromString 把字符串写进配置字段。
//
// 解析方式由**字段类型**决定（字符串 / 整数 / 布尔 / 时长），
// 不另设一份类型表——类型表会与字段类型漂移。
func setFieldFromString(f reflect.Value, raw string) error {
	switch {
	case f.Type() == durationType:
		d, err := time.ParseDuration(raw)
		if err != nil {
			return fmt.Errorf("无法解析为时长: %w", err)
		}
		f.Set(reflect.ValueOf(d))
	case f.Kind() == reflect.String:
		f.SetString(raw)
	case f.Kind() >= reflect.Int && f.Kind() <= reflect.Int64:
		n, err := strconv.Atoi(raw)
		if err != nil {
			return fmt.Errorf("无法解析为整数: %w", err)
		}
		f.SetInt(int64(n))
	case f.Kind() == reflect.Bool:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return fmt.Errorf("无法解析为布尔值: %w", err)
		}
		f.SetBool(b)
	default:
		return fmt.Errorf("不支持的配置类型 %s", f.Type())
	}
	return nil
}

package service

import (
	"encoding/json"
	"fmt"
	"reflect"
	"time"
)

// 设置项与配置字段之间的双向映射。
//
// 定位靠 SettingDef.YAMLPath 里的 yaml 段名，逐段在 config.Config 上
// 找同名 yaml tag 的字段——**不靠 Go 字段名**。
// 这样"设置项定义"与"配置结构"两处不会漂移：
// 新增一个设置项只要在 AllSettingDefs 里写对 YAMLPath，
// 读、写、导出三边自动跟上。

var (
	durationType    = reflect.TypeOf(time.Duration(0))
	stringSliceType = reflect.TypeOf([]string{})
)

// setFieldValue 把设置值写进配置字段。
//
// 值来自三个地方，形态不一样：HTTP JSON（数字是 float64）、
// DB（可能是 int / int64 / json.Number）、配置文件（字符串）。
// 归一化都在这里做，配置字段只接受最终形态。
func setFieldValue(f reflect.Value, v any) error {
	if f.Type() == durationType {
		return setDurationValue(f, v)
	}
	switch f.Kind() {
	case reflect.String:
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("期望字符串，得到 %T", v)
		}
		f.SetString(s)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := toInt64(v)
		if err != nil {
			return err
		}
		f.SetInt(n)
	case reflect.Bool:
		b, err := toBool(v)
		if err != nil {
			return err
		}
		f.SetBool(b)
	case reflect.Slice:
		if f.Type() != stringSliceType {
			return fmt.Errorf("不支持的设置类型 %s", f.Type())
		}
		list, err := toStringSlice(v)
		if err != nil {
			return err
		}
		f.Set(reflect.ValueOf(list))
	default:
		return fmt.Errorf("不支持的设置类型 %s", f.Type())
	}
	return nil
}

// readFieldValue 读出配置字段的值。
//
// 时长以字符串返回（"1m0s"）——设置项对外的表达形式就是字符串。
func readFieldValue(f reflect.Value) any {
	if f.Type() == durationType {
		return f.Interface().(time.Duration).String()
	}
	return f.Interface()
}

// setDurationValue 写时长字段。
//
// 裸数字按**秒**解释：HTTP JSON 里时长是字符串。
func setDurationValue(f reflect.Value, v any) error {
	switch d := v.(type) {
	case string:
		p, err := time.ParseDuration(d)
		if err != nil {
			return fmt.Errorf("非法时长 %q: %w", d, err)
		}
		f.Set(reflect.ValueOf(p))
	case float64:
		f.Set(reflect.ValueOf(time.Duration(int64(d)) * time.Second))
	case int64:
		f.Set(reflect.ValueOf(time.Duration(d) * time.Second))
	case int:
		f.Set(reflect.ValueOf(time.Duration(d) * time.Second))
	default:
		return fmt.Errorf("期望时长字符串，得到 %T", v)
	}
	return nil
}

// toInt64 把任意来源的数字归一成 int64。
func toInt64(v any) (int64, error) {
	switch n := v.(type) {
	case float64:
		return int64(n), nil
	case int:
		return int64(n), nil
	case int64:
		return n, nil
	case json.Number:
		return n.Int64()
	}
	return 0, fmt.Errorf("期望数字，得到 %T", v)
}

// toBool 把任意来源的布尔值归一成 bool。
func toBool(v any) (bool, error) {
	switch b := v.(type) {
	case bool:
		return b, nil
	case float64:
		return b != 0, nil
	case int:
		return b != 0, nil
	case int64:
		return b != 0, nil
	}
	return false, fmt.Errorf("期望布尔值，得到 %T", v)
}

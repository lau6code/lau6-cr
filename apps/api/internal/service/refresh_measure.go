package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// 镜像的**度量**与 semver 比较：纯计算。
//
// 只用到 Refresher 的两个取数方法（getManifest / getBlob），
// 不涉及队列与刷新状态，所以与 refresh.go 分开放。

// manifestMeasure 是一个 manifest（或 manifest list）的度量结果。
type manifestMeasure struct {
	size      int64
	created   string
	platforms []string
}

func (m *manifestMeasure) add(o manifestMeasure) {
	m.size += o.size
	// created 取"最大"：ISO 8601 字符串按字典序比较即时间序（同一时区格式下成立）。
	if o.created > m.created {
		m.created = o.created
	}
	for _, p := range o.platforms {
		m.addPlatformString(p)
	}
}

func (m *manifestMeasure) addPlatform(p map[string]any) {
	os, _ := p["os"].(string)
	arch, _ := p["architecture"].(string)
	variant, _ := p["variant"].(string)
	if os == "" && arch == "" {
		return
	}
	s := os + "/" + arch
	if variant != "" {
		s += "/" + variant
	}
	m.addPlatformString(s)
}

// 去重是必需的：manifest list 的条目上带 platform，
// 而子 manifest 的 config blob 里也有同一个 platform——
// 两处都记就会重复。
func (m *manifestMeasure) addPlatformString(s string) {
	if s == "" {
		return
	}
	for _, existing := range m.platforms {
		if existing == s {
			return
		}
	}
	m.platforms = append(m.platforms, s)
}

// ⚠️ **必须拉镜像 config blob**：
// 单架构镜像的 manifest 里，`config` 只是**描述符**（mediaType/size/digest），
// `created` 与平台信息都在**镜像 config blob** 里——
// 只读 manifest 的话，created 与平台**永远是空的**。
//
// manifest list（多架构镜像）则要逐个子 manifest 处理，
// 尺寸按子 manifest 累加、created 取最大、平台取并集。
func (r *Refresher) measureManifest(ctx context.Context, full string, m map[string]any) (manifestMeasure, error) {
	res := manifestMeasure{platforms: []string{}}
	if m == nil {
		return res, fmt.Errorf("manifest 为空")
	}
	// manifest list：顶层有 manifests 数组。
	if subs, ok := m["manifests"].([]any); ok {
		for _, s := range subs {
			sm, _ := s.(map[string]any)
			if sm == nil {
				continue
			}
			// 有些 registry 的 list 条目直接带 platform（不指向子 manifest）。
			if p, ok := sm["platform"].(map[string]any); ok {
				res.addPlatform(p)
			}
			digest, _ := sm["digest"].(string)
			if digest == "" {
				continue
			}
			sub, err := r.getManifest(ctx, full, digest)
			if err != nil {
				return res, fmt.Errorf("拉子 manifest %s 失败: %w", digest, err)
			}
			subRes, err := r.measureOne(ctx, full, sub)
			if err != nil {
				return res, err
			}
			res.add(subRes)
		}
		return res, nil
	}
	return r.measureOne(ctx, full, m)
}

func (r *Refresher) measureOne(ctx context.Context, full string, m map[string]any) (manifestMeasure, error) {
	res := manifestMeasure{platforms: []string{}}
	if m == nil {
		return res, fmt.Errorf("manifest 为空")
	}
	cfg, _ := m["config"].(map[string]any)
	if cfg != nil {
		if s, ok := cfg["size"].(float64); ok {
			res.size += int64(s)
		}
	}
	if layers, ok := m["layers"].([]any); ok {
		for _, l := range layers {
			lm, _ := l.(map[string]any)
			if lm == nil {
				continue
			}
			if s, ok := lm["size"].(float64); ok {
				res.size += int64(s)
			}
			if p, ok := lm["platform"].(map[string]any); ok {
				res.addPlatform(p)
			}
		}
	}
	// 平台与创建时间在**镜像 config blob** 里——必须再拉一次。
	if cfg != nil {
		digest, _ := cfg["digest"].(string)
		if digest != "" {
			blob, err := r.getBlob(ctx, full, digest)
			if err != nil {
				return res, fmt.Errorf("拉镜像 config blob 失败: %w", err)
			}
			var img map[string]any
			if err := json.Unmarshal(blob, &img); err != nil {
				return res, fmt.Errorf("解析镜像 config blob 失败: %w", err)
			}
			if c, ok := img["created"].(string); ok && c != "" {
				res.created = c
			}
			p := map[string]any{}
			if os, ok := img["os"].(string); ok {
				p["os"] = os
			}
			if arch, ok := img["architecture"].(string); ok {
				p["architecture"] = arch
			}
			if v, ok := img["variant"].(string); ok {
				p["variant"] = v
			}
			res.addPlatform(p)
		}
	}
	return res, nil
}

// 优先级：latest > 最高 semver > 字典序最大。
// semver 优先于字典序是必须的——字典序会把 v0.0.10 判成比 v0.0.9 小。
func pickRepresentativeTag(tags []string) string {
	if len(tags) == 0 {
		return ""
	}
	has := map[string]bool{}
	for _, t := range tags {
		has[t] = true
	}
	if has["latest"] {
		return "latest"
	}
	best := ""
	bestVer := ""
	for _, t := range tags {
		v := semverOf(t)
		if v == "" {
			continue
		}
		if bestVer == "" || compareSemver(v, bestVer) > 0 {
			best, bestVer = t, v
		}
	}
	if best != "" {
		return best
	}
	for _, t := range tags {
		if t > best {
			best = t
		}
	}
	return best
}

func semverOf(tag string) string {
	v := strings.TrimPrefix(tag, "v")
	if !strings.Contains(v, ".") {
		return ""
	}
	parts := strings.SplitN(v, "-", 2)
	nums := strings.Split(parts[0], ".")
	if len(nums) < 2 || len(nums) > 3 {
		return ""
	}
	for _, n := range nums {
		if n == "" {
			return ""
		}
		for _, c := range n {
			if c < '0' || c > '9' {
				return ""
			}
		}
	}
	out := parts[0]
	if len(parts) == 2 {
		out += "-" + parts[1]
	}
	return out
}

func compareSemver(a, b string) int {
	as := strings.SplitN(a, "-", 2)
	bs := strings.SplitN(b, "-", 2)
	an := strings.Split(as[0], ".")
	bn := strings.Split(bs[0], ".")
	for i := 0; i < 3; i++ {
		var av, bv int
		if i < len(an) {
			av = atoiSafe(an[i])
		}
		if i < len(bn) {
			bv = atoiSafe(bn[i])
		}
		if av != bv {
			if av > bv {
				return 1
			}
			return -1
		}
	}
	switch {
	case len(as) == 2 && len(bs) == 2:
		return strings.Compare(as[1], bs[1])
	case len(as) == 2:
		return 1 // 有预发布标识的版本更小
	case len(bs) == 2:
		return -1
	}
	return 0
}

func atoiSafe(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return n
		}
		n = n*10 + int(c-'0')
	}
	return n
}

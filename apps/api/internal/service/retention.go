package service

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
)

// 保留策略的规则求值。
//
// 设计依据见 docs/design/03-registry.md §5.2。

// RetentionAlgorithmOR 是唯一的组合算法。
//
// 命中任意一条规则即保留；全部规则都不命中才删。
// Harbor 同样只有 OR（03-registry.md §5.1）。
const RetentionAlgorithmOR = "or"

// 跳过原因（写进执行明细）。
const (
	// SkipReasonUnknownTime：推送时间未知，信息不足，保留。
	SkipReasonUnknownTime = "unknown_time"
	// SkipReasonSharedDigest：同一 digest 下还有别的 tag 要保留，整组保留。
	SkipReasonSharedDigest = "shared_digest"
	// SkipReasonProtectedLatest：latest 被豁免。
	SkipReasonProtectedLatest = "protected_latest"
	// SkipReasonNoDigest：拿不到 digest，保留。
	SkipReasonNoDigest = "no_digest"
)

// DeletionCandidate 是一个待删的 manifest（按 digest 聚合）。
type DeletionCandidate struct {
	Digest string
	Tags   []string
	Size   int64
}

// RetentionDecision 是单个仓库的评估结果。
type RetentionDecision struct {
	Repo string
	// Keep 是保留下来的 tag。
	Keep []string
	// Delete 是待删的 manifest 候选（已按 digest 分组）。
	Delete []DeletionCandidate
	// Skipped 是被跳过的条目及原因。
	Skipped []domain.CleanupSkipped
}

// ValidateRetentionPolicy 校验一条策略。
//
// ⚠️ 校验必须在**保存时**做（返回 400），不留到执行期才炸——
// 执行期才发现规则非法，那次清理已经开始了。
func ValidateRetentionPolicy(p *domain.RetentionPolicy) error {
	if p == nil {
		return fmt.Errorf("策略不能为空")
	}
	if p.Algorithm == "" {
		p.Algorithm = RetentionAlgorithmOR
	}
	if p.Algorithm != RetentionAlgorithmOR {
		return fmt.Errorf("未知的组合算法 %q（只支持 %q）", p.Algorithm, RetentionAlgorithmOR)
	}
	if p.TagSelector == "" {
		p.TagSelector = "**"
	}
	if _, err := compileGlob(p.TagSelector); err != nil {
		return fmt.Errorf("策略级 tag 选择器非法: %w", err)
	}
	if len(p.Rules) == 0 {
		// 空规则列表**不是错误**——它表示"这条策略什么都不删"（§5.2.4 ④，fail-safe）。
		// enabled=true 却一条规则都没有，多半是用户配错了；
		// 但空策略本来就删不掉任何东西，拦下来也换不来什么。
		return nil
	}
	for i := range p.Rules {
		r := &p.Rules[i]
		switch r.Template {
		case domain.RuleKeepAlways:
			if len(r.Params) > 0 {
				return fmt.Errorf("规则 %d（%s）不接受参数", i+1, r.Template)
			}
		case domain.RuleKeepLatestK:
			k, err := ruleInt(r, "k")
			if err != nil {
				return fmt.Errorf("规则 %d（%s）: %w", i+1, r.Template, err)
			}
			if k <= 0 {
				return fmt.Errorf("规则 %d（%s）的 k 必须大于 0", i+1, r.Template)
			}
		case domain.RuleKeepWithinDays:
			days, err := ruleInt(r, "days")
			if err != nil {
				return fmt.Errorf("规则 %d（%s）: %w", i+1, r.Template, err)
			}
			if days <= 0 {
				return fmt.Errorf("规则 %d（%s）的 days 必须大于 0", i+1, r.Template)
			}
		case "":
			return fmt.Errorf("规则 %d 缺少模板", i+1)
		default:
			return fmt.Errorf("规则 %d 使用了未知模板 %q", i+1, r.Template)
		}
		if r.TagSelector != "" {
			if _, err := compileGlob(r.TagSelector); err != nil {
				return fmt.Errorf("规则 %d 的 tag 选择器非法: %w", i+1, err)
			}
		}
	}
	return nil
}

func ruleInt(r *domain.RetentionRule, name string) (int, error) {
	v, ok := r.Params[name]
	if !ok {
		return 0, fmt.Errorf("缺少参数 %q", name)
	}
	return v, nil
}

// EvaluateRetention 对一个仓库的 tag 集合求值，算出该删哪些。
//
// 参数 tags 应当是**上游当前真实**的 tag 集合（不是本地快照），
// 见 03-registry.md §5.2.5 第 1 步。
//
// 返回值里的 Delete 已经过 digest 分组保护（§5.2.4 ①），
// 可以直接按 digest 执行删除。
func EvaluateRetention(policy *domain.RetentionPolicy, repo string, tags []domain.RetentionTag,
	now time.Time, protectLatest bool) (*RetentionDecision, error) {

	dec := &RetentionDecision{Repo: repo, Keep: []string{}, Delete: []DeletionCandidate{},
		Skipped: []domain.CleanupSkipped{}}

	// 空策略 / 未启用 → 什么都不做（fail-safe，§5.2.4 ④）。
	if policy == nil || !policy.Enabled || len(policy.Rules) == 0 {
		return dec, nil
	}

	keep := make(map[string]bool, len(tags))
	markKeep := func(tag string) { keep[tag] = true }

	// 每条规则编译一次选择器，供该规则的全部 tag 复用。
	type compiledRule struct {
		rule     *domain.RetentionRule
		selector *regexp.Regexp
	}
	compiled := make([]compiledRule, 0, len(policy.Rules))
	for i := range policy.Rules {
		r := &policy.Rules[i]
		sel := r.TagSelector
		if sel == "" {
			sel = policy.TagSelector
		}
		if sel == "" {
			sel = "**"
		}
		re, err := compileGlob(sel)
		if err != nil {
			return nil, fmt.Errorf("编译 tag 选择器 %q: %w", sel, err)
		}
		compiled = append(compiled, compiledRule{rule: r, selector: re})
	}

	// 规则求值（OR）
	for _, cr := range compiled {
		var matched []domain.RetentionTag
		for _, t := range tags {
			if cr.selector.MatchString(t.Tag) {
				matched = append(matched, t)
			}
		}
		switch cr.rule.Template {
		case domain.RuleKeepAlways:
			for _, t := range matched {
				markKeep(t.Tag)
			}
		case domain.RuleKeepLatestK:
			k, err := ruleInt(cr.rule, "k")
			if err != nil {
				return nil, err
			}
			for _, t := range latestK(matched, k) {
				markKeep(t.Tag)
			}
		case domain.RuleKeepWithinDays:
			days, err := ruleInt(cr.rule, "days")
			if err != nil {
				return nil, err
			}
			cutoff := now.Add(-time.Duration(days) * 24 * time.Hour)
			for _, t := range matched {
				// 推送时间未知的**不参与**这个规则——
				// 它们由下面的"信息不足一律保留"兜底。
				if t.PushedAt == nil {
					continue
				}
				if time.Unix(*t.PushedAt, 0).After(cutoff) {
					markKeep(t.Tag)
				}
			}
		}
	}

	// 安全规则 ②：推送时间未知 → 保留
	for _, t := range tags {
		if t.PushedAt == nil {
			markKeep(t.Tag)
			dec.Skipped = append(dec.Skipped, domain.CleanupSkipped{
				Repo: repo, Tag: t.Tag, Digest: t.Digest, Reason: SkipReasonUnknownTime,
			})
		}
	}

	// 安全规则 ③：latest 豁免
	if protectLatest {
		for _, t := range tags {
			if t.Tag == "latest" && !keep[t.Tag] {
				markKeep(t.Tag)
				dec.Skipped = append(dec.Skipped, domain.CleanupSkipped{
					Repo: repo, Tag: t.Tag, Digest: t.Digest, Reason: SkipReasonProtectedLatest,
				})
			}
		}
	}

	// 拿不到 digest 的 tag → 保留
	for _, t := range tags {
		if t.Digest == "" {
			markKeep(t.Tag)
			dec.Skipped = append(dec.Skipped, domain.CleanupSkipped{
				Repo: repo, Tag: t.Tag, Reason: SkipReasonNoDigest,
			})
		}
	}

	// 组装待删集合，并按 digest 分组（安全规则 ①）
	//
	// DELETE /v2/<repo>/manifests/<digest> 会**连带删掉所有指向该 digest 的 tag**。
	// 所以只有当一个 digest 下的**全部** tag 都被判定为可删，才能删这个 digest。
	groups := map[string][]domain.RetentionTag{}
	var order []string
	for _, t := range tags {
		if t.Digest == "" {
			continue
		}
		if _, seen := groups[t.Digest]; !seen {
			order = append(order, t.Digest)
		}
		groups[t.Digest] = append(groups[t.Digest], t)
	}
	for _, dg := range order {
		group := groups[dg]
		allDeletable := true
		for _, t := range group {
			if !keep[t.Tag] {
				continue
			}
			allDeletable = false
			break
		}
		if !allDeletable {
			// 同 digest 下有 tag 要保留 → 整组保留。
			for _, t := range group {
				if keep[t.Tag] {
					continue
				}
				dec.Skipped = append(dec.Skipped, domain.CleanupSkipped{
					Repo: repo, Tag: t.Tag, Digest: dg, Reason: SkipReasonSharedDigest,
				})
			}
			continue
		}
		cand := DeletionCandidate{Digest: dg, Tags: []string{}}
		for _, t := range group {
			cand.Tags = append(cand.Tags, t.Tag)
			cand.Size += t.Size
		}
		sort.Strings(cand.Tags)
		dec.Delete = append(dec.Delete, cand)
	}

	for _, t := range tags {
		if keep[t.Tag] {
			dec.Keep = append(dec.Keep, t.Tag)
		}
	}
	sort.Strings(dec.Keep)
	return dec, nil
}

// 推送时间未知的**不占名额**：它们会被"信息不足一律保留"兜底，
// 让它们占名额等于让已知时间的 tag 少留一个。
func latestK(tags []domain.RetentionTag, k int) []domain.RetentionTag {
	candidates := make([]domain.RetentionTag, 0, len(tags))
	for _, t := range tags {
		if t.PushedAt == nil {
			continue
		}
		candidates = append(candidates, t)
	}
	// 推送时间降序；同一秒内按 tag 名升序，保证结果可复现。
	sort.SliceStable(candidates, func(i, j int) bool {
		if *candidates[i].PushedAt != *candidates[j].PushedAt {
			return *candidates[i].PushedAt > *candidates[j].PushedAt
		}
		return candidates[i].Tag < candidates[j].Tag
	})
	if k > len(candidates) {
		k = len(candidates)
	}
	return candidates[:k]
}

// 支持：
//
//	**  匹配任意字符序列（含 "/"）
//	*   匹配任意字符序列（不含 "/"）
//	?   匹配单个字符（不含 "/"）
//
// 其余字符按字面量处理（正则元字符会被转义）。
func compileGlob(pattern string) (*regexp.Regexp, error) {
	if pattern == "" {
		pattern = "**"
	}
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch c {
		case '*':
			// ** 跨 "/"，* 不跨。
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				b.WriteString(".*")
				i++
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil, err
	}
	return re, nil
}

// MatchTagPattern 判断 tag 名是否匹配 glob 模式。
//
// 对外暴露给需要单点判断的场景；策略求值走 EvaluateRetention。
func MatchTagPattern(pattern, tag string) bool {
	re, err := compileGlob(pattern)
	if err != nil {
		// 模式非法时**不匹配**——宁可少删，不可误删。
		return false
	}
	return re.MatchString(tag)
}

package service

import (
	"testing"
	"time"

	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
)

// 保留策略求值的测试。
//
// 重点覆盖**四条安全规则**（docs/design/03-registry.md §5.2.4）——
// 每一条都对应一类不可逆的数据损失。

// ts 返回一个 unix 秒时间指针。
func ts(v int64) *int64 { return &v }

// baseTime 是测试用的基准时刻。
var baseTime = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

// policyWith 造一条策略。
func policyWith(rules ...domain.RetentionRule) *domain.RetentionPolicy {
	return &domain.RetentionPolicy{
		Enabled:     true,
		Algorithm:   RetentionAlgorithmOR,
		TagSelector: "**",
		Rules:       rules,
	}
}

// latestKRule 造一条 keep_latest_k 规则。
func latestKRule(k int, selector string) domain.RetentionRule {
	return domain.RetentionRule{
		Template:    domain.RuleKeepLatestK,
		Params:      map[string]int{"k": k},
		TagSelector: selector,
	}
}

// TestEvaluateRetention_KeepLatestK 验证只保留最近推送的 K 个。
func TestEvaluateRetention_KeepLatestK(t *testing.T) {
	policy := policyWith(latestKRule(2, ""))
	tags := []domain.RetentionTag{
		{Tag: "v1", Digest: "sha256:a", PushedAt: ts(baseTime.Add(-72 * time.Hour).Unix())},
		{Tag: "v2", Digest: "sha256:b", PushedAt: ts(baseTime.Add(-48 * time.Hour).Unix())},
		{Tag: "v3", Digest: "sha256:c", PushedAt: ts(baseTime.Add(-1 * time.Hour).Unix())},
		{Tag: "v4", Digest: "sha256:d", PushedAt: ts(baseTime.Unix())},
	}
	dec, err := EvaluateRetention(policy, "team-a/app", tags, baseTime, true)
	if err != nil {
		t.Fatalf("求值失败: %v", err)
	}
	if len(dec.Keep) != 2 {
		t.Fatalf("保留数 = %d（%v），期望 2", len(dec.Keep), dec.Keep)
	}
	if dec.Keep[0] != "v3" || dec.Keep[1] != "v4" {
		t.Fatalf("保留集 = %v，期望 [v3 v4]", dec.Keep)
	}
	if len(dec.Delete) != 2 {
		t.Fatalf("待删数 = %d，期望 2", len(dec.Delete))
	}
}

// TestEvaluateRetention_KeepWithinDays 验证按推送时间窗口保留。
func TestEvaluateRetention_KeepWithinDays(t *testing.T) {
	policy := policyWith(domain.RetentionRule{
		Template: domain.RuleKeepWithinDays,
		Params:   map[string]int{"days": 7},
	})
	tags := []domain.RetentionTag{
		{Tag: "old", Digest: "sha256:a", PushedAt: ts(baseTime.Add(-30 * 24 * time.Hour).Unix())},
		{Tag: "recent", Digest: "sha256:b", PushedAt: ts(baseTime.Add(-24 * time.Hour).Unix())},
	}
	dec, err := EvaluateRetention(policy, "team-a/app", tags, baseTime, true)
	if err != nil {
		t.Fatalf("求值失败: %v", err)
	}
	if len(dec.Keep) != 1 || dec.Keep[0] != "recent" {
		t.Fatalf("保留集 = %v，期望 [recent]", dec.Keep)
	}
	if len(dec.Delete) != 1 || dec.Delete[0].Digest != "sha256:a" {
		t.Fatalf("待删 = %+v，期望 sha256:a", dec.Delete)
	}
}

// TestEvaluateRetention_UnknownTimeIsKept 验证推送时间未知的 tag 一律保留。
//
// 这是安全规则 ②：不知道 ≠ 很老。把 NULL 当 0 会让老镜像被误删。
func TestEvaluateRetention_UnknownTimeIsKept(t *testing.T) {
	policy := policyWith(latestKRule(1, ""))
	tags := []domain.RetentionTag{
		{Tag: "no-time", Digest: "sha256:a"}, // 推送时间未知
		{Tag: "known", Digest: "sha256:b", PushedAt: ts(baseTime.Unix())},
	}
	dec, err := EvaluateRetention(policy, "team-a/app", tags, baseTime, true)
	if err != nil {
		t.Fatalf("求值失败: %v", err)
	}
	// 已知时间的 tag 占掉 k=1 的名额，未知时间的那个靠安全规则被保留。
	if len(dec.Keep) != 2 {
		t.Fatalf("保留数 = %d（%v），期望 2", len(dec.Keep), dec.Keep)
	}
	if len(dec.Delete) != 0 {
		t.Fatalf("待删 = %+v，期望空——推送时间未知的 tag 不该被删", dec.Delete)
	}
	found := false
	for _, sk := range dec.Skipped {
		if sk.Tag == "no-time" && sk.Reason == SkipReasonUnknownTime {
			found = true
		}
	}
	if !found {
		t.Fatalf("跳过明细里没有 no-time/unknown_time：%+v", dec.Skipped)
	}
}

// TestEvaluateRetention_SharedDigestKeepsWholeGroup 验证 digest 分组保护。
//
// 安全规则 ①：DELETE /manifests/<digest> 会连带删掉所有指向它的 tag，
// 所以同 digest 下只要有一个 tag 要保留，整个 digest 都不能删。
func TestEvaluateRetention_SharedDigestKeepsWholeGroup(t *testing.T) {
	policy := policyWith(latestKRule(1, ""))
	tags := []domain.RetentionTag{
		// v2 与 v3 指向同一个 manifest。
		{Tag: "v2", Digest: "sha256:same", PushedAt: ts(baseTime.Add(-2 * time.Hour).Unix())},
		{Tag: "v3", Digest: "sha256:same", PushedAt: ts(baseTime.Unix())},
		{Tag: "other", Digest: "sha256:other", PushedAt: ts(baseTime.Add(-9 * time.Hour).Unix())},
	}
	dec, err := EvaluateRetention(policy, "team-a/app", tags, baseTime, true)
	if err != nil {
		t.Fatalf("求值失败: %v", err)
	}
	// v3 最新 → 保留；于是 sha256:same 整组保留，v2 也不能删。
	for _, d := range dec.Delete {
		if d.Digest == "sha256:same" {
			t.Fatalf("sha256:same 被判为可删，但它下面的 v3 要保留——会连带删掉 v3")
		}
	}
	// other 组只有一个 tag 且不在保留集 → 可以删。
	if len(dec.Delete) != 1 || dec.Delete[0].Digest != "sha256:other" {
		t.Fatalf("待删 = %+v，期望只有 sha256:other", dec.Delete)
	}
	found := false
	for _, sk := range dec.Skipped {
		if sk.Tag == "v2" && sk.Reason == SkipReasonSharedDigest {
			found = true
		}
	}
	if !found {
		t.Fatalf("跳过明细里没有 v2/shared_digest：%+v", dec.Skipped)
	}
}

// TestEvaluateRetention_AllTagsOfDigestDeletable 验证同 digest 全部待删时整组删除。
func TestEvaluateRetention_AllTagsOfDigestDeletable(t *testing.T) {
	policy := policyWith(latestKRule(1, ""))
	tags := []domain.RetentionTag{
		{Tag: "old1", Digest: "sha256:same", PushedAt: ts(baseTime.Add(-5 * time.Hour).Unix())},
		{Tag: "old2", Digest: "sha256:same", PushedAt: ts(baseTime.Add(-4 * time.Hour).Unix())},
		{Tag: "new", Digest: "sha256:new", PushedAt: ts(baseTime.Unix())},
	}
	dec, err := EvaluateRetention(policy, "team-a/app", tags, baseTime, true)
	if err != nil {
		t.Fatalf("求值失败: %v", err)
	}
	if len(dec.Delete) != 1 {
		t.Fatalf("待删数 = %d，期望 1", len(dec.Delete))
	}
	if dec.Delete[0].Digest != "sha256:same" {
		t.Fatalf("待删 digest = %q，期望 sha256:same", dec.Delete[0].Digest)
	}
	if len(dec.Delete[0].Tags) != 2 {
		t.Fatalf("待删 tag = %v，期望两个", dec.Delete[0].Tags)
	}
}

// TestEvaluateRetention_LatestProtected 验证 latest 被豁免。
func TestEvaluateRetention_LatestProtected(t *testing.T) {
	policy := policyWith(latestKRule(1, ""))
	tags := []domain.RetentionTag{
		{Tag: "latest", Digest: "sha256:a", PushedAt: ts(baseTime.Add(-100 * time.Hour).Unix())},
		{Tag: "v2", Digest: "sha256:b", PushedAt: ts(baseTime.Unix())},
	}
	dec, err := EvaluateRetention(policy, "team-a/app", tags, baseTime, true)
	if err != nil {
		t.Fatalf("求值失败: %v", err)
	}
	for _, d := range dec.Delete {
		if d.Digest == "sha256:a" {
			t.Fatal("latest 被判为可删——protect_latest=true 时不该删")
		}
	}
	// 关掉豁免后它就该可删。
	dec2, err := EvaluateRetention(policy, "team-a/app", tags, baseTime, false)
	if err != nil {
		t.Fatalf("求值失败: %v", err)
	}
	deleted := false
	for _, d := range dec2.Delete {
		if d.Digest == "sha256:a" {
			deleted = true
		}
	}
	if !deleted {
		t.Fatal("protect_latest=false 时 latest 应当可删")
	}
}

// TestEvaluateRetention_EmptyPolicyDeletesNothing 验证空策略/未启用时什么都不删。
//
// 安全规则 ④：未配置态一律 fail-safe。
func TestEvaluateRetention_EmptyPolicyDeletesNothing(t *testing.T) {
	tags := []domain.RetentionTag{
		{Tag: "v1", Digest: "sha256:a", PushedAt: ts(baseTime.Add(-100 * time.Hour).Unix())},
	}
	cases := []struct {
		name   string
		policy *domain.RetentionPolicy
	}{
		{"nil 策略", nil},
		{"未启用", &domain.RetentionPolicy{Enabled: false, Algorithm: RetentionAlgorithmOR, TagSelector: "**"}},
		{"无规则", policyWith()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dec, err := EvaluateRetention(tc.policy, "team-a/app", tags, baseTime, true)
			if err != nil {
				t.Fatalf("求值失败: %v", err)
			}
			if len(dec.Delete) != 0 {
				t.Fatalf("待删 = %+v，期望空", dec.Delete)
			}
		})
	}
}

// TestEvaluateRetention_MissingDigestKeeps 验证拿不到 digest 的 tag 被保留。
func TestEvaluateRetention_MissingDigestKeeps(t *testing.T) {
	policy := policyWith(latestKRule(1, ""))
	tags := []domain.RetentionTag{
		{Tag: "broken", PushedAt: ts(baseTime.Add(-100 * time.Hour).Unix())}, // digest 空
	}
	dec, err := EvaluateRetention(policy, "team-a/app", tags, baseTime, true)
	if err != nil {
		t.Fatalf("求值失败: %v", err)
	}
	if len(dec.Delete) != 0 {
		t.Fatalf("待删 = %+v，期望空——拿不到 digest 的 tag 不该被删", dec.Delete)
	}
}

// TestEvaluateRetention_RuleSelector 验证规则级选择器生效。
func TestEvaluateRetention_RuleSelector(t *testing.T) {
	// release-* 永不删，其余只留最近 1 个。
	policy := policyWith(
		domain.RetentionRule{Template: domain.RuleKeepAlways, TagSelector: "release-*"},
		latestKRule(1, "**"),
	)
	tags := []domain.RetentionTag{
		{Tag: "release-1.0", Digest: "sha256:a", PushedAt: ts(baseTime.Add(-100 * time.Hour).Unix())},
		{Tag: "dev-1", Digest: "sha256:b", PushedAt: ts(baseTime.Add(-50 * time.Hour).Unix())},
		{Tag: "dev-2", Digest: "sha256:c", PushedAt: ts(baseTime.Unix())},
	}
	dec, err := EvaluateRetention(policy, "team-a/app", tags, baseTime, true)
	if err != nil {
		t.Fatalf("求值失败: %v", err)
	}
	if len(dec.Keep) != 2 {
		t.Fatalf("保留数 = %d（%v），期望 2", len(dec.Keep), dec.Keep)
	}
	kept := map[string]bool{}
	for _, k := range dec.Keep {
		kept[k] = true
	}
	if !kept["release-1.0"] {
		t.Fatalf("release-1.0 没被保留：%v", dec.Keep)
	}
	if !kept["dev-2"] {
		t.Fatalf("dev-2 没被保留：%v", dec.Keep)
	}
	if len(dec.Delete) != 1 || dec.Delete[0].Digest != "sha256:b" {
		t.Fatalf("待删 = %+v，期望只有 sha256:b", dec.Delete)
	}
}

// TestValidateRetentionPolicy 覆盖策略校验。
func TestValidateRetentionPolicy(t *testing.T) {
	cases := []struct {
		name    string
		policy  *domain.RetentionPolicy
		wantErr bool
	}{
		{
			name: "合法 keep_latest_k",
			policy: policyWith(domain.RetentionRule{
				Template: domain.RuleKeepLatestK, Params: map[string]int{"k": 3},
			}),
		},
		{
			name: "缺参数",
			policy: policyWith(domain.RetentionRule{
				Template: domain.RuleKeepLatestK,
			}),
			wantErr: true,
		},
		{
			name: "k 为 0",
			policy: policyWith(domain.RetentionRule{
				Template: domain.RuleKeepLatestK, Params: map[string]int{"k": 0},
			}),
			wantErr: true,
		},
		{
			name: "days 为负",
			policy: policyWith(domain.RetentionRule{
				Template: domain.RuleKeepWithinDays, Params: map[string]int{"days": -1},
			}),
			wantErr: true,
		},
		{
			name: "未知模板",
			policy: policyWith(domain.RetentionRule{
				Template: "keep_everything", Params: map[string]int{},
			}),
			wantErr: true,
		},
		{
			name: "未知算法",
			policy: &domain.RetentionPolicy{
				Enabled: true, Algorithm: "and", TagSelector: "**",
				Rules: []domain.RetentionRule{{Template: domain.RuleKeepAlways}},
			},
			wantErr: true,
		},
		{
			name: "keep_always 带参数",
			policy: policyWith(domain.RetentionRule{
				Template: domain.RuleKeepAlways, Params: map[string]int{"k": 1},
			}),
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateRetentionPolicy(tc.policy)
			if tc.wantErr && err == nil {
				t.Fatal("应当被拒绝，但没有报错")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("不应被拒绝，却报错: %v", err)
			}
		})
	}
}

// TestMatchTagPattern 覆盖 glob 匹配。
func TestMatchTagPattern(t *testing.T) {
	cases := []struct {
		pattern string
		tag     string
		want    bool
	}{
		{"**", "v1.0.0", true},
		{"**", "", true},
		{"release-*", "release-1.0", true},
		{"release-*", "dev-1.0", false},
		{"v?", "v1", true},
		{"v?", "v10", false},
		{"exact", "exact", true},
		{"exact", "exactly", false},
	}
	for _, tc := range cases {
		if got := MatchTagPattern(tc.pattern, tc.tag); got != tc.want {
			t.Errorf("MatchTagPattern(%q, %q) = %v，期望 %v", tc.pattern, tc.tag, got, tc.want)
		}
	}
}

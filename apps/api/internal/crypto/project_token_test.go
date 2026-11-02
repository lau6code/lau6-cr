package crypto

import (
	"strings"
	"testing"
)

func TestGenerateProjectToken(t *testing.T) {
	raw, hash, prefix, err := GenerateProjectToken()
	if err != nil {
		t.Fatalf("生成项目令牌: %v", err)
	}
	if !strings.HasPrefix(raw, ProjectTokenPrefix) {
		t.Fatalf("令牌前缀 = %q，期望 %q", raw, ProjectTokenPrefix)
	}
	if !strings.HasPrefix(prefix, ProjectTokenPrefix) {
		t.Fatalf("展示前缀 = %q，期望 %q", prefix, ProjectTokenPrefix)
	}
	if hash != HashProjectToken(raw) {
		t.Fatal("散列与原始值不匹配")
	}
	if strings.Contains(hash, raw) {
		t.Fatal("散列里不能出现原始值")
	}
	// 展示前缀只是原始令牌的一小截，不能等于令牌本体。
	if !strings.HasPrefix(raw, prefix) {
		t.Fatalf("展示前缀 %q 不是原始令牌 %q 的前缀", prefix, raw)
	}
	if len(prefix) >= len(raw) {
		t.Fatalf("展示前缀长度 %d 不得大于等于令牌长度 %d", len(prefix), len(raw))
	}
}

func TestGenerateProjectTokenIsUnique(t *testing.T) {
	a, _, _, err := GenerateProjectToken()
	if err != nil {
		t.Fatal(err)
	}
	b, _, _, err := GenerateProjectToken()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("两次生成的项目令牌不能相同")
	}
}

func TestIsProjectToken(t *testing.T) {
	raw, _, _, err := GenerateProjectToken()
	if err != nil {
		t.Fatal(err)
	}
	if !IsProjectToken(raw) {
		t.Fatal("生成的项目令牌应被识别")
	}
	if IsProjectToken("lau6_u_不是一枚项目令牌") {
		t.Fatal("个人令牌前缀不应被识别为项目令牌")
	}
	if IsProjectToken("") {
		t.Fatal("空串不应被识别为项目令牌")
	}
}

func TestIsLegacyPersonalToken(t *testing.T) {
	if !IsLegacyPersonalToken("cr_pat_旧令牌") {
		t.Fatal("旧前缀应被识别为历史令牌")
	}
	if IsLegacyPersonalToken("lau6_u_新令牌") {
		t.Fatal("新前缀不应被识别为历史令牌")
	}
	if IsLegacyPersonalToken("") {
		t.Fatal("空串不应被识别为历史令牌")
	}
}

func TestPersonalTokenPrefixIsLau6(t *testing.T) {
	raw, _, prefix, err := GeneratePersonalToken()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(raw, PATPrefix) {
		t.Fatalf("个人令牌前缀 = %q，期望 %q", raw, PATPrefix)
	}
	if !strings.HasPrefix(prefix, PATPrefix) {
		t.Fatalf("展示前缀 = %q，期望 %q", prefix, PATPrefix)
	}
	// 旧前缀不再被识别为个人令牌。
	if IsPersonalToken("cr_pat_旧令牌") {
		t.Fatal("旧前缀不应被识别为个人令牌")
	}
}

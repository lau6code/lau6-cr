package service

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lau6code/lau6-cr/apps/api/internal/config"
	"github.com/lau6code/lau6-cr/apps/api/internal/store"
)

// newTestSetupService 起一个带 sqlite 的 SetupService。
func newTestSetupService(t *testing.T) *SetupService {
	t.Helper()
	dsn := "file:" + filepath.ToSlash(filepath.Join(t.TempDir(), "test.db"))
	db, err := store.Open("sqlite", dsn, 1, 1)
	if err != nil {
		t.Fatalf("打开测试数据库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("执行迁移: %v", err)
	}
	settings, err := NewSettingsManager(db, config.Default(), "")
	if err != nil {
		t.Fatalf("构造设置管理器: %v", err)
	}
	return &SetupService{db: db, settings: settings}
}

// TestValidateBootstrapToken 覆盖令牌的长度约束。
func TestValidateBootstrapToken(t *testing.T) {
	cases := []struct {
		name    string
		tok     string
		wantErr bool
	}{
		{"空", "", true},
		{"太短", "short", true},
		{"刚好 16", strings.Repeat("a", 16), false},
		{"正常 64 hex", strings.Repeat("a", 64), false},
		{"超长", strings.Repeat("a", 129), true},
	}
	for _, c := range cases {
		err := validateBootstrapToken(c.tok)
		if c.wantErr && err == nil {
			t.Errorf("%s: 期望报错，实际 nil", c.name)
		}
		if !c.wantErr && err != nil {
			t.Errorf("%s: 期望通过，实际 %v", c.name, err)
		}
	}
}

// TestHashBootstrapTokenStable 确认摘要是确定的且不等于明文。
func TestHashBootstrapTokenStable(t *testing.T) {
	a := hashBootstrapToken("token-abc")
	b := hashBootstrapToken("token-abc")
	if a != b {
		t.Fatalf("同输入摘要不一致: %q vs %q", a, b)
	}
	if a == hashBootstrapToken("token-abd") {
		t.Fatal("不同输入摘要相同")
	}
	if strings.Contains(a, "token-abc") {
		t.Fatalf("摘要里出现明文: %q", a)
	}
}

// TestSubmitStoresBootstrapToken 覆盖首次初始化：令牌落库（存摘要，不存明文）。
func TestSubmitStoresBootstrapToken(t *testing.T) {
	ctx := context.Background()
	s := newTestSetupService(t)

	const tok = "0123456789abcdef0123456789abcdef"
	if _, err := s.Submit(ctx, SetupInput{
		BootstrapToken: tok,
		PublicURL:      "http://localhost:5173",
		AdminUsername:  "admin",
		AdminPassword:  "VerifyPass123!",
	}); err != nil {
		t.Fatalf("首次 Submit: %v", err)
	}

	stored, err := s.StoredBootstrapToken(ctx)
	if err != nil {
		t.Fatalf("读已存令牌: %v", err)
	}
	if stored == "" {
		t.Fatal("令牌没有落库")
	}
	if stored == tok {
		t.Fatal("库里存的是明文令牌")
	}
	if stored != hashBootstrapToken(tok) {
		t.Fatalf("库里摘要 = %q, 期望 %q", stored, hashBootstrapToken(tok))
	}
}

// TestSubmitRejectsWrongTokenOnReinit 覆盖「已有令牌时必须匹配」。
//
// 场景：setup reset 清掉初始化标记后重新初始化——库里还留着上次的令牌摘要，
// 这时必须提交同一个令牌。
func TestSubmitRejectsWrongTokenOnReinit(t *testing.T) {
	ctx := context.Background()
	s := newTestSetupService(t)

	const tok = "0123456789abcdef0123456789abcdef"
	in := SetupInput{
		BootstrapToken: tok,
		PublicURL:      "http://localhost:5173",
		AdminUsername:  "admin",
		AdminPassword:  "VerifyPass123!",
	}
	if _, err := s.Submit(ctx, in); err != nil {
		t.Fatalf("首次 Submit: %v", err)
	}

	// 模拟 setup reset：清掉初始化标记（令牌摘要保留）。
	if _, err := s.db.Exec(ctx, `DELETE FROM system_settings WHERE key = 'system.initialized'`); err != nil {
		t.Fatalf("清初始化标记: %v", err)
	}
	if _, err := s.db.Exec(ctx, `DELETE FROM users`); err != nil {
		t.Fatalf("清用户: %v", err)
	}

	// 错的令牌 → 拒绝。
	bad := in
	bad.BootstrapToken = "ffffffffffffffffffffffffffffffff"
	if _, err := s.Submit(ctx, bad); err != ErrBadBootstrapToken {
		t.Fatalf("错误令牌 err = %v, 期望 ErrBadBootstrapToken", err)
	}

	// 正确的令牌 → 放行。
	if _, err := s.Submit(ctx, in); err != nil {
		t.Fatalf("正确令牌 Submit: %v", err)
	}
}

// TestSubmitRejectsShortToken 覆盖首次初始化时弱令牌被拒。
func TestSubmitRejectsShortToken(t *testing.T) {
	ctx := context.Background()
	s := newTestSetupService(t)

	_, err := s.Submit(ctx, SetupInput{
		BootstrapToken: "short",
		PublicURL:      "http://localhost:5173",
		AdminUsername:  "admin",
		AdminPassword:  "VerifyPass123!",
	})
	if err == nil {
		t.Fatal("弱令牌应被拒绝")
	}
	if !strings.Contains(err.Error(), "引导令牌") {
		t.Fatalf("错误信息 = %q, 期望提到引导令牌", err.Error())
	}
}

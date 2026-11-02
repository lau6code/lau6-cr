package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lau6code/lau6-cr/apps/api/internal/config"
	"github.com/lau6code/lau6-cr/apps/api/internal/store"
)

// newTestSetupServiceWithConfig 起一个带 sqlite 的 SetupService，
// 配置基线用 cfg（已经按环境变量生效后的值）。
func newTestSetupServiceWithConfig(t *testing.T, cfg *config.Config, yamlPath string) *SetupService {
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
	settings, err := NewSettingsManager(db, cfg, yamlPath)
	if err != nil {
		t.Fatalf("构造设置管理器: %v", err)
	}
	return &SetupService{db: db, settings: settings, yamlPath: yamlPath}
}

// TestSubmitKeepsEnvPinnedLDAPEnabled 覆盖「env 钉死的项不被向导覆盖」。
//
// setup 不能无条件改写 cfg.LDAP.*，必须走 envPinned 检查。
// 否则 CR_LDAP_ENABLED=true 的实例在初始化时被静默改成关闭——
// 运行期 LDAP 登录不可用、配置文件里写着 ldap.enabled: false，
// 重启后 env 又把它打开，出现"初始化后关、重启后开"的翻转。
func TestSubmitKeepsEnvPinnedLDAPEnabled(t *testing.T) {
	ctx := context.Background()
	t.Setenv("CR_LDAP_ENABLED", "true")
	t.Setenv("CR_LDAP_URL", "ldap://ldap.example.com:389")
	t.Setenv("CR_LDAP_USER_SEGMENT", "uid={0},ou=people,dc=example,dc=com")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("加载配置: %v", err)
	}
	if !cfg.LDAP.Enabled {
		t.Fatal("前置条件：CR_LDAP_ENABLED=true 应让配置里的 LDAP 处于启用")
	}

	yamlPath := filepath.Join(t.TempDir(), "application.yaml")
	s := newTestSetupServiceWithConfig(t, cfg, yamlPath)

	res, err := s.Submit(ctx, SetupInput{
		BootstrapToken: "0123456789abcdef0123456789abcdef",
		PublicURL:      "http://localhost:5173",
		AdminUsername:  "admin",
		AdminPassword:  "VerifyPass123!",
		// 向导里把 LDAP 关掉、指向另一个地址。
		LDAP: &LDAPInput{
			Enabled:     false,
			URL:         "ldap://ignored.example.com:389",
			DC:          "dc=ignored",
			UserSegment: "uid={0},ou=ignored",
			AllowCreate: true,
			TimeoutSec:  5,
		},
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	// ① 运行期快照：LDAP 必须还是启用的，且用的是 env 里的地址。
	snap := s.settings.Snapshot()
	if !snap.LDAP.Enabled {
		t.Fatal("env 钉死的 LDAP 被向导关掉了")
	}
	if got := snap.LDAP.URL; got != "ldap://ldap.example.com:389" {
		t.Fatalf("LDAP URL = %q, 期望保留环境变量的值", got)
	}

	// ② 被忽略的项必须回报给调用方（不然用户以为填了就生效）。
	var found bool
	for _, p := range res.IgnoredEnvPinned {
		if p.Key == "ldap.enabled" && p.EnvVar == "CR_LDAP_ENABLED" {
			found = true
		}
	}
	if !found {
		t.Fatalf("IgnoredEnvPinned = %+v, 期望包含 ldap.enabled", res.IgnoredEnvPinned)
	}

	// ③ 配置文件里也不能写成关闭——运维查文件会得出错误结论。
	b, err := os.ReadFile(yamlPath)
	if err != nil {
		t.Fatalf("读配置: %v", err)
	}
	if !strings.Contains(string(b), "enabled: true") {
		t.Fatalf("配置文件里没有 ldap.enabled: true：\n%s", string(b))
	}
}

// TestSubmitWithoutLDAPBlockKeepsLDAP 覆盖「请求里没有 LDAP 段 = 没这个意图」。
//
// CLI 路径（lau6-cr setup）根本不问 LDAP，
// 用值类型会把"没填"和"显式关掉"混成同一个零值——
// 结果是 CLI 初始化把 LDAP 悄悄关掉了。
func TestSubmitWithoutLDAPBlockKeepsLDAP(t *testing.T) {
	ctx := context.Background()
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("加载配置: %v", err)
	}
	cfg.LDAP.Enabled = true
	cfg.LDAP.URL = "ldap://ldap.example.com:389"
	cfg.LDAP.UserSegment = "uid={0},ou=people,dc=example,dc=com"

	s := newTestSetupServiceWithConfig(t, cfg, "")

	// 注意：没有 LDAP 段 —— 与 CLI 路径一致。
	if _, err := s.Submit(ctx, SetupInput{
		BootstrapToken: "0123456789abcdef0123456789abcdef",
		PublicURL:      "http://localhost:5173",
		AdminUsername:  "admin",
		AdminPassword:  "VerifyPass123!",
	}); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	if !s.settings.Snapshot().LDAP.Enabled {
		t.Fatal("请求里没有 LDAP 段，LDAP 不该被动过")
	}
}

// TestSubmitAppliesLDAPWhenNotEnvPinned 覆盖「没被 env 钉死时向导照常生效」。
//
// 这是修 env 钉死不能顺手改坏的那一半：向导里开 LDAP 就得真的开。
func TestSubmitAppliesLDAPWhenNotEnvPinned(t *testing.T) {
	ctx := context.Background()
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("加载配置: %v", err)
	}
	cfg.LDAP.Enabled = false

	s := newTestSetupServiceWithConfig(t, cfg, "")

	res, err := s.Submit(ctx, SetupInput{
		BootstrapToken: "0123456789abcdef0123456789abcdef",
		PublicURL:      "http://localhost:5173",
		AdminUsername:  "admin",
		AdminPassword:  "VerifyPass123!",
		LDAP: &LDAPInput{
			Enabled:     true,
			URL:         "ldap://ldap.example.com:389",
			DC:          "dc=example,dc=com",
			UserSegment: "uid={0},ou=people,dc=example,dc=com",
			AllowCreate: true,
			TimeoutSec:  9,
		},
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	snap := s.settings.Snapshot()
	if !snap.LDAP.Enabled {
		t.Fatal("向导里开了 LDAP，初始化后应当仍然是开的")
	}
	if snap.LDAP.URL != "ldap://ldap.example.com:389" {
		t.Fatalf("LDAP URL = %q, 期望用向导里的值", snap.LDAP.URL)
	}
	if snap.LDAP.TimeoutSec != 9 {
		t.Fatalf("LDAP TimeoutSec = %d, 期望 9", snap.LDAP.TimeoutSec)
	}
	for _, p := range res.IgnoredEnvPinned {
		if strings.HasPrefix(p.Key, "ldap.") {
			t.Fatalf("没有被 env 钉死，却报成被忽略: %+v", p)
		}
	}
}

// TestSubmitKeepsEnvPinnedPublicURL 覆盖 public_url 也受 env 钉死保护。
//
// env 钉死的字段不能被向导无条件覆盖——
// 那与设置页"由环境变量固定，无法在此修改"的规矩冲突。
func TestSubmitKeepsEnvPinnedPublicURL(t *testing.T) {
	ctx := context.Background()
	t.Setenv("CR_PUBLIC_URL", "https://registry.example.com")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("加载配置: %v", err)
	}
	if cfg.Core.PublicURL != "https://registry.example.com" {
		t.Fatalf("前置条件：PublicURL = %q, 期望环境变量的值", cfg.Core.PublicURL)
	}

	s := newTestSetupServiceWithConfig(t, cfg, "")

	res, err := s.Submit(ctx, SetupInput{
		BootstrapToken: "0123456789abcdef0123456789abcdef",
		PublicURL:      "http://localhost:5173",
		AdminUsername:  "admin",
		AdminPassword:  "VerifyPass123!",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if got := s.settings.Snapshot().Core.PublicURL; got != "https://registry.example.com" {
		t.Fatalf("PublicURL = %q, 期望保留环境变量的值", got)
	}
	var found bool
	for _, p := range res.IgnoredEnvPinned {
		if p.Key == "core.public_url" && p.EnvVar == "CR_PUBLIC_URL" {
			found = true
		}
	}
	if !found {
		t.Fatalf("IgnoredEnvPinned = %+v, 期望包含 core.public_url", res.IgnoredEnvPinned)
	}
}

// TestSubmitDoesNotReportHiddenLDAPDetails 覆盖「LDAP 关着时看不见的细节字段不报」。
//
// 场景：LDAP 全靠环境变量配（CLI 路径根本不问 LDAP），向导里的 LDAP 开关是关着的
// → 细节字段（start_tls / allow_create / timeout_sec）在界面上**看不见**。
// 报成"向导里的值被忽略"是在说用户没填过的东西。
//
// ⚠️ ldap.enabled 该报：那个开关用户**看得见**（显示关着，实际被 env 打开）。
func TestSubmitDoesNotReportHiddenLDAPDetails(t *testing.T) {
	ctx := context.Background()
	t.Setenv("CR_LDAP_ENABLED", "true")
	t.Setenv("CR_LDAP_URL", "ldap://ldap.example.com:389")
	t.Setenv("CR_LDAP_USER_SEGMENT", "uid={0},ou=people,dc=example,dc=com")
	t.Setenv("CR_LDAP_START_TLS", "true")
	t.Setenv("CR_LDAP_ALLOW_CREATE", "false")
	t.Setenv("CR_LDAP_TIMEOUT_SEC", "9")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("加载配置: %v", err)
	}
	s := newTestSetupServiceWithConfig(t, cfg, "")

	res, err := s.Submit(ctx, SetupInput{
		BootstrapToken: "0123456789abcdef0123456789abcdef",
		PublicURL:      "http://localhost:5173",
		AdminUsername:  "admin",
		AdminPassword:  "VerifyPass123!",
		LDAP: &LDAPInput{
			// 向导里 LDAP 是关着的 → 细节字段用户看不见。
			Enabled:     false,
			StartTLS:    false,
			AllowCreate: true,
			TimeoutSec:  5,
		},
	})
	if err != nil {
		t.Fatalf("Submit 不该失败: %v", err)
	}
	for _, p := range res.IgnoredEnvPinned {
		if p.Key != "ldap.enabled" {
			t.Fatalf("看不见的 LDAP 细节字段不该报成被忽略: %+v", res.IgnoredEnvPinned)
		}
	}
	// 生效值仍来自环境变量——向导的默认值没盖掉它。
	snap := s.settings.Snapshot()
	if !snap.LDAP.StartTLS {
		t.Error("env 钉死的 start_tls 被向导的默认值盖掉了")
	}
	if snap.LDAP.AllowCreate {
		t.Error("env 钉死的 allow_create 被向导的默认值盖掉了")
	}
	if snap.LDAP.TimeoutSec != 9 {
		t.Errorf("timeout_sec = %d，期望 9", snap.LDAP.TimeoutSec)
	}
}

// TestStatusReportsEffectivePublicURL 覆盖引导状态里带上**生效**的公共访问 URL。
//
// 被 env 钉死的那一栏在向导里是只读的，只给空值的话——
// 用户看到一个空的只读输入框，既看不到地址也填不进去，
// 点"下一步"就被"必须是 http:// 或 https:// 开头"卡住，初始化走不下去。
func TestStatusReportsEffectivePublicURL(t *testing.T) {
	t.Setenv("CR_PUBLIC_URL", "https://registry.example.com")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("加载配置: %v", err)
	}
	s := newTestSetupServiceWithConfig(t, cfg, "")

	st, err := s.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.PublicURL != "https://registry.example.com" {
		t.Fatalf("Status().PublicURL = %q, 期望生效值 %q", st.PublicURL, "https://registry.example.com")
	}
}

// TestSubmitAcceptsEmptyPublicURLWhenEnvPinned 覆盖「被 env 钉死时不再校验向导值」。
//
// 场景：CR_PUBLIC_URL 已经把对外地址定死了，向导那一栏是只读的，
// 用户没填（也不该填）。此时服务端若还要求 public_url 非空且以 http(s) 开头，
// 初始化就会卡在一个**用户改不了**的字段上。
func TestSubmitAcceptsEmptyPublicURLWhenEnvPinned(t *testing.T) {
	ctx := context.Background()
	t.Setenv("CR_PUBLIC_URL", "https://registry.example.com")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("加载配置: %v", err)
	}
	s := newTestSetupServiceWithConfig(t, cfg, "")

	res, err := s.Submit(ctx, SetupInput{
		BootstrapToken: "0123456789abcdef0123456789abcdef",
		PublicURL:      "", // 只读栏，用户没填
		AdminUsername:  "admin",
		AdminPassword:  "VerifyPass123!",
	})
	if err != nil {
		t.Fatalf("Submit 不该失败: %v", err)
	}
	if got := s.settings.Snapshot().Core.PublicURL; got != "https://registry.example.com" {
		t.Fatalf("PublicURL = %q, 期望保留环境变量的值", got)
	}
	// 空串 = **没填**（与 LDAP 文本字段同一口径），不是"想清空它"——
	// 没什么被忽略，不该报。
	for _, p := range res.IgnoredEnvPinned {
		if p.Key == "core.public_url" {
			t.Fatalf("没填的值不该报成被忽略: %+v", res.IgnoredEnvPinned)
		}
	}
}

// TestSubmitDoesNotReportReadOnlyPublicURL 覆盖「只读栏的值不算被忽略」。
//
// 场景：CR_PUBLIC_URL 把对外地址定死了，向导那一栏是**只读**的
// （前端预填生效值、禁用输入框），用户根本没机会改它。
// 把它报成"向导里的值已被忽略"是在说一件没发生过的事——
// 初始化后还弹这条提示，纯属噪声。
func TestSubmitDoesNotReportReadOnlyPublicURL(t *testing.T) {
	ctx := context.Background()
	t.Setenv("CR_PUBLIC_URL", "https://registry.example.com")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("加载配置: %v", err)
	}
	s := newTestSetupServiceWithConfig(t, cfg, "")

	res, err := s.Submit(ctx, SetupInput{
		BootstrapToken: "0123456789abcdef0123456789abcdef",
		// 只读栏：前端预填的就是生效值。
		PublicURL:     "https://registry.example.com",
		AdminUsername: "admin",
		AdminPassword: "VerifyPass123!",
	})
	if err != nil {
		t.Fatalf("Submit 不该失败: %v", err)
	}
	if got := s.settings.Snapshot().Core.PublicURL; got != "https://registry.example.com" {
		t.Fatalf("PublicURL = %q, 期望保留环境变量的值", got)
	}
	for _, p := range res.IgnoredEnvPinned {
		if p.Key == "core.public_url" {
			t.Fatalf("只读栏的值不该报成被忽略: %+v", res.IgnoredEnvPinned)
		}
	}
}

// TestSubmitStillValidatesPublicURLWhenNotPinned 覆盖「没被钉死时校验照旧」。
//
// public_url 只有被 env 钉死时才跳过校验；普通情形下必须合法。
func TestSubmitStillValidatesPublicURLWhenNotPinned(t *testing.T) {
	ctx := context.Background()
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("加载配置: %v", err)
	}
	s := newTestSetupServiceWithConfig(t, cfg, "")

	_, err = s.Submit(ctx, SetupInput{
		BootstrapToken: "0123456789abcdef0123456789abcdef",
		PublicURL:      "",
		AdminUsername:  "admin",
		AdminPassword:  "VerifyPass123!",
	})
	if err == nil {
		t.Fatal("public_url 为空时 Submit 应当报错")
	}
	if !strings.Contains(err.Error(), "public_url") {
		t.Fatalf("错误信息 = %q, 期望提到 public_url", err.Error())
	}
}

// TestStatusReportsEnvPinned 覆盖初始化状态里带上 env 钉死清单。
//
// 向导靠它把对应字段显示成只读——
// 后端拒绝改还不够，用户也得**事先知道**改了不生效。
func TestStatusReportsEnvPinned(t *testing.T) {
	t.Setenv("CR_LDAP_ENABLED", "true")
	t.Setenv("CR_LDAP_URL", "ldap://ldap.example.com:389")
	t.Setenv("CR_LDAP_USER_SEGMENT", "uid={0},ou=people,dc=example,dc=com")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("加载配置: %v", err)
	}
	s := newTestSetupServiceWithConfig(t, cfg, "")

	st, err := s.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	var found bool
	for _, p := range st.EnvPinned {
		if p.Key == "ldap.enabled" && p.EnvVar == "CR_LDAP_ENABLED" {
			found = true
		}
	}
	if !found {
		t.Fatalf("EnvPinned = %+v, 期望包含 ldap.enabled", st.EnvPinned)
	}
}

// TestSubmitKeepsExistingLDAPURLWhenWizardEmpty 覆盖「向导里的空串不抹掉已有值」。
//
// 场景：env 把 LDAP 打开了（CR_LDAP_ENABLED=true），URL 却来自配置文件。
// Web 向导里用户没填 URL（空串）→ 空串会把配置文件里的 URL 抹掉，
// 紧接着的校验报"启用 LDAP 时 url 不能为空"，**初始化直接卡死**。
func TestSubmitKeepsExistingLDAPURLWhenWizardEmpty(t *testing.T) {
	ctx := context.Background()
	t.Setenv("CR_LDAP_ENABLED", "true")

	// URL 来自配置文件（不是 env），所以它本身没被 env 钉死。
	yamlCfg := filepath.Join(t.TempDir(), "application.yaml")
	if err := os.WriteFile(yamlCfg, []byte(
		"ldap:\n"+
			"  url: ldap://from-yaml.example.com:389\n"+
			"  user_segment: \"uid={0},ou=people,dc=example,dc=com\"\n"), 0o600); err != nil {
		t.Fatalf("写配置: %v", err)
	}
	cfg, err := config.Load(yamlCfg)
	if err != nil {
		t.Fatalf("加载配置: %v", err)
	}

	s := newTestSetupServiceWithConfig(t, cfg, "")

	if _, err := s.Submit(ctx, SetupInput{
		BootstrapToken: "0123456789abcdef0123456789abcdef",
		PublicURL:      "http://localhost:5173",
		AdminUsername:  "admin",
		AdminPassword:  "VerifyPass123!",
		LDAP: &LDAPInput{
			Enabled: false, // 向导里没开（但被 env 钉死成开）
			URL:     "",    // 用户没填
		},
	}); err != nil {
		t.Fatalf("Submit 不该失败：%v", err)
	}
	snap := s.settings.Snapshot()
	if !snap.LDAP.Enabled {
		t.Fatal("env 钉死的 LDAP 被向导关掉了")
	}
	if snap.LDAP.URL != "ldap://from-yaml.example.com:389" {
		t.Fatalf("LDAP URL = %q, 期望保留配置文件里的值", snap.LDAP.URL)
	}
}

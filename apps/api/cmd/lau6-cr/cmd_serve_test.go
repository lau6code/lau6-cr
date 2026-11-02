package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/lau6code/lau6-cr/apps/api/internal/config"
	"github.com/lau6code/lau6-cr/apps/api/internal/server"
)

// TestSetupSecretsProvidePasswordKeys 覆盖 serve 路径上的口令加密密钥材料齐备。
//
// ⚠️ 这条链路踩过的坑：密钥库被构造出来了，却没接进 server.App。
//
//	结果公钥接口**每个请求**都 500（"口令加密未正确初始化"），
//	而服务照常启动、登录也照常成功（optional 模式收明文）——
//	只有前端拿不到公钥，排查起来最费时间。
//
// 这里锁住两件事：密钥库造得出来，且接进 App 后启动自检通过。
func TestSetupSecretsProvidePasswordKeys(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Keystore.PrivatePath = filepath.Join(dir, "jwt-private.pem")
	cfg.Keystore.JWKSPath = filepath.Join(dir, "jwt-jwks.json")
	cfg.Keystore.PasswordKeyPath = filepath.Join(dir, "password-key.pem")
	cfg.Keystore.WebhookSecretPath = filepath.Join(dir, "webhook-secret")
	cfg.Keystore.RegistrySecretPath = filepath.Join(dir, "registry-secret")

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	// 缓存传 nil：local 模式下私钥以文件为权威源（12-cache.md §3.3）。
	sec := setupSecrets(cfg, nil, logger)

	if sec.passwordKeys == nil {
		t.Fatalf("serveSecrets 没带上口令加密密钥库")
	}
	app := &server.App{Cfg: cfg, PasswordKeys: sec.passwordKeys}
	if err := app.CheckPasswordTransport(); err != nil {
		t.Fatalf("接上密钥库后启动自检仍失败: %v", err)
	}
}

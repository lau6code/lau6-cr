package service

import (
	"fmt"
	"os"
	"strings"

	"github.com/lau6code/lau6-cr/apps/api/internal/config"
)

// 它回答一个问题：**运维要在远程 distribution 上配什么，才能和这台 lau6-cr 对上**。
// 两样东西：
//
//  1. JWKS 来源 —— distribution 验 lau6-cr 签的 JWT 时要公钥；
//  2. 事件回调地址 + 共享密钥 —— distribution 把 push/pull/delete 事件回调回来。
type IntegrationInfo struct {
	// UpstreamAddr 是 distribution 的地址（registry.upstream_addr）。
	UpstreamAddr string
	// PublicURL 是 lau6-cr 的外部访问根 URL。
	PublicURL string
	// JWKSURL 是 JWKS 的 HTTP 地址（<public_url>/jwks.json）。
	JWKSURL string
	// JWKSPath 是 JWKS 文件路径——复制到远程 distribution 即可。
	JWKSPath string
	// CallbackURL 是事件回调地址（<public_url>/api/internal/registry-events）。
	CallbackURL string
	// WebhookSecretPath 是回调共享密钥的文件路径。
	WebhookSecretPath string
	// WebhookSecret 是回调共享密钥的**值**——只在 showSecret=true 时填充。
	WebhookSecret string
}

// showSecret=false 时**不读**密钥文件（只给路径），
// 免得"看一眼接入信息"就把密钥读进内存 / 日志。
func BuildIntegrationInfo(cfg *config.Config, showSecret bool) (*IntegrationInfo, error) {
	if cfg == nil {
		return nil, fmt.Errorf("配置为空")
	}
	// ⚠️ 内嵌模式下**没有**远程 distribution 可接入：
	//    它就在本容器里，JWKS 与事件回调都由 lau6-cr 自己渲染好。
	//    这时还打印一份"接入清单"就是误导——照着配到哪儿去？
	if cfg.Registry.Mode == config.ModeEmbedded {
		return nil, fmt.Errorf("当前是内嵌模式（registry.mode=embedded）：" +
			"distribution 与 lau6-cr 同容器运行，不需要外部接入。" +
			"接入清单只在 registry.mode=external（独立部署的 distribution）时才有意义")
	}
	base := strings.TrimRight(cfg.Core.PublicURL, "/")
	info := &IntegrationInfo{
		UpstreamAddr:      strings.TrimRight(cfg.Registry.UpstreamAddr, "/"),
		PublicURL:         base,
		JWKSURL:           base + "/jwks.json",
		JWKSPath:          cfg.Keystore.JWKSPath,
		CallbackURL:       base + "/api/internal/registry-events",
		WebhookSecretPath: cfg.Keystore.WebhookSecretPath,
	}
	if showSecret {
		secret, err := readSecretFile(cfg.Keystore.WebhookSecretPath)
		if err != nil {
			return nil, err
		}
		info.WebhookSecret = secret
	}
	return info, nil
}

// readSecretFile 读一个密钥文件（十六进制随机串）。
//
// 文件不存在不算错误——服务还没起过时会没有，
// 这时返回空串，由调用方提示"先启动一次服务生成密钥"。
func readSecretFile(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("读取密钥 %s: %w", path, err)
	}
	return strings.TrimSpace(string(b)), nil
}

// 口令加密密钥库（X25519）——与 Ed25519 签名密钥**完全独立**。
//
// 为什么必须独立（docs/design/13-password-transport.md §4.2 / ADR 4）：
//
//   - 签名密钥（cr:signing-key / jwt-private.pem）被读走 = 攻击者可以伪造
//     任意用户的 registry token，影响面是整个系统；
//   - 口令加密私钥（cr:password-key）被读走，只能解开**抓到的密文**，
//     影响面小得多。
//
// 两把钥匙的存放策略同构（与 keystore.go 一致）、但键名与文件各自分开，
// 轮换节奏也各自独立。
package crypto

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"time"

	"github.com/lau6code/lau6-cr/apps/api/internal/cache"
	"github.com/lau6code/lau6-cr/apps/api/internal/fsatomic"
)

// 口令加密密钥的存放位置。
const (
	// CacheKeyPasswordKey 是口令加密私钥的缓存键（12-cache.md §3.6）。
	//
	// ⚠️ 与签名密钥（CacheKeySigningKey）是**两套独立的密钥**：
	//    键名、文件、轮换节奏、失效影响都不同。
	CacheKeyPasswordKey = "cr:password-key"

	// CacheKeyPasswordNoncePrefix 是重放防护用的 nonce 键前缀。
	CacheKeyPasswordNoncePrefix = "cr:password-nonce:"
)

// PasswordKeyStore 持有口令加密的 X25519 私钥。
//
// 私钥是**唯一真源**：公钥不单独落盘，随时从私钥导出。
type PasswordKeyStore struct {
	priv *ecdh.PrivateKey
	pub  []byte
	kid  string
}

// PasswordKeyOptions 是口令加密密钥库的构造参数。
type PasswordKeyOptions struct {
	// PrivatePath 是口令加密私钥的文件路径（0600）。
	//
	// 与签名私钥（jwt-private.pem）**分开存放**，建议放在同目录下的
	// password-key.pem。
	PrivatePath string

	// Cache 是缓存后端（可为 nil → 只用文件）。
	//
	// ⚠️ 非 nil 且 driver=redis 时，**缓存是权威源**
	//（docs/design/12-cache.md §3.3，与签名密钥同构）：
	//
	//	缓存里有   → 载入内存（**不读文件**）
	//	缓存里没有 → 生成 → 写回缓存 + 写一份文件（审计副本）
	//	缓存连不上 → **启动失败**（fail-closed）
	//
	// fail-closed 是硬要求：降级成"连不上就生成新密钥"会让每次缓存抖动
	// 都换一把密钥——正在使用旧 kid 的客户端会突然全部登录失败。
	Cache cache.Cache
}

// NewPasswordKeyStore 按配置构造口令加密密钥库。
func NewPasswordKeyStore(o PasswordKeyOptions) (*PasswordKeyStore, error) {
	if o.PrivatePath == "" {
		return nil, fmt.Errorf("口令加密私钥路径未配置")
	}
	ctx := context.Background()
	priv, err := o.loadPrivateKey(ctx)
	if err != nil {
		return nil, err
	}
	pub := priv.PublicKey().Bytes()
	return &PasswordKeyStore{priv: priv, pub: pub, kid: passwordKeyID(pub)}, nil
}

// loadPrivateKey 按 §3.3 的存放策略取私钥。
func (o PasswordKeyOptions) loadPrivateKey(ctx context.Context) (*ecdh.PrivateKey, error) {
	return loadKey(ctx, o.Cache, keySource[*ecdh.PrivateKey]{
		label:    "口令加密密钥",
		cacheKey: CacheKeyPasswordKey,
		filePath: o.PrivatePath,
		parse:    parsePasswordPrivateKey,
		generate: generatePasswordPrivateKey,
		encode:   encodePasswordPrivateKey,
	})
}

// KID 返回密钥标识。
//
// 轮换期间新旧 kid 并存时，客户端用它挑一把能解的钥匙。
func (s *PasswordKeyStore) KID() string { return s.kid }

// PublicBase64 返回公钥（base64url，无填充）。
func (s *PasswordKeyStore) PublicBase64() string {
	return base64.RawURLEncoding.EncodeToString(s.pub)
}

// Decrypt 解开一个口令信封（服务端侧）。
//
// username 是期望的用户名（参与 HKDF 的 info 派生）；now 用于校验 ts 窗口。
func (s *PasswordKeyStore) Decrypt(
	env PasswordEnvelope,
	username string,
	now time.Time,
) (PasswordPayload, error) {
	return DecryptPassword(s.priv, s.kid, env, username, now)
}

// SeedCache 把口令加密私钥写进给定的缓存后端。
//
// 用途：**换缓存后端**时把密钥搬过去（local → redis 热切换）。
//
// ⚠️ 不搬的后果：新后端里没有密钥 → 下次启动重新生成 → 旧 kid 失效，
// 正在用旧 kid 的客户端会登录失败（客户端重新拉公钥即可自愈）。
func (s *PasswordKeyStore) SeedCache(ctx context.Context, c cache.Cache) error {
	if c == nil {
		return nil
	}
	pemBytes, err := encodePasswordPrivateKey(s.priv)
	if err != nil {
		return err
	}
	// ⚠️ 不设 TTL（它是启动源）。
	if err := c.Set(ctx, CacheKeyPasswordKey, pemBytes, 0); err != nil {
		return fmt.Errorf("写口令加密密钥到缓存: %w", err)
	}
	return nil
}

// PersistPrivate 把私钥写到文件（0600）。
//
// 用途：换缓存后端时切回 local——local 模式以文件为权威源，
// 文件里没有就得重新生成（那等于换了 kid，客户端要重新拉公钥）。
func (s *PasswordKeyStore) PersistPrivate(path string) error {
	pemBytes, err := encodePasswordPrivateKey(s.priv)
	if err != nil {
		return err
	}
	return fsatomic.Write(path, pemBytes, 0o600)
}

// passwordKeyID 由公钥派生 kid。
//
// 形如 "pk-<16 个 base64url 字符>"：同一把钥匙重启后 kid 不变，
// 换一把钥匙 kid 就变——客户端据此发现轮换。
func passwordKeyID(pub []byte) string {
	sum := sha256.Sum256(pub)
	return "pk-" + base64.RawURLEncoding.EncodeToString(sum[:])[:16]
}

// generatePasswordPrivateKey 生成一个新的 X25519 私钥。
func generatePasswordPrivateKey() (*ecdh.PrivateKey, error) {
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("生成 X25519 私钥: %w", err)
	}
	return priv, nil
}

// encodePasswordPrivateKey 把私钥编码成 PEM（PKCS#8）。
func encodePasswordPrivateKey(priv *ecdh.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("编码口令加密私钥: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// parsePasswordPrivateKey 解析 PEM 编码的 X25519 私钥。
func parsePasswordPrivateKey(data []byte, source string) (*ecdh.PrivateKey, error) {
	blk, _ := pem.Decode(data)
	if blk == nil || blk.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("%s 的 PEM 格式不正确", source)
	}
	key, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("解析 %s 的私钥: %w", source, err)
	}
	priv, ok := key.(*ecdh.PrivateKey)
	if !ok || priv.Curve() != ecdh.X25519() {
		return nil, fmt.Errorf("%s 不是 X25519 私钥", source)
	}
	return priv, nil
}

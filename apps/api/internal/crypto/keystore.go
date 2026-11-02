// Package crypto 提供 Ed25519 密钥库、JWKS 派生与 registry token 签发验签。
package crypto

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/lau6code/lau6-cr/apps/api/internal/cache"
	"github.com/lau6code/lau6-cr/apps/api/internal/fsatomic"
)

// 缓存中的密钥相关键名。
//
// ⚠️ 这些是**应用层**的键名；Redis 后端还会在前面加实例前缀
// （cache.redis.prefix，默认 "lau6-cr:"），多实例共用一个 Redis 时靠它隔离。
const (
	// CacheKeySigningKey 是签名私钥的缓存键。
	//
	// ⚠️ **不设 TTL**（docs/design/12-cache.md §4.2）：
	//    它是**启动源**而不是热点数据。设 TTL 等于给"全体登出"埋雷——
	//    私钥被淘汰 → 下次启动读不到 → 重新生成 → 全体用户的 token 作废。
	CacheKeySigningKey = "cr:signing-key"

	// CacheKeyJWKS 是 JWKS 文档的缓存键。
	CacheKeyJWKS = "cr:jwks"

	// jwksTTL 是 JWKS 缓存项的 TTL。
	//
	// 15 分钟：必须 > token TTL（300s），取 3× 余量。
	// 见 docs/design/12-cache.md §4.2 / §4.3。
	jwksTTL = 15 * time.Minute
)

// Keystore 持有签名私钥及其派生信息。
//
// 私钥是**唯一真源**：公钥不单独落盘，JWKS 每次启动从私钥重新派生并原子写盘。
type Keystore struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
	kid  string
}

// Options 是密钥库的构造参数。
type Options struct {
	// PrivatePath 是签名私钥的文件路径（0600）。
	PrivatePath string
	// JWKSPath 是 JWKS 文档的文件路径（0644——公钥本来就是公开的）。
	JWKSPath string
	// Cache 是缓存后端（可为 nil → 只用文件）。
	//
	// ⚠️ 非 nil 且 driver=redis 时，**缓存是权威源**
	//（docs/design/12-cache.md §3.3）：
	//
	//	缓存里有        → 载入内存（**不读文件**）
	//	缓存里没有      → 生成 → 写回缓存 + 写一份文件（审计副本）
	//	缓存连不上      → **启动失败**（fail-closed）
	//
	// fail-closed 是硬要求：降级成"连不上就生成新密钥"，
	// Redis 每次重启都会让全体用户重新登录——
	// 一次缓存抖动被放大成可用性事故。
	Cache cache.Cache
}

// NewKeystore 按配置构造密钥库。
func NewKeystore(o Options) (*Keystore, error) {
	ctx := context.Background()
	priv, err := o.loadPrivateKey(ctx)
	if err != nil {
		return nil, err
	}
	pub := priv.Public().(ed25519.PublicKey)
	ks := &Keystore{priv: priv, pub: pub, kid: JWKThumbprint(pub)}
	if err := ks.writeJWKS(o.JWKSPath, o.Cache); err != nil {
		return nil, err
	}
	return ks, nil
}

// loadPrivateKey 按 §3.3 的存放策略取私钥。
func (o Options) loadPrivateKey(ctx context.Context) (ed25519.PrivateKey, error) {
	return loadKey(ctx, o.Cache, keySource[ed25519.PrivateKey]{
		label:    "签名密钥",
		cacheKey: CacheKeySigningKey,
		filePath: o.PrivatePath,
		parse:    parsePrivateKey,
		generate: generatePrivateKey,
		encode:   encodePrivateKey,
	})
}

// Public 返回公钥。
func (k *Keystore) Public() ed25519.PublicKey { return k.pub }

// KID 返回 JWK thumbprint（RFC 7638）。
func (k *Keystore) KID() string { return k.kid }

// SeedCache 把"不可丢"的键（签名私钥、JWKS）写进给定的缓存后端。
//
// 用途：**换缓存后端**时把密钥搬过去。
//
// ⚠️ 不搬的后果：下次启动时新后端里没有密钥 → 按 §3.3 重新生成 →
//
//	**全体用户被登出**（token 最多 5 分钟自愈，但用户会以为登录坏了）。
//
// 这也是向导里 local → redis 热切换必须做的事。
func (k *Keystore) SeedCache(ctx context.Context, c cache.Cache) error {
	if c == nil {
		return nil
	}
	pemBytes, err := encodePrivateKey(k.priv)
	if err != nil {
		return err
	}
	// ⚠️ 不设 TTL（§4.2）。
	if err := c.Set(ctx, CacheKeySigningKey, pemBytes, 0); err != nil {
		return fmt.Errorf("写签名密钥到缓存: %w", err)
	}
	jwks, err := k.JWKSBytes()
	if err != nil {
		return err
	}
	if err := c.Set(ctx, CacheKeyJWKS, jwks, jwksTTL); err != nil {
		return fmt.Errorf("写 JWKS 到缓存: %w", err)
	}
	return nil
}

// PersistPrivate 把私钥写到文件（0600）。
//
// 用途：换缓存后端时切回 local——local 模式以文件为权威源，
// 文件里没有就得重新生成（那等于全体登出）。
func (k *Keystore) PersistPrivate(path string) error {
	pemBytes, err := encodePrivateKey(k.priv)
	if err != nil {
		return err
	}
	return fsatomic.Write(path, pemBytes, 0o600)
}

// JWKThumbprint 计算 RFC 7638 的 JWK thumbprint。
//
// OKP 类型的规范化形式只含 crv / kty / x 三个成员，**顺序固定**——
// 顺序错了算出来的 thumbprint 就对不上。
func JWKThumbprint(pub ed25519.PublicKey) string {
	canonical := `{"crv":"Ed25519","kty":"OKP","x":"` +
		base64.RawURLEncoding.EncodeToString(pub) + `"}`
	sum := sha256.Sum256([]byte(canonical))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

type jwk struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Kid string `json:"kid"`
	Use string `json:"use,omitempty"`
	Alg string `json:"alg,omitempty"`
}

type jwkSet struct {
	Keys []jwk `json:"keys"`
}

// JWKSBytes 返回 JWKS 文档内容（仅公钥）。
//
// ⚠️ 错误必须返回：marshal 失败时若返回 nil，
//
//	/jwks.json 会写出**空 body + 200**——
//	上游 distribution 验签直接失败，而日志里什么都没有（AGENTS.md 规则 1 / 3）。
func (k *Keystore) JWKSBytes() ([]byte, error) {
	doc := jwkSet{Keys: []jwk{{
		Kty: "OKP",
		Crv: "Ed25519",
		X:   base64.RawURLEncoding.EncodeToString(k.pub),
		Kid: k.kid,
		Use: "sig",
		Alg: "EdDSA",
	}}}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("序列化 JWKS: %w", err)
	}
	return b, nil
}

// writeJWKS 把 JWKS 写进缓存（redis 模式）与文件。
func (k *Keystore) writeJWKS(jwksPath string, c cache.Cache) error {
	doc, err := k.JWKSBytes()
	if err != nil {
		return err
	}
	if c != nil && c.Driver() == "redis" {
		if err := c.Set(context.Background(), CacheKeyJWKS, doc, jwksTTL); err != nil {
			return fmt.Errorf("写 JWKS 到缓存: %w", err)
		}
	}
	return fsatomic.Write(jwksPath, doc, 0o644)
}

// generatePrivateKey 生成一个新的 Ed25519 私钥。
func generatePrivateKey() (ed25519.PrivateKey, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("生成 Ed25519 私钥: %w", err)
	}
	return priv, nil
}

// encodePrivateKey 把私钥编码成 PEM（PKCS#8）。
func encodePrivateKey(priv ed25519.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// parsePrivateKey 解析 PEM 编码的私钥。
func parsePrivateKey(data []byte, source string) (ed25519.PrivateKey, error) {
	blk, _ := pem.Decode(data)
	if blk == nil || blk.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("%s 的 PEM 格式不正确", source)
	}
	key, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("解析 %s 的私钥: %w", source, err)
	}
	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s 不是 Ed25519 私钥", source)
	}
	return priv, nil
}

// LoadOrGenerateSecret 读取或生成一个十六进制随机密钥文件（用于 webhook / registry secret）。
func LoadOrGenerateSecret(path string, nBytes int) (string, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		s := strings.TrimSpace(string(b))
		if s != "" {
			return s, nil
		}
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("读取密钥 %s: %w", path, err)
	}
	raw := make([]byte, nBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("生成随机密钥: %w", err)
	}
	s := hex.EncodeToString(raw)
	// 末尾补换行便于 cat 查看；读取方会 TrimSpace。
	if err := fsatomic.Write(path, []byte(s+"\n"), 0o600); err != nil {
		return "", err
	}
	return s, nil
}

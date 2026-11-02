package crypto

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
)

// ErrInvalidToken 表示 token 无法解析或签名不合法。
var ErrInvalidToken = errors.New("invalid token")

// RegistryClaims 是 registry token 的载荷。
//
// 两种签名方式共用同一套载荷：
//   - HS256（用户级）：每个用户一把密钥，签给客户端；
//   - EdDSA（实例级）：实例密钥，签给上游与匿名客户端。
type RegistryClaims struct {
	jwt.RegisteredClaims
	Access []domain.Access `json:"access"`
	// TokenID 是**项目令牌**的 ID。
	//
	// 用户级令牌缺省（0）——它是"用哪把密钥验"的索引：
	// 缺省查用户密钥库，有值查项目密钥库（02-auth.md §2.6.3）。
	TokenID int64 `json:"token_id,omitempty"`
}

// TokenIssuer 签发与校验 registry token。
//
// 它同时管两套密钥：
//
//	实例级 Ed25519（ks）  → 上游转发令牌、匿名令牌、内部调用
//	用户级 HMAC（secrets） → 发给客户端的令牌
//
// ⚠️ 两者不能互换：distribution 只认 JWKS 里的 EdDSA 公钥，
//
//	客户端令牌（HS256）到上游必须换发（见 service.RegistryService.stream）。
type TokenIssuer struct {
	ks      *Keystore
	secrets *UserSecretStore
	// projectSecrets 是每枚项目令牌自己的签名密钥库。
	//
	// 用户级与项目级是两套独立的密钥体系：
	// 用户密钥按用户名索引，项目令牌密钥按令牌 ID 索引。
	projectSecrets *ProjectSecretStore
	issuer         string
	audience       string
	// ttl 是实例级令牌（上游转发 / 匿名）的有效期。
	//
	// ⚠️ 用函数而不是值：core.token_ttl_seconds 是 T0 热生效设置，
	//    启动时把 TTL 冻住的话，设置页上的 T0 就是个假开关
	//    （能改、能显示"已保存"，运行期却还是旧窗口）。
	ttl func() time.Duration
}

// NewTokenIssuer 构造签发器。
//
// ttl 用函数传入（读当前生效的设置快照），不得小于 1 分钟——
// distribution 会拒绝过短窗口。
//
// ⚠️ 窗口下限在**两个时点**各校验一次：
//   - 构造时：配置写错要在启动时暴露，而不是等第一次签发才发现；
//   - 签发时：运行期把 TTL 改小同样要当场报错，
//     不能发一枚必然被上游拒绝的令牌。
//
// projectSecrets 传 nil 时**不报错**——单测与 CLI 子命令可能用不到项目令牌；
// 但那时签发项目令牌会当场失败（见 IssueProject），不会悄悄发出一枚无人能验的令牌。
func NewTokenIssuer(ks *Keystore, secrets *UserSecretStore, projectSecrets *ProjectSecretStore, issuer, audience string, ttl func() time.Duration) (*TokenIssuer, error) {
	if ttl == nil {
		return nil, errors.New("令牌有效期未配置")
	}
	if d := ttl(); d < time.Minute {
		return nil, fmt.Errorf("token TTL 不得小于 1 分钟，当前 %s", d)
	}
	if secrets == nil {
		// 不给默认值：用户级密钥库缺了会在**启动时**暴露，
		// 而不是等到第一个用户来换令牌时才 500。
		return nil, errors.New("用户密钥库未配置")
	}
	return &TokenIssuer{
		ks:             ks,
		secrets:        secrets,
		projectSecrets: projectSecrets,
		issuer:         issuer,
		audience:       audience,
		ttl:            ttl,
	}, nil
}

// TTL 返回实例级令牌的有效期。
func (i *TokenIssuer) TTL() time.Duration { return i.ttl() }

// ExpiresIn 返回实例级令牌的有效秒数。
func (i *TokenIssuer) ExpiresIn() int { return int(i.ttl() / time.Second) }

// UserExpiresIn 返回客户端令牌（用户级）的有效秒数。
//
// 用户级令牌与用户密钥**共用**一个有效期（core.user_token_ttl_seconds）：
// 令牌不会活得比它的密钥久，两者分开设只会出现"令牌还在有效期内却验不过"。
func (i *TokenIssuer) UserExpiresIn() int { return int(i.secrets.TTL() / time.Second) }

// ProjectExpiresIn 返回项目令牌的客户端令牌有效秒数。
//
// 项目令牌密钥与用户密钥是**两套独立的体系**，有效期各看各的设置。
func (i *TokenIssuer) ProjectExpiresIn() int {
	if i.projectSecrets == nil {
		return 0
	}
	return int(i.projectSecrets.TTL() / time.Second)
}

// Issue 签发一枚带 access scope 的**实例级**令牌（EdDSA）。
//
// 用于三处：
//   - 转发给上游 distribution 的令牌；
//   - 匿名客户端的令牌（匿名没有用户身份，不需要用户级密钥）；
//   - 内部调用（refresh / reconciler / 标签守卫）。
//
// access 为空是合法情形——用于 docker login 的探活与"无权限"场景，
// 统一返回 200 + 空 access，让客户端无法据此探测仓库是否存在。
func (i *TokenIssuer) Issue(subject string, access []domain.Access) (string, error) {
	ttl := i.ttl()
	if ttl < time.Minute {
		// 运行期把 TTL 改到 1 分钟以下：当场报错。
		// 发一枚上游必然拒绝的令牌，只会把问题推到更远的地方。
		return "", fmt.Errorf("token TTL 不得小于 1 分钟，当前 %s", ttl)
	}
	now := time.Now()
	claims, err := i.mintClaims(subject, access, now, now.Add(ttl))
	if err != nil {
		return "", err
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	tok.Header["kid"] = i.ks.KID()
	s, err := tok.SignedString(i.ks.priv)
	if err != nil {
		return "", fmt.Errorf("签发 token: %w", err)
	}
	return s, nil
}

// IssueUser 用**该用户自己的**密钥签发一枚客户端令牌（HS256）。
//
// 为什么客户端令牌不直接用实例密钥：
// 实例密钥只有一把，任何一枚令牌泄漏都只能靠轮换实例密钥止血——
// 而轮换会把**所有人**的令牌一起作废（02-auth.md §2.5）。
// 每用户一把密钥才能做到"只吊销这一个人"。
//
// 代价：distribution 只认 JWKS 里的 EdDSA 公钥，认不了 HS256。
// 所以这枚令牌**不能**直接转发给上游——转发前必须换发实例级令牌。
//
// 有效期：令牌不会活得比它的密钥久——密钥 30 天到期，
// 这枚令牌就跟着作废（验签时找不到密钥）。
func (i *TokenIssuer) IssueUser(ctx context.Context, subject string, access []domain.Access) (string, error) {
	if strings.TrimSpace(subject) == "" {
		return "", errors.New("签发用户令牌：用户名为空")
	}
	rec, err := i.secrets.Ensure(ctx, subject)
	if err != nil {
		return "", err
	}

	now := time.Now()
	expires := now.Add(i.secrets.TTL())
	// 密钥的寿命是硬上限：令牌不能比它的密钥活得久，
	// 否则会出现"令牌还在有效期里但已经验不过"的怪现象。
	if limit := rec.CreatedAt.Add(i.secrets.TTL()); limit.Before(expires) {
		expires = limit
	}

	claims, err := i.mintClaims(subject, access, now, expires)
	if err != nil {
		return "", err
	}
	s, err := signHS256(claims, rec.Secret)
	if err != nil {
		return "", fmt.Errorf("签发用户令牌: %w", err)
	}
	return s, nil
}

// IssueProject 用**该项目令牌自己的**密钥签发一枚客户端令牌（HS256）。
//
// 为什么不用实例级 Ed25519 密钥：实例密钥只有一把，吊销不了单枚令牌——
// 令牌行删了，但之前换发出去的 JWT 在有效期内**仍然能用**。
// 每枚令牌一把密钥才能做到"吊销立即生效"（02-auth.md §2.6.3）。
//
// subject 传 `project:<项目名>`：它同时是审计里的 actor 与
// 转发给上游的令牌 subject——上游只看得到一个不透明的字符串。
//
// 有效期：令牌不会活得比它的密钥久——密钥到期即该令牌换发的所有 JWT 作废。
func (i *TokenIssuer) IssueProject(ctx context.Context, tokenID int64, subject string, access []domain.Access) (string, error) {
	if i.projectSecrets == nil {
		return "", errors.New("签发项目令牌：项目令牌密钥库未配置")
	}
	if tokenID <= 0 {
		return "", errors.New("签发项目令牌：令牌 ID 无效")
	}
	if strings.TrimSpace(subject) == "" {
		return "", errors.New("签发项目令牌：主体为空")
	}
	rec, err := i.projectSecrets.Ensure(ctx, tokenID)
	if err != nil {
		return "", err
	}

	now := time.Now()
	expires := now.Add(i.projectSecrets.TTL())
	if limit := rec.CreatedAt.Add(i.projectSecrets.TTL()); limit.Before(expires) {
		expires = limit
	}

	claims, err := i.mintClaims(subject, access, now, expires)
	if err != nil {
		return "", err
	}
	claims.TokenID = tokenID
	s, err := signHS256(claims, rec.Secret)
	if err != nil {
		return "", fmt.Errorf("签发项目令牌: %w", err)
	}
	return s, nil
}

// mintClaims 拼出令牌载荷。
//
// 三处签发（实例级 / 用户级 / 项目级）的载荷结构完全一样，
// 差别只在签发算法与密钥上——那些留在各自的调用方，这里只管拼载荷。
//
// access 为 nil 时补成空切片：载荷里少一个 null 与空数组，
// 上游与客户端解析的口径不一致。
func (i *TokenIssuer) mintClaims(subject string, access []domain.Access, now, expires time.Time) (RegistryClaims, error) {
	jti := make([]byte, 16)
	if _, err := rand.Read(jti); err != nil {
		return RegistryClaims{}, fmt.Errorf("生成 jti: %w", err)
	}
	claims := RegistryClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    i.issuer,
			Subject:   subject,
			Audience:  jwt.ClaimStrings{i.audience},
			ExpiresAt: jwt.NewNumericDate(expires),
			NotBefore: jwt.NewNumericDate(now.Add(-5 * time.Second)), // 时钟偏移容忍
			IssuedAt:  jwt.NewNumericDate(now),
			ID:        base64.RawURLEncoding.EncodeToString(jti),
		},
		Access: access,
	}
	if claims.Access == nil {
		claims.Access = []domain.Access{}
	}
	return claims, nil
}

// signHS256 用 HS256 密钥签发载荷。
//
// 用户级与项目级令牌都用它——两者的差别只在密钥来源，不在签名方式。
func signHS256(claims RegistryClaims, secret string) (string, error) {
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	s, err := tok.SignedString([]byte(secret))
	if err != nil {
		return "", err
	}
	return s, nil
}

// Verify 校验 token 并返回载荷。
//
// 按 **alg** 分派密钥：
//
//	HS256 → 用户自己的密钥（从用户密钥库取）
//	       或项目令牌自己的密钥（claims.TokenID 有值时从项目密钥库取）
//	EdDSA → 实例公钥（并强制 kid 与当前密钥一致）
//
// ⚠️ 算法是**锁定**的（WithValidMethods）：alg=none 与算法混淆都被拒。
// 密钥类型也天然隔离：jwt-go 拿到 []byte 只会验 HMAC，
// 拿到 ed25519 公钥只会验 EdDSA——把 HS256 令牌改成 EdDSA 会直接验不过。
func (i *TokenIssuer) Verify(ctx context.Context, raw string) (*RegistryClaims, error) {
	claims := &RegistryClaims{}
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg(), jwt.SigningMethodEdDSA.Alg()}),
		jwt.WithIssuer(i.issuer),
		jwt.WithAudience(i.audience),
		jwt.WithLeeway(5*time.Second),
	)
	_, err := parser.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		switch t.Method.Alg() {
		case jwt.SigningMethodHS256.Alg():
			// claims 在 keyfunc 被调用时**已经解码**（尚未验签）。
			// 这里的 subject 只是"用哪把密钥验"的索引，
			// 真正的授权结论仍然来自验签本身：
			// 把 sub 改成别人，签名就对不上了。
			if claims.Subject == "" {
				return nil, ErrInvalidToken
			}
			// token_id 有值 → 项目令牌，用它自己那把密钥；
			// 缺省 → 用户令牌，用该用户的密钥。
			//
			// ⚠️ 分派依据是**载荷里的 token_id**，不是令牌前缀：
			// JWT 本身没有前缀，它在 /token 换发时就已经是 JWT 了。
			if claims.TokenID != 0 {
				if i.projectSecrets == nil {
					return nil, ErrInvalidToken
				}
				rec, ok, err := i.projectSecrets.Get(ctx, claims.TokenID)
				if err != nil {
					return nil, fmt.Errorf("读取项目令牌密钥: %w", err)
				}
				if !ok {
					// 密钥不存在或已过期 → 这枚令牌已经作废。
					return nil, ErrInvalidToken
				}
				return []byte(rec.Secret), nil
			}
			rec, ok, err := i.secrets.Get(ctx, claims.Subject)
			if err != nil {
				return nil, fmt.Errorf("读取用户密钥: %w", err)
			}
			if !ok {
				// 密钥不存在或已过期 → 这枚令牌已经作废。
				return nil, ErrInvalidToken
			}
			return []byte(rec.Secret), nil
		case jwt.SigningMethodEdDSA.Alg():
			kid, _ := t.Header["kid"].(string)
			if kid == "" || kid != i.ks.KID() {
				return nil, ErrInvalidToken
			}
			return i.ks.priv.Public(), nil
		}
		return nil, ErrInvalidToken
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	return claims, nil
}

// AllowsAccess 判断载荷是否对指定仓库授予了指定动作。
//
// 单独的纯函数：调用方常常**已经验过签**（拿到 claims），
// 这时不该为了判权限再验一遍——用户级令牌每验一次都要回缓存取一次密钥。
func AllowsAccess(claims *RegistryClaims, repository, action string) bool {
	if claims == nil {
		return false
	}
	for _, a := range claims.Access {
		if a.Type != "repository" || a.Name != repository {
			continue
		}
		for _, granted := range a.Actions {
			if granted == action || granted == "*" {
				return true
			}
		}
	}
	return false
}

// NewRegistryClaims 造一份只带身份与权限的载荷。
//
// 它**不是**一枚可用的令牌——没有 exp/iat/jti，签不出去。
// 用途是：认证路径已经知道"谁、对哪个仓库、什么动作"，
// 要把它换发成上游令牌时用（上游只认 EdDSA，用户级 HS256 令牌它验不了）。
func NewRegistryClaims(subject string, access []domain.Access) *RegistryClaims {
	if access == nil {
		access = []domain.Access{}
	}
	return &RegistryClaims{
		RegisteredClaims: jwt.RegisteredClaims{Subject: subject},
		Access:           access,
	}
}

// ParseBearer 从 Authorization 头里取 bearer token。
func ParseBearer(header string) (string, error) {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", ErrInvalidToken
	}
	tok := strings.TrimSpace(header[len(prefix):])
	if tok == "" {
		return "", ErrInvalidToken
	}
	return tok, nil
}

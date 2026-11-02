package service

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/lau6code/lau6-cr/apps/api/internal/crypto"
	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
)

// ErrLegacyTokenPrefix 表示凭据用了**已停用的令牌前缀**（`cr_pat_`）。
//
// 散列是原像：前缀变了散列必然跟着变，库里查不到这种前缀的令牌，
// 所以带 `cr_pat_` 的凭据一律不可用。
//
// ⚠️ 必须在**验签 / 口令校验之前**判定：
// 旧前缀的令牌既不是 JWT，也不是账户口令——
// 走验签只能得到笼统的"令牌无效"，走口令校验还会撞上 LDAP 自动建号。
//
// ⚠️ 必须报**明确原因**，不能笼统地说"令牌无效"：
// 后者会让用户以为令牌被吊销了，去查权限、翻审计，越查越找不到原因。
var ErrLegacyTokenPrefix = errors.New("令牌前缀已变更（cr_pat_ → lau6_u_），请重新生成")

// TokenService 实现 Docker CLI 的 token realm。
type TokenService struct {
	perm   *PermissionEngine
	issuer *crypto.TokenIssuer
	auth   *AuthService
	// pats 是个人令牌服务——realm 接受个人令牌作为凭据。
	pats *PersonalTokenService
	// projTokens 是项目令牌服务——realm 接受项目令牌作为凭据。
	projTokens *ProjectTokenService
}

// NewTokenService 构造 token 服务。
func NewTokenService(perm *PermissionEngine, issuer *crypto.TokenIssuer, auth *AuthService, pats *PersonalTokenService, projTokens *ProjectTokenService) *TokenService {
	return &TokenService{perm: perm, issuer: issuer, auth: auth, pats: pats, projTokens: projTokens}
}

// TokenResult 是签发结果。
type TokenResult struct {
	Token     string
	ExpiresIn int
	// Subject 是 token 的 subject（用户名，匿名时是 "anonymous"）。
	//
	// 记日志用：token 本身是 JWT，排查时需要知道它是谁的。
	Subject string
	// Granted 是本次实际授予的访问列表。
	//
	// 请求的 scope 与实际授予**不一定相同**（越权部分被裁掉），
	// 两者都记下来才能解释"为什么客户端说没权限"。
	Granted []domain.Access
}

// ProjectNotFoundError 表示请求的项目（Docker 命名空间）不存在。
//
// 它是**操作前提**问题而不是权限问题：调用方需要先建项目。
// 已认证的调用方直说，避免它拿着一枚空 access 的 token 去反复重试。
type ProjectNotFoundError struct {
	Project string
}

func (e *ProjectNotFoundError) Error() string {
	return fmt.Sprintf("未经授权: 项目 %s 不存在", e.Project)
}

// AnonymousWriteScopeError 表示匿名主体申请了它**永远不可能拥有**的写权限。
//
// 匿名能不能 push / delete 与仓库存不存在无关（匿名永远不能），
// 所以在这里拒掉**不泄露任何仓库存在性**。
type AnonymousWriteScopeError struct {
	// Scope 是原始 scope 串。
	Scope string
	// Action 是被拒的动作（push / delete）。
	Action string
}

func (e *AnonymousWriteScopeError) Error() string {
	return fmt.Sprintf("未经授权: 匿名主体不能 %s（需要登录）", e.Action)
}

// 匿名只能 pull（§4.5）。push / delete 走的是"有身份才谈得上"的路径，
// 对匿名主体一律不可能。
func impossibleAnonymousAction(actions []string) string {
	for _, act := range actions {
		switch Action(act) {
		case ActionPush:
			return string(ActionPush)
		case ActionDelete:
			return string(ActionDelete)
		}
	}
	return ""
}

// Issue 根据 Basic Auth 与 scope 参数签发 registry token。
//
// 关键行为：
//   - **无凭据也签发**，只是 access 为空或只含公开仓库的 pull。
//     这让 `docker login` 的探活能成功，也让公开镜像免登录可拉。
//   - 凭据错误时**返回 401**，绝不降级为匿名——
//     否则用户密码打错会静默退化成"以公开身份操作"。
//   - scope 全部被拒时**照样签发空 access**，不泄露仓库是否存在。
//     **例外**：已认证请求且项目根本不存在 → 直接报"项目 xxx 不存在"
//     （见 ProjectNotFoundError）；未认证请求仍不暴露，
//     那是未认证攻击面，不能用来探测私有项目名。
func (s *TokenService) Issue(ctx context.Context, basicAuth string, scopes []string) (*TokenResult, error) {
	principal := Principal{}
	subject := anonymousSubject

	if basicAuth != "" {
		username, password, ok := parseBasicAuth(basicAuth)
		if !ok {
			return nil, ErrInvalidCredentials
		}
		switch {
		case crypto.IsPersonalToken(password):
			// 个人令牌（PAT）作为凭据
			//
			// 用户在 CI / 脚本里用个人令牌代替账户口令：
			// `docker login <registry> -u <任意用户名> -p <个人令牌>`。
			//
			// ⚠️ **不走口令校验**——VerifyCredentials 会在用户不存在时
			//    触发 LDAP 自动建号，拿令牌当口令去撞那条路径是错的。
			//
			// ⚠️ **不校验 Basic 里的用户名**：令牌本身就是完整的凭据
			//    （口径与 apc 一致）。审计记录的是令牌的**属主**，
			//    所以"谁真的持有凭据"在审计里不会含糊。
			user, _, err := s.pats.Verify(ctx, password)
			if err != nil {
				return nil, err
			}
			principal = Principal{User: user}
			subject = user.Username
		case crypto.IsProjectToken(password):
			// 项目令牌作为凭据
			//
			// 同样不走口令校验，也不校验 Basic 里的用户名：
			// 项目令牌本身就是完整的凭据。
			pt, err := s.projTokens.Verify(ctx, password)
			if err != nil {
				return nil, err
			}
			principal = Principal{Project: pt}
			subject, err = s.projTokens.Subject(ctx, pt)
			if err != nil {
				return nil, err
			}
		case crypto.IsLegacyPersonalToken(password):
			// 旧前缀：直接报"前缀已变更"，不查库。
			//
			// ⚠️ 必须在 VerifyCredentials **之前**判定：
			//    否则旧令牌会被当成账户口令去撞 bcrypt / LDAP 自动建号。
			return nil, ErrLegacyTokenPrefix
		default:
			res, err := s.auth.VerifyCredentials(ctx, username, password)
			if err != nil {
				return nil, err
			}
			principal = Principal{User: res.User}
			subject = res.User.Username
		}
	}

	// 逐条解析 scope，交给权限引擎判定。
	access := make([]domain.Access, 0, len(scopes))
	for _, raw := range scopes {
		sc, ok := ParseScope(raw)
		if !ok || sc.Type != "repository" {
			continue
		}
		// 匿名主体**永远不可能**有 push / delete。
		//
		// ⚠️ 这里必须**拒**，不能"签一枚降级 token"：
		//
		//	客户端拿到只有 pull 的匿名 token → 拿着它继续推 →
		//	registry 回 401 + 挑战 → 客户端按挑战再去换 token → **死循环**。
		//	（现象：日志里一串同一条 push 拒绝，间隔几秒一轮，永不停。）
		//
		//	在换 token 这一步就拒掉，客户端立刻带着明确原因停下——
		//	与下面"项目不存在"的处理同源。
		//
		//	拒它**不泄露仓库存在性**：匿名能不能 push 与仓库存不存在无关
		//	（匿名永远不能），两种情况都是同一个 401。
		if principal.IsAnonymous() {
			if act := impossibleAnonymousAction(sc.Actions); act != "" {
				return nil, &AnonymousWriteScopeError{Scope: raw, Action: act}
			}
		}
		d, err := s.perm.Decide(ctx, principal, sc.Name, sc.Actions)
		if err != nil {
			return nil, err
		}
		if len(d.Granted) == 0 {
			// 项目不存在 → 对**已认证**调用方直说。
			//
			// 为什么不能只签空 access 就算了：客户端拿到空 access 后
			// 会在 /v2/ 上反复重试（每次都是 403），
			// 运维看到的只是一串 403 日志，看不出真正原因。
			if d.Reason == ReasonProjectNotFound && !principal.IsAnonymous() {
				return nil, &ProjectNotFoundError{Project: d.ReasonProject}
			}
			continue
		}
		access = append(access, domain.Access{
			Type: "repository", Name: sc.Name, Actions: d.Granted,
		})
	}

	// 签发
	//
	// 用户 → **用户级令牌**（HS256，用该用户自己的密钥）：
	// 每把密钥只服务一个用户，吊销一个人的密钥不会影响别人。
	//
	// 项目令牌 → **项目级令牌**（HS256，用该令牌自己的密钥）：
	// 吊销一枚令牌只作废它自己（02-auth.md §2.6.3）。
	//
	// 匿名 → **实例级令牌**（EdDSA）：
	// 匿名没有用户身份，也就无所谓"每用户一把钥匙"；
	// 用实例密钥签还能少一次缓存往返。
	var tok string
	var err error
	expiresIn := s.issuer.ExpiresIn()
	switch {
	case principal.Project != nil:
		tok, err = s.issuer.IssueProject(ctx, principal.Project.ID, subject, access)
		expiresIn = s.issuer.ProjectExpiresIn()
	case principal.IsAnonymous():
		tok, err = s.issuer.Issue(subject, access)
	default:
		tok, err = s.issuer.IssueUser(ctx, subject, access)
		expiresIn = s.issuer.UserExpiresIn()
	}
	if err != nil {
		return nil, err
	}
	return &TokenResult{
		Token:     tok,
		ExpiresIn: expiresIn,
		Subject:   subject,
		Granted:   access,
	}, nil
}

// 前缀匹配**大小写不敏感**（RFC 7235 规定 scheme 不区分大小写）。
func parseBasicAuth(header string) (username, password string, ok bool) {
	const prefix = "Basic "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", "", false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(header[len(prefix):]))
	if err != nil {
		return "", "", false
	}
	s := string(raw)
	idx := strings.IndexByte(s, ':')
	if idx < 0 {
		return "", "", false
	}
	return s[:idx], s[idx+1:], true
}

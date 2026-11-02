package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/lau6code/lau6-cr/apps/api/internal/crypto"
	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
	"github.com/lau6code/lau6-cr/apps/api/internal/store"
)

// ErrInvalidProjectToken 表示项目令牌无效、已过期或已被吊销。
var ErrInvalidProjectToken = errors.New("invalid project token")

// ProjectTokenService 管理项目访问令牌。
//
// 项目令牌是**项目身份**的长期凭据：项目管理员生成后交给 CI / 流水线，
// 让自动化以项目身份推拉镜像，不必占用某个人的个人账号。
//
// 口径参考实现 apc 的 `personal_token`：
//
//	原始令牌 = lau6_p_ + base64url(32 字节随机数)
//	入库     = SHA-256(原始令牌) 的十六进制
//	展示     = lau6_p_ + 前 6 位
//
// ⚠️ **原始令牌只返回一次**：之后既取不回来，也没法从库里还原。
//
//	这是"数据库泄露也拿不到可用凭据"的前提。
//
// ⚠️ 与个人令牌的**根本区别**：
//
//	个人令牌不带 scope，权限按用户的**当前**角色实时算；
//	项目令牌带一个创建时选定、**此后不变**的角色（02-auth.md §2.6.2）。
type ProjectTokenService struct {
	db *store.DB
	// ttl 取值是函数：设置页改了 core.project_token_ttl_seconds 后立即生效。
	ttl func() time.Duration
	// secrets 是项目令牌的签名密钥库。
	//
	// 吊销令牌时必须**同时**删掉它的密钥：否则那枚令牌换发出去的
	// JWT 在有效期内仍然能用（密钥还在，验签照样过）。
	secrets *crypto.ProjectSecretStore
	// logger 记录"可容忍但不该静默"的失败（如最近使用时间写不进去）。
	logger Logger
}

// ProjectTokenOptions 是 ProjectTokenService 的构造参数。
type ProjectTokenOptions struct {
	// TTL 是项目令牌的有效期；传 nil 时用 DefaultProjectTokenTTL。
	TTL func() time.Duration
	// Secrets 是项目令牌的签名密钥库。
	//
	// **必填**：吊销令牌要靠它删密钥，缺了它"吊销"就只是删一行库记录，
	// 那枚令牌换发的 JWT 还会继续用——吊销形同虚设。
	Secrets *crypto.ProjectSecretStore
	// Logger 可选：用于记录被容忍的失败。
	Logger Logger
}

// DefaultProjectTokenTTL 是项目令牌的默认有效期（30 天）。
//
// 与个人令牌同寿命口径（02-auth.md §2.6.2）。
const DefaultProjectTokenTTL = 30 * 24 * time.Hour

// NewProjectTokenService 构造项目令牌服务。
func NewProjectTokenService(db *store.DB, o ProjectTokenOptions) *ProjectTokenService {
	ttl := o.TTL
	if ttl == nil {
		ttl = func() time.Duration { return DefaultProjectTokenTTL }
	}
	return &ProjectTokenService{db: db, ttl: ttl, secrets: o.Secrets, logger: o.Logger}
}

func (s *ProjectTokenService) TTL() time.Duration {
	return tokenTTL(s.ttl, DefaultProjectTokenTTL)
}

// 返回的 raw 是**原始令牌**，只在这一次可见。
//
// policy 决定失效策略（见 ExpiryPolicy）：永久 / 指定时间 / 系统默认。
func (s *ProjectTokenService) Create(ctx context.Context, projectID, createdBy int64, name string, role domain.Role, policy ExpiryPolicy) (string, *domain.ProjectToken, error) {
	if projectID <= 0 {
		return "", nil, errors.New("项目令牌：项目 ID 无效")
	}
	name, err := validateTokenName(name)
	if err != nil {
		return "", nil, err
	}
	if err := validateProjectTokenRole(role); err != nil {
		return "", nil, err
	}
	expiresAt, err := s.resolveExpiry(policy)
	if err != nil {
		return "", nil, err
	}
	raw, hash, prefix, err := crypto.GenerateProjectToken()
	if err != nil {
		return "", nil, err
	}
	tok := &domain.ProjectToken{
		ProjectID:   projectID,
		Name:        name,
		Role:        role,
		TokenHash:   hash,
		TokenPrefix: prefix,
		CreatedBy:   createdBy,
		ExpiresAt:   expiresAt,
	}
	if err := s.db.CreateProjectToken(ctx, tok); err != nil {
		return "", nil, fmt.Errorf("写项目令牌: %w", err)
	}
	return raw, tok, nil
}

// validateProjectTokenRole 校验角色取值。
//
// ⚠️ 这是**权威闸门**：MySQL 上 CHECK 约束可能不被执行，
// 光靠数据库约束等于没有约束。
func validateProjectTokenRole(role domain.Role) error {
	switch role {
	case domain.RoleAdmin, domain.RoleDeveloper, domain.RoleReader:
		return nil
	}
	return fmt.Errorf("项目令牌角色无效: %q（只认 admin / developer / reader）", role)
}

// 返回 **nil = 永久有效**（落 NULL）。
func (s *ProjectTokenService) resolveExpiry(policy ExpiryPolicy) (*int64, error) {
	return resolveTokenExpiry(policy, s.TTL())
}

// SetExpiry 改一枚令牌的失效时间（限本项目）。
//
// 只改失效时间，**不动散列**——令牌本体不变，
// CI 里已经配好的凭据不用重新配置。
//
// 永久 ↔ 定时可以来回改；都不给策略参数 = 重置为系统默认 TTL。
func (s *ProjectTokenService) SetExpiry(ctx context.Context, id, projectID int64, policy ExpiryPolicy) error {
	expiresAt, err := s.resolveExpiry(policy)
	if err != nil {
		return err
	}
	if err := s.db.SetProjectTokenExpiry(ctx, id, projectID, expiresAt); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrInvalidProjectToken
		}
		return err
	}
	return nil
}

func (s *ProjectTokenService) List(ctx context.Context, projectID int64) ([]domain.ProjectToken, error) {
	return s.db.ListProjectTokens(ctx, projectID)
}

// Revoke 吊销一枚令牌（限本项目）。
//
// ⚠️ **必须同时删掉它的签名密钥**：那枚令牌换发出去的 JWT 是用密钥验签的，
// 密钥还在就照样能用——只删库行的话，"吊销"要等到 JWT 过期才真正生效。
func (s *ProjectTokenService) Revoke(ctx context.Context, id, projectID int64) error {
	if s.secrets == nil {
		// 不静默：密钥库没接线 = 吊销做不到"立即生效"，
		// 这时候必须让调用方知道，而不是删一行库记录就说"已吊销"。
		return errors.New("吊销项目令牌：签名密钥库未配置，已换发的 JWT 不会立即失效")
	}
	// 先取令牌，拿到它的 ID 与所属项目——删密钥要用令牌 ID。
	tok, err := s.db.ProjectTokenByID(ctx, id, projectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrInvalidProjectToken
		}
		return err
	}
	if err := s.db.DeleteProjectToken(ctx, id, projectID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrInvalidProjectToken
		}
		return err
	}
	// 密钥删不掉就报错——库里那行已经没了，但 JWT 还能用，
	// 半吊子的吊销比不吊销更危险（用户以为已经安全了）。
	if err := s.secrets.Revoke(ctx, tok.ID); err != nil {
		return fmt.Errorf("吊销项目令牌密钥: %w", err)
	}
	return nil
}

// 用于"这个项目的自动化凭据整体作废"：删除项目时用。
func (s *ProjectTokenService) RevokeAll(ctx context.Context, projectID int64) (int64, error) {
	toks, err := s.db.ListProjectTokens(ctx, projectID)
	if err != nil {
		return 0, err
	}
	n, err := s.db.DeleteProjectTokensOfProject(ctx, projectID)
	if err != nil {
		return 0, err
	}
	if s.secrets != nil {
		for _, t := range toks {
			if err := s.secrets.Revoke(ctx, t.ID); err != nil {
				return n, fmt.Errorf("吊销项目令牌密钥（%d）: %w", t.ID, err)
			}
		}
	}
	return n, nil
}

// 用在"按 token_id 定位具体那枚令牌"的场景：catalog 解析主体、
// 吊销时删签名密钥。一个项目可能有多枚角色不同的令牌，
// 按项目名找会挑错。
func (s *ProjectTokenService) ProjectTokenByID(ctx context.Context, id int64) (*domain.ProjectToken, error) {
	if id <= 0 {
		return nil, ErrInvalidProjectToken
	}
	tok, err := s.db.ProjectTokenByTokenID(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrInvalidProjectToken
		}
		return nil, fmt.Errorf("读项目令牌: %w", err)
	}
	return tok, nil
}

// 形态是 `project:<项目名>`——人类可读，审计里的 actor 与转发给上游的
// 令牌 subject 都用它。上游只看得到一个不透明的字符串，
// 不需要知道它背后是项目还是用户。
func (s *ProjectTokenService) Subject(ctx context.Context, pt *domain.ProjectToken) (string, error) {
	if pt == nil {
		return "", errors.New("项目令牌主体为空")
	}
	project, err := s.db.ProjectByID(ctx, pt.ProjectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", ErrInvalidProjectToken
		}
		return "", fmt.Errorf("读项目: %w", err)
	}
	return ProjectSubject(project.Name), nil
}

// Verify 校验一枚项目令牌，返回令牌记录。
//
// 令牌不存在 / 已过期 / 项目已删 → ErrInvalidProjectToken。
//
// ⚠️ **不区分这三种情况**：调用方（认证路径）只需要知道"能不能用"，
//
//	细分原因对攻击者没有价值，对用户也没有可操作性。
func (s *ProjectTokenService) Verify(ctx context.Context, raw string) (*domain.ProjectToken, error) {
	if !crypto.IsProjectToken(raw) {
		return nil, ErrInvalidProjectToken
	}
	hash := crypto.HashProjectToken(raw)
	tok, err := s.db.ProjectTokenByHash(ctx, hash)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrInvalidProjectToken
		}
		return nil, fmt.Errorf("读项目令牌: %w", err)
	}
	// 最近使用时间只是**观测数据**，写失败不影响鉴权结论——
	// 把它当鉴权的一部分会让一次写失败放大成"令牌不可用"。
	// 但"不影响结论"不等于"可以静默"：至少要留一条日志。
	if err := s.db.TouchProjectToken(ctx, tok.ID); err != nil && s.logger != nil {
		s.logger.Warn("更新项目令牌最近使用时间失败", "err", err, "token_id", tok.ID)
	}
	return tok, nil
}

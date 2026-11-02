// Package domain 定义核心领域模型。
package domain

// Role 是项目内成员角色。
type Role string

const (
	RoleAdmin     Role = "admin"     // 项目管理员：项目内全权
	RoleDeveloper Role = "developer" // 开发者：pull / push / delete
	RoleReader    Role = "reader"    // 只读：仅 pull
)

func (r Role) Valid() bool {
	switch r {
	case RoleAdmin, RoleDeveloper, RoleReader:
		return true
	}
	return false
}

// Actions 返回该角色允许的 Docker 动作。
func (r Role) Actions() []string {
	switch r {
	case RoleAdmin, RoleDeveloper:
		return []string{"pull", "push", "delete"}
	case RoleReader:
		return []string{"pull"}
	}
	return nil
}

// Visibility 是项目 / 仓库的可见性。
type Visibility string

const (
	VisibilityPublic  Visibility = "public"
	VisibilityPrivate Visibility = "private"
	// VisibilityInherit 表示"跟随项目"。
	//
	// ⚠️ 它**只存在于 API 层**：数据库里没有这个值——
	//    仓库行用 inherit_visibility 标记"是否跟随项目"，
	//    visibility 列始终存具体的 public/private
	//    （既有 SQL 与索引不用改，CHECK 约束也不用动）。
	//
	// 语义见 02-auth.md §6：公开项目里的镜像默认**跟随项目**，
	// 项目改公开时它自动跟着公开；单个仓库要收紧，项目管理员显式设置。
	VisibilityInherit Visibility = "inherit"
)

// 可见性的来源（API 回给前端用）。
const (
	// VisibilitySourceInherit 表示跟随项目。
	VisibilitySourceInherit = "inherit"
	// VisibilitySourceExplicit 表示仓库被显式设置过。
	VisibilitySourceExplicit = "explicit"
)

// User 是账户。
type User struct {
	ID            int64  `json:"id"`
	Username      string `json:"username"`
	DisplayName   string `json:"display_name"`
	PasswordHash  string `json:"-"`
	Source        string `json:"source"` // local | ldap
	IsSystemAdmin bool   `json:"is_system_admin"`
	Disabled      bool   `json:"disabled"`
	CreatedAt     int64  `json:"created_at"`
	UpdatedAt     int64  `json:"updated_at"`
}

// Project 是组织单元，同时也是 Docker 名称空间的第一段。
type Project struct {
	ID          int64      `json:"-"`
	Name        string     `json:"name"`
	DisplayName string     `json:"display_name"`
	Description string     `json:"description"`
	Visibility  Visibility `json:"visibility"`
	CreatedBy   *int64     `json:"-"`
	CreatedAt   int64      `json:"-"`
	UpdatedAt   int64      `json:"-"`

	MemberCount int `json:"member_count"`
	RepoCount   int `json:"repo_count"`

	// TotalSize 是项目内所有仓库的镜像大小之和（字节）。
	//
	// 数据来自 repo_meta 缓存行（与镜像目录页的"大小"列同一个来源），
	// 所以它反映的是**最近一次刷新**时的统计值，不是实时值。
	TotalSize int64 `json:"total_size"`

	// MyRole 是**当前查看者**在本项目里的角色；nil = 不是成员。
	//
	// 它是**请求相关**的字段：同一个项目对不同查看者返回不同的 my_role。
	// 前端据此决定"编辑项目 / 管理成员"入口是否可见
	// （项目管理员或系统管理员才能编辑项目）。
	MyRole *Role `json:"my_role,omitempty"`
}

// Member 是项目成员关系。
type Member struct {
	ProjectID    int64    `json:"-"`
	UserID       int64    `json:"user_id"`
	Role         Role     `json:"role"`
	RepoPatterns []string `json:"-"`
	CreatedAt    int64    `json:"-"`
	UpdatedAt    int64    `json:"-"`

	// 关联字段（列表时填充）
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
}

// MemberCandidate 是"可加入本项目"的候选用户。
//
// CurrentRole 为空表示尚未加入本项目；非空表示已是成员，
// 取值是其在**该项目**里的当前角色。
type MemberCandidate struct {
	UserID        int64  `json:"user_id"`
	Username      string `json:"username"`
	DisplayName   string `json:"display_name"`
	CurrentRole   *Role  `json:"current_role"`
	Source        string `json:"source"`
	IsSystemAdmin bool   `json:"is_system_admin"`
}

// RepositoryGrant 是**私有仓库**的显式访问授权。
//
// 只对 visibility='private' 的仓库有意义：项目内的非管理员成员
// 想访问私有仓库，除了项目角色还得有这条显式授权（permission.go 第 9 步）。
type RepositoryGrant struct {
	UserID      int64  `json:"user_id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
}

// ProjectMembership 是一条跨项目的成员关系（"项目 × 用户"）。
//
// 与 Member 的区别：Member 是单个项目内的成员，ProjectMembership
// 额外带上项目名，用于一张表里横看所有项目的成员分布。
type ProjectMembership struct {
	ProjectName        string `json:"project"`
	ProjectDisplayName string `json:"project_display_name"`
	UserID             int64  `json:"user_id"`
	Username           string `json:"username"`
	DisplayName        string `json:"display_name"`
	Role               Role   `json:"role"`
	CreatedAt          int64  `json:"created_at"`
}

// Repository 是镜像仓库记录。
type Repository struct {
	ID          int64  `json:"-"`
	ProjectID   int64  `json:"-"`
	ProjectName string `json:"project"`
	// Name 是项目内的相对路径，不直接对外暴露。
	Name string `json:"-"`
	// FullName 是完整的 Docker 仓库名（<project>/<name>），对外字段名是 "name"。
	//
	// ⚠️ 不能让 Name 与 FullName 共用同一个 json tag：
	// encoding/json 遇到标签冲突会把**两个字段都丢掉**，
	// 响应里就完全没有 "name" 了。
	FullName string `json:"name"`
	// Visibility 是**生效**的可见性（跟随项目的已解析成具体值）。
	//
	// 存储层在 SQL 里解析好（见 store/repo.go 的 repoSelect），
	// 调用方拿到的永远是"实际是什么"而不是"从哪来"。
	Visibility Visibility `json:"visibility"`
	// VisibilitySource 是可见性的来源：inherit（跟随项目）/ explicit（显式设置）。
	//
	// 前端据此显示"跟随项目（当前：公开）"，并允许改回显式值。
	VisibilitySource string `json:"visibility_source"`
	Description      string `json:"description"`
	CreatedDigest    string `json:"-"`
	LatestDigest     string `json:"-"`
	PushedAt         *int64 `json:"-"`
	CreatedAt        int64  `json:"-"`
	UpdatedAt        int64  `json:"-"`
}

// RepoMeta 是 Catalog 页用的反规范化缓存。
type RepoMeta struct {
	RepositoryID int64    `json:"-"`
	LatestTag    string   `json:"latest_tag"`
	TagCount     int      `json:"tag_count"`
	Size         int64    `json:"size"`
	Created      string   `json:"created"`
	Platforms    []string `json:"platforms"`
	PullCount    int64    `json:"-"`
	LastPulledAt *int64   `json:"-"`
	RefreshedAt  int64    `json:"-"`
}

// PersonalToken 是用户自己生成的个人访问令牌（PAT）。
//
// 它是一把**长期身份钥匙**：用户在 CI / 脚本里用它推拉需要权限的镜像。
//
// 数据库里**只存散列**（SHA-256），原始值只在创建时返回一次——
// 之后既取不回来，也没法从库里还原。
type PersonalToken struct {
	ID     int64 `json:"id"`
	UserID int64 `json:"-"`
	// Name 是用户给令牌起的名字（便于在列表里区分用途）。
	Name string `json:"name"`
	// TokenHash 是散列，**永不出 API**。
	TokenHash string `json:"-"`
	// TokenPrefix 是展示用的前缀（如 lau6_u_a1b2c3），用来在列表里认人。
	TokenPrefix string `json:"token_prefix"`
	CreatedAt   int64  `json:"created_at"`
	// ExpiresAt 是失效时间（unix 秒）；**nil = 永久有效**。
	//
	// 永久是用户**显式选**的（不是默认值）：创建时要么给失效时间、
	// 要么声明 never_expires，要么走系统默认（core.pat_ttl_seconds）。
	ExpiresAt *int64 `json:"expires_at"`
	// LastUsedAt 是最近一次使用时间；没用过为 nil。
	LastUsedAt *int64 `json:"last_used_at"`
}

// ProjectToken 是**项目身份**的访问令牌。
//
// 它是一把**长期凭据**：项目管理员生成后交给 CI / 流水线，
// 让自动化以项目身份推拉镜像，不必占用某个人的个人账号。
//
// 数据库里**只存散列**（SHA-256），原始值只在创建时返回一次——
// 之后既取不回来，也没法从库里还原。
//
// ⚠️ Role 在创建时选定、**此后不变**：角色即权限（02-auth.md §2.6.2）。
// 它不代表"某个用户在这个项目里的位置"，所以不随成员关系漂移。
type ProjectToken struct {
	ID        int64 `json:"id"`
	ProjectID int64 `json:"-"`
	// Name 是给令牌起的名字（便于在列表里区分用途）。
	Name string `json:"name"`
	// Role 是创建时选定的项目内角色。
	Role Role `json:"role"`
	// TokenHash 是散列，**永不出 API**。
	TokenHash string `json:"-"`
	// TokenPrefix 是展示用的前缀（如 lau6_p_a1b2c3），用来在列表里认人。
	TokenPrefix string `json:"token_prefix"`
	// CreatedBy 是创建人的用户 ID，**只用于审计**——令牌本身不代表这个人。
	CreatedBy int64 `json:"-"`
	CreatedAt int64 `json:"created_at"`
	// ExpiresAt 是失效时间（unix 秒）；**nil = 永久有效**。
	//
	// 永久是用户**显式选**的（不是默认值）：创建时要么给失效时间、
	// 要么声明 never_expires，要么走系统默认（core.project_token_ttl_seconds）。
	ExpiresAt *int64 `json:"expires_at"`
	// LastUsedAt 是最近一次使用时间；没用过为 nil。
	LastUsedAt *int64 `json:"last_used_at"`
}

// AuditLog 是只追加的审计记录。
type AuditLog struct {
	ID       int64  `json:"-"`
	TS       int64  `json:"ts"`
	Actor    string `json:"actor"`
	Action   string `json:"action"`
	Target   string `json:"target"`
	Scope    string `json:"scope"`
	ClientIP string `json:"client_ip"`
	Success  bool   `json:"success"`
	Detail   string `json:"detail"`
}

// Access 是 registry token 里的一条 scope。
type Access struct {
	Type    string   `json:"type"`
	Name    string   `json:"name"`
	Actions []string `json:"actions"`
}

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
)

// members

// AddMember 添加或更新项目成员。
func (d *DB) AddMember(ctx context.Context, projectID, userID int64, role domain.Role, patterns []string) error {
	return addMember(ctx, d.Dialect(), d.Exec, projectID, userID, role, patterns)
}

// AddMember 在事务里添加或更新项目成员。
func (t *Tx) AddMember(ctx context.Context, projectID, userID int64, role domain.Role, patterns []string) error {
	return addMember(ctx, t.Dialect, t.Exec, projectID, userID, role, patterns)
}

// addMember 是 DB.AddMember 与 Tx.AddMember 的共同实现。
//
// upsert 语句在 postgres / sqlite / mysql 三种方言下不同，
// MySQL 不支持 ON CONFLICT，只能退回 ON DUPLICATE KEY。
func addMember(ctx context.Context, dialect Dialect, exec func(context.Context, string, ...any) (sql.Result, error),
	projectID, userID int64, role domain.Role, patterns []string) error {
	if len(patterns) == 0 {
		patterns = []string{}
	}
	b, err := json.Marshal(patterns)
	if err != nil {
		return fmt.Errorf("序列化 repo_patterns: %w", err)
	}
	now := Now()
	_, err = exec(ctx, `INSERT INTO project_members
		(project_id, user_id, role, repo_patterns, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (project_id, user_id) DO UPDATE SET
			role = EXCLUDED.role, repo_patterns = EXCLUDED.repo_patterns, updated_at = EXCLUDED.updated_at`,
		projectID, userID, string(role), string(b), now, now)
	if err == nil {
		return nil
	}
	if dialect != DialectMySQL {
		return err
	}
	_, err = exec(ctx, `INSERT INTO project_members
		(project_id, user_id, role, repo_patterns, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE role = VALUES(role), repo_patterns = VALUES(repo_patterns), updated_at = VALUES(updated_at)`,
		projectID, userID, string(role), string(b), now, now)
	return err
}

// Membership 返回用户在项目中的成员关系；不是成员则返回 nil。
func (d *DB) Membership(ctx context.Context, projectID, userID int64) (*domain.Member, error) {
	m := &domain.Member{}
	var patterns string
	err := d.QueryRow(ctx, `SELECT project_id, user_id, role, repo_patterns, created_at, updated_at
		FROM project_members WHERE project_id = ? AND user_id = ?`, projectID, userID).
		Scan(&m.ProjectID, &m.UserID, &m.Role, &patterns, &m.CreatedAt, &m.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	parsed, err := parseRepoPatterns(patterns)
	if err != nil {
		return nil, fmt.Errorf("读取成员 %d 的成员关系（项目 %d）: %w", userID, projectID, err)
	}
	m.RepoPatterns = parsed
	return m, nil
}

// ListMembers 返回项目成员（含用户信息）。
func (d *DB) ListMembers(ctx context.Context, projectID int64) ([]domain.Member, error) {
	rows, err := d.Query(ctx, `SELECT m.user_id, m.role, m.repo_patterns, m.created_at, m.updated_at,
		u.username, u.display_name
		FROM project_members m JOIN users u ON u.id = m.user_id
		WHERE m.project_id = ? ORDER BY u.username`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Member
	for rows.Next() {
		var m domain.Member
		var patterns string
		if err := rows.Scan(&m.UserID, &m.Role, &patterns, &m.CreatedAt, &m.UpdatedAt,
			&m.Username, &m.DisplayName); err != nil {
			return nil, err
		}
		parsed, err := parseRepoPatterns(patterns)
		if err != nil {
			return nil, fmt.Errorf("读取项目 %d 的成员列表: %w", projectID, err)
		}
		m.RepoPatterns = parsed
		out = append(out, m)
	}
	return out, rows.Err()
}

// parseRepoPatterns 解析成员的项目内仓库通配（repo_patterns 列）。
//
// ⚠️ 解析失败**必须**报错，不能当成"没有收窄"：
//
//	收窄的判据是 len(patterns) > 0，空数组 = 不收窄 = 可见范围**变宽**。
//	静默吞掉解析错误，等于把"这个成员只能看 2 个仓库"
//	悄悄变成"能看项目里所有仓库"——权限被放宽，而且不留任何痕迹。
//
// 这个字段只由成员管理写入（写的是合法 JSON），
// 解析失败说明数据坏了：让它变成调用方看得见的失败（fail-closed）。
func parseRepoPatterns(raw string) ([]string, error) {
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("解析 repo_patterns 失败（值=%q）: %w", raw, err)
	}
	return out, nil
}

// ListMemberships 返回跨项目的成员关系（"项目 × 用户"）。
//
// memberOfUser 非 nil 时，只返回**该用户作为成员**的项目里的成员关系——
// 可见范围与 ListProjects 完全一致：普通用户只能看到自己所在的项目，
// 系统管理员传 nil 看全部。
func (d *DB) ListMemberships(ctx context.Context, memberOfUser *int64) ([]domain.ProjectMembership, error) {
	q := `SELECT p.name, p.display_name, m.user_id, u.username, u.display_name, m.role, m.created_at
		FROM project_members m
		JOIN users u ON u.id = m.user_id
		JOIN projects p ON p.id = m.project_id`
	var args []any
	if memberOfUser != nil {
		q += ` WHERE EXISTS (SELECT 1 FROM project_members m2
			WHERE m2.project_id = m.project_id AND m2.user_id = ?)`
		args = append(args, *memberOfUser)
	}
	q += ` ORDER BY p.name, u.username`
	rows, err := d.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ProjectMembership
	for rows.Next() {
		var m domain.ProjectMembership
		if err := rows.Scan(&m.ProjectName, &m.ProjectDisplayName, &m.UserID,
			&m.Username, &m.DisplayName, &m.Role, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// RemoveMember 移除项目成员。
func (d *DB) RemoveMember(ctx context.Context, projectID, userID int64) error {
	_, err := d.Exec(ctx, `DELETE FROM project_members WHERE project_id = ? AND user_id = ?`, projectID, userID)
	return err
}

// repositories

// repoSelect 是仓库查询的公共列。
//
// ⚠️ visibility 用 CASE 解析成**生效**可见性：
//
//	inherit_visibility=1（跟随项目）→ 取项目可见性
//	inherit_visibility=0（显式设置）→ 取仓库自己的
//
// 解析放在 SQL 里（而不是 Go 里各处判断）是有意的：
// 权限判定、目录过滤、UI 展示拿到的是**同一个**值，
// 不会出现"某一处忘了解析"的两套语义。
const repoSelect = `SELECT r.id, r.project_id, p.name, r.name,
		CASE WHEN r.inherit_visibility = 1 THEN p.visibility ELSE r.visibility END,
		r.inherit_visibility,
		r.description, r.created_digest, r.latest_digest, r.pushed_at, r.created_at, r.updated_at
		FROM repositories r JOIN projects p ON p.id = r.project_id`

func scanRepository(scan func(dest ...any) error) (*domain.Repository, error) {
	r := &domain.Repository{}
	// digest 两列可空：建仓后还没推过镜像，digest 要等第一次推送才回填。
	// 直接扫进 string 遇到 NULL 会报 "converting NULL to string is unsupported"，
	// 目录页、权限判定都跟着 500 —— 用 NullString 兜住（与 AllRepositories 一致）。
	var createdDigest, latestDigest sql.NullString
	var inherit int
	if err := scan(&r.ID, &r.ProjectID, &r.ProjectName, &r.Name, &r.Visibility, &inherit,
		&r.Description, &createdDigest, &latestDigest, &r.PushedAt, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return nil, err
	}
	r.CreatedDigest = createdDigest.String
	r.LatestDigest = latestDigest.String
	r.FullName = r.ProjectName + "/" + r.Name
	r.VisibilitySource = domain.VisibilitySourceExplicit
	if inherit != 0 {
		r.VisibilitySource = domain.VisibilitySourceInherit
	}
	return r, nil
}

// CreateRepository 在项目下创建仓库记录。
//
// ⚠️ 新仓库一律**跟随项目**（inherit_visibility=1）：
// 公开项目里推上来的镜像就该能匿名拉，私有项目里的则是私有的。
// 项目管理员要单独收紧时，用 UpdateRepository 显式设置（02-auth.md §6）。
//
// visibility 参数仍然写进库里：它是"跟随项目"时的兜底值
// （CHECK 约束只允许 public/private），生效值由 SQL 的 CASE 解析。
func (d *DB) CreateRepository(ctx context.Context, projectID int64, name string, visibility domain.Visibility, description string) (*domain.Repository, error) {
	now := Now()
	if _, err := d.insertAndGetID(ctx, `INSERT INTO repositories
		(project_id, name, visibility, inherit_visibility, description, created_at, updated_at)
		VALUES (?, ?, ?, 1, ?, ?, ?)`, projectID, name, string(visibility), description, now, now); err != nil {
		if IsDuplicate(err) {
			return nil, ErrDuplicate
		}
		return nil, err
	}
	// ⚠️ 必须回读一次：ProjectName 与 FullName 要 JOIN projects 才有，
	//    INSERT 出来的对象里没有它们。
	//
	//    漏掉这一步的代价非常隐蔽：刷新队列以 FullName 为键，
	//    键是空串 → 刷新被**静默跳过** → 目录页显示"有镜像但大小/标签全空"，
	//    而且**一条日志都没有**（队列键为空时连"仓库名不合法"都走不到）。
	//
	//    所以这里统一返回**完整记录**，不让调用方拿到半成品。
	created, err := d.RepositoryByPath(ctx, projectID, name)
	if err != nil {
		return nil, err
	}
	return created, nil
}

// RepositoryByPath 按项目 + 仓库相对路径查仓库。
func (d *DB) RepositoryByPath(ctx context.Context, projectID int64, name string) (*domain.Repository, error) {
	row := d.QueryRow(ctx, repoSelect+` WHERE r.project_id = ? AND r.name = ?`, projectID, name)
	r, err := scanRepository(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}

// RepositoryByID 按主键查仓库。
func (d *DB) RepositoryByID(ctx context.Context, id int64) (*domain.Repository, error) {
	row := d.QueryRow(ctx, repoSelect+` WHERE r.id = ?`, id)
	r, err := scanRepository(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}

// UpdateRepository 更新仓库属性。
//
// visibility 的三态语义（02-auth.md §6）：
//
//	VisibilityInherit → 恢复"跟随项目"（inherit_visibility=1）
//	VisibilityPublic / VisibilityPrivate → 显式设置（inherit_visibility=0）
func (d *DB) UpdateRepository(ctx context.Context, id int64, visibility *domain.Visibility, description *string) error {
	sets := []string{}
	args := []any{}
	if visibility != nil {
		if *visibility == domain.VisibilityInherit {
			sets = append(sets, "inherit_visibility = ?")
			args = append(args, 1)
		} else {
			sets = append(sets, "visibility = ?", "inherit_visibility = ?")
			args = append(args, string(*visibility), 0)
		}
	}
	if description != nil {
		sets = append(sets, "description = ?")
		args = append(args, *description)
	}
	if len(sets) == 0 {
		return nil
	}
	sets = append(sets, "updated_at = ?")
	args = append(args, Now(), id)
	_, err := d.Exec(ctx, `UPDATE repositories SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
	return err
}

// TouchRepositoryPush 记录一次推送的 digest。
func (d *DB) TouchRepositoryPush(ctx context.Context, id int64, digest string) error {
	now := Now()
	_, err := d.Exec(ctx, `UPDATE repositories SET
		created_digest = COALESCE(created_digest, ?),
		latest_digest = ?, pushed_at = ?, updated_at = ? WHERE id = ?`,
		digest, digest, now, now, id)
	return err
}

// DeleteRepository 删除仓库记录（不删 blob）。
func (d *DB) DeleteRepository(ctx context.Context, id int64) error {
	_, err := d.Exec(ctx, `DELETE FROM repositories WHERE id = ?`, id)
	return err
}

// AllRepositories 返回全部仓库（按项目名 + 仓库名排序）。
func (d *DB) AllRepositories(ctx context.Context) ([]domain.Repository, error) {
	rows, err := d.Query(ctx, repoSelect+` ORDER BY p.name, r.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Repository
	for rows.Next() {
		r, err := scanRepository(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// RepoMeta 读某仓库的缓存行；不存在返回 nil。
func (d *DB) RepoMeta(ctx context.Context, repoID int64) (*domain.RepoMeta, error) {
	m := &domain.RepoMeta{}
	var platforms sql.NullString
	err := d.QueryRow(ctx, `SELECT repository_id, latest_tag, tag_count, size, created,
		platforms, pull_count, last_pulled_at, refreshed_at
		FROM repo_meta WHERE repository_id = ?`, repoID).
		Scan(&m.RepositoryID, &m.LatestTag, &m.TagCount, &m.Size, &m.Created,
			&platforms, &m.PullCount, &m.LastPulledAt, &m.RefreshedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if platforms.Valid && platforms.String != "" {
		// ⚠️ 解析失败必须报出来：这一列存的是缓存下来的平台列表，
		//    坏掉说明写入端或迁移出过问题。吞掉它等于把"数据坏了"
		//    伪装成"这个镜像没有平台信息"——调用方看不出失败过。
		if err := json.Unmarshal([]byte(platforms.String), &m.Platforms); err != nil {
			return nil, fmt.Errorf("解析 repo_meta.platforms 失败（repository_id=%d）: %w", repoID, err)
		}
	}
	if m.Platforms == nil {
		m.Platforms = []string{}
	}
	return m, nil
}

// UpsertRepoMeta 写入（覆盖）缓存行。
func (d *DB) UpsertRepoMeta(ctx context.Context, m *domain.RepoMeta) error {
	platforms := "[]"
	if len(m.Platforms) > 0 {
		b, err := json.Marshal(m.Platforms)
		if err != nil {
			return err
		}
		platforms = string(b)
	}
	now := Now()
	q := d.Dialect().UpsertOverwrite("repo_meta", "repository_id",
		[]string{"repository_id", "latest_tag", "tag_count", "size", "created", "platforms", "pull_count", "last_pulled_at", "refreshed_at"},
		[]string{"latest_tag", "tag_count", "size", "created", "platforms", "pull_count", "last_pulled_at", "refreshed_at"},
		Placeholders(9))
	_, err := d.Exec(ctx, q,
		m.RepositoryID, m.LatestTag, m.TagCount, m.Size, m.Created, platforms,
		m.PullCount, m.LastPulledAt, now)
	return err
}

// DeleteRepoMeta 删除某仓库的缓存行。
func (d *DB) DeleteRepoMeta(ctx context.Context, repoID int64) error {
	_, err := d.Exec(ctx, `DELETE FROM repo_meta WHERE repository_id = ?`, repoID)
	return err
}

// IncrementPull 增加拉取计数。
func (d *DB) IncrementPull(ctx context.Context, repoID int64) error {
	_, err := d.Exec(ctx, `UPDATE repo_meta SET pull_count = pull_count + 1,
		last_pulled_at = ? WHERE repository_id = ?`, Now(), repoID)
	return err
}

// ListRepositoriesInProject 返回项目内的仓库。
func (d *DB) ListRepositoriesInProject(ctx context.Context, projectID int64) ([]domain.Repository, error) {
	rows, err := d.Query(ctx, repoSelect+` WHERE r.project_id = ? ORDER BY r.name`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Repository
	for rows.Next() {
		r, err := scanRepository(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// grants

// AddGrant 为用户授予私有仓库访问权。
//
// 返回 added=false 表示**这条授权本来就在**（重复授权是幂等的，
// 不是错误，但调用方要据此告诉用户"他已经有权了"，不能当成刚授权成功）。
func (d *DB) AddGrant(ctx context.Context, repoID, userID int64, grantedBy *int64) (bool, error) {
	_, err := d.Exec(ctx, `INSERT INTO repository_grants (repository_id, user_id, granted_by, created_at)
		VALUES (?, ?, ?, ?)`, repoID, userID, grantedBy, Now())
	if err != nil && IsDuplicate(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// RemoveGrant 取消授权。
func (d *DB) RemoveGrant(ctx context.Context, repoID, userID int64) error {
	_, err := d.Exec(ctx, `DELETE FROM repository_grants WHERE repository_id = ? AND user_id = ?`, repoID, userID)
	return err
}

// HasGrant 判断用户是否被显式授权。
func (d *DB) HasGrant(ctx context.Context, repoID, userID int64) (bool, error) {
	var n int
	err := d.QueryRow(ctx, `SELECT COUNT(*) FROM repository_grants WHERE repository_id = ? AND user_id = ?`,
		repoID, userID).Scan(&n)
	return n > 0, err
}

// ListGrants 返回仓库的授权列表。
func (d *DB) ListGrants(ctx context.Context, repoID int64) ([]domain.RepositoryGrant, error) {
	rows, err := d.Query(ctx, `SELECT u.id, u.username, u.display_name
		FROM repository_grants g JOIN users u ON u.id = g.user_id
		WHERE g.repository_id = ? ORDER BY u.username`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.RepositoryGrant
	for rows.Next() {
		var g domain.RepositoryGrant
		if err := rows.Scan(&g.UserID, &g.Username, &g.DisplayName); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

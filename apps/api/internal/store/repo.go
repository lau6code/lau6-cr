package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
)

// ErrNotFound 表示资源不存在。
var ErrNotFound = errors.New("not found")

// ErrDuplicate 表示唯一约束冲突。
var ErrDuplicate = errors.New("duplicate")

// IsDuplicate 判断错误是否为唯一约束冲突。
func IsDuplicate(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint") ||
		strings.Contains(msg, "duplicate entry") ||
		strings.Contains(msg, "unique violation") ||
		strings.Contains(msg, "constraint failed: unique")
}

// users

// CreateUser 创建账户。source 为 local 时 passwordHash 是 bcrypt 散列。
func (d *DB) CreateUser(ctx context.Context, username, passwordHash, source, displayName string, isSystemAdmin bool) (*domain.User, error) {
	now := Now()
	admin := 0
	if isSystemAdmin {
		admin = 1
	}
	id, err := d.insertAndGetID(ctx, `INSERT INTO users
		(username, password_hash, source, display_name, is_system_admin, disabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 0, ?, ?)`,
		username, passwordHash, source, displayName, admin, now, now)
	if err != nil {
		if IsDuplicate(err) {
			return nil, ErrDuplicate
		}
		return nil, err
	}
	return &domain.User{
		ID: id, Username: username, DisplayName: displayName, PasswordHash: passwordHash,
		Source: source, IsSystemAdmin: isSystemAdmin, CreatedAt: now, UpdatedAt: now,
	}, nil
}

// UserByUsername 按用户名查账户。
func (d *DB) UserByUsername(ctx context.Context, username string) (*domain.User, error) {
	u := &domain.User{}
	err := d.QueryRow(ctx, `SELECT id, username, password_hash, source, display_name,
		is_system_admin, disabled, created_at, updated_at FROM users WHERE username = ?`, username).
		Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Source, &u.DisplayName,
			&u.IsSystemAdmin, &u.Disabled, &u.CreatedAt, &u.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

// UserByID 按主键查账户。
func (d *DB) UserByID(ctx context.Context, id int64) (*domain.User, error) {
	u := &domain.User{}
	err := d.QueryRow(ctx, `SELECT id, username, password_hash, source, display_name,
		is_system_admin, disabled, created_at, updated_at FROM users WHERE id = ?`, id).
		Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Source, &u.DisplayName,
			&u.IsSystemAdmin, &u.Disabled, &u.CreatedAt, &u.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

// ListUsers 返回所有账户。
func (d *DB) ListUsers(ctx context.Context) ([]domain.User, error) {
	rows, err := d.Query(ctx, `SELECT id, username, password_hash, source, display_name,
		is_system_admin, disabled, created_at, updated_at FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.User
	for rows.Next() {
		var u domain.User
		if err := rows.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Source, &u.DisplayName,
			&u.IsSystemAdmin, &u.Disabled, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ListMemberCandidates 返回可加入该项目的候选用户。
//
// 返回**所有启用账户**，并带上其在该项目里的当前角色
// （LEFT JOIN）：还没加入的 current_role 为 NULL，已加入的带上角色，
// 前端据此把候选人分成"可添加"与"已是成员（提交后覆盖角色）"两组。
//
// 禁用的账户不进候选——登录不了，进了也用不上。
func (d *DB) ListMemberCandidates(ctx context.Context, projectID int64) ([]domain.MemberCandidate, error) {
	rows, err := d.Query(ctx, `SELECT u.id, u.username, u.display_name, u.source, u.is_system_admin, m.role
		FROM users u
		LEFT JOIN project_members m ON m.user_id = u.id AND m.project_id = ?
		WHERE u.disabled = 0
		ORDER BY u.username`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.MemberCandidate
	for rows.Next() {
		var c domain.MemberCandidate
		var role sql.NullString
		if err := rows.Scan(&c.UserID, &c.Username, &c.DisplayName, &c.Source,
			&c.IsSystemAdmin, &role); err != nil {
			return nil, err
		}
		if role.Valid {
			r := domain.Role(role.String)
			c.CurrentRole = &r
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// UpdateUser 修改账户属性。nil 指针表示不修改该字段。
func (d *DB) UpdateUser(ctx context.Context, id int64, displayName *string, isSystemAdmin *bool, disabled *bool) error {
	sets := []string{}
	args := []any{}
	if displayName != nil {
		sets = append(sets, "display_name = ?")
		args = append(args, *displayName)
	}
	if isSystemAdmin != nil {
		v := 0
		if *isSystemAdmin {
			v = 1
		}
		sets = append(sets, "is_system_admin = ?")
		args = append(args, v)
	}
	if disabled != nil {
		v := 0
		if *disabled {
			v = 1
		}
		sets = append(sets, "disabled = ?")
		args = append(args, v)
	}
	if len(sets) == 0 {
		return nil
	}
	sets = append(sets, "updated_at = ?")
	args = append(args, Now(), id)
	_, err := d.Exec(ctx, `UPDATE users SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
	return err
}

// UpdatePassword 修改本地口令散列。
func (d *DB) UpdatePassword(ctx context.Context, id int64, hash string) error {
	_, err := d.Exec(ctx, `UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?`, hash, Now(), id)
	return err
}

// DeleteUser 删除账户。
func (d *DB) DeleteUser(ctx context.Context, id int64) error {
	_, err := d.Exec(ctx, `DELETE FROM users WHERE id = ?`, id)
	return err
}

// projects

// CreateProject 创建项目。
func (d *DB) CreateProject(ctx context.Context, name, displayName, description string, visibility domain.Visibility, createdBy *int64) (*domain.Project, error) {
	now := Now()
	id, err := d.insertAndGetID(ctx, `INSERT INTO projects
		(name, display_name, description, visibility, created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, name, displayName, description, string(visibility), createdBy, now, now)
	if err != nil {
		if IsDuplicate(err) {
			return nil, ErrDuplicate
		}
		return nil, err
	}
	return &domain.Project{
		ID: id, Name: name, DisplayName: displayName, Description: description,
		Visibility: visibility, CreatedBy: createdBy, CreatedAt: now, UpdatedAt: now,
	}, nil
}

// ProjectByName 按项目名查项目。
func (d *DB) ProjectByName(ctx context.Context, name string) (*domain.Project, error) {
	p := &domain.Project{}
	// 成员数与仓库数要和 ListProjects 一样带出来：
	// 详情页展示的是这两个计数，只查 projects 表的话永远是 0。
	err := d.QueryRow(ctx, `SELECT p.id, p.name, p.display_name, p.description, p.visibility,
		p.created_by, p.created_at, p.updated_at,
		(SELECT COUNT(*) FROM project_members m WHERE m.project_id = p.id),
		(SELECT COUNT(*) FROM repositories r WHERE r.project_id = p.id),
		(SELECT COALESCE(SUM(m.size), 0) FROM repo_meta m
			JOIN repositories r ON r.id = m.repository_id WHERE r.project_id = p.id)
		FROM projects p WHERE p.name = ?`, name).
		Scan(&p.ID, &p.Name, &p.DisplayName, &p.Description, &p.Visibility,
			&p.CreatedBy, &p.CreatedAt, &p.UpdatedAt, &p.MemberCount, &p.RepoCount, &p.TotalSize)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

// ProjectByID 按 ID 查项目。
//
// 项目令牌的主体标识是 `project:<项目名>`（人类可读），
// 而令牌记录里存的是项目 ID——要拼出 subject 就得先按 ID 查回项目名。
func (d *DB) ProjectByID(ctx context.Context, id int64) (*domain.Project, error) {
	p := &domain.Project{}
	err := d.QueryRow(ctx, `SELECT id, name, display_name, description, visibility,
		created_by, created_at, updated_at
		FROM projects WHERE id = ?`, id).
		Scan(&p.ID, &p.Name, &p.DisplayName, &p.Description, &p.Visibility,
			&p.CreatedBy, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

// ListProjects 返回项目列表。memberOfUser 非 nil 时只返回该用户是成员的项目。
//
// viewerID 非 nil 时顺带回填 domain.Project.MyRole —— 当前查看者在本项目里的角色
// （nil = 不是成员）。前端据此渲染"编辑项目"入口，
// 免得把"谁是项目管理员"这件事在各个页面各算一遍。
func (d *DB) ListProjects(ctx context.Context, memberOfUser *int64, viewerID *int64) ([]domain.Project, error) {
	// LEFT JOIN 取出查看者在本项目里的角色：不是成员时该列为 NULL。
	// ⚠️ 参数顺序：JOIN 里的 ? 排在 WHERE 里的 ? 之前，
	//    所以 args 必须是 [viewerID, memberOfUser] 这个顺序，不能调换。
	//
	// viewerID 为 nil 时用 -1 兜底：用户 id 是自增正整数，-1 永远匹配不到任何行，
	// 效果等同于"不是任何项目的成员"，也避免了 NULL 比较在三种方言下的差异。
	viewer := int64(-1)
	if viewerID != nil {
		viewer = *viewerID
	}
	q := `SELECT p.id, p.name, p.display_name, p.description, p.visibility,
		p.created_by, p.created_at, p.updated_at,
		(SELECT COUNT(*) FROM project_members m WHERE m.project_id = p.id),
		(SELECT COUNT(*) FROM repositories r WHERE r.project_id = p.id),
		(SELECT COALESCE(SUM(m.size), 0) FROM repo_meta m
			JOIN repositories r ON r.id = m.repository_id WHERE r.project_id = p.id),
		pm.role
		FROM projects p
		LEFT JOIN project_members pm ON pm.project_id = p.id AND pm.user_id = ?`
	args := []any{viewer}
	if memberOfUser != nil {
		q += ` WHERE EXISTS (SELECT 1 FROM project_members m
			WHERE m.project_id = p.id AND m.user_id = ?)`
		args = append(args, *memberOfUser)
	}
	q += ` ORDER BY p.name`
	rows, err := d.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Project
	for rows.Next() {
		var p domain.Project
		var role sql.NullString
		if err := rows.Scan(&p.ID, &p.Name, &p.DisplayName, &p.Description, &p.Visibility,
			&p.CreatedBy, &p.CreatedAt, &p.UpdatedAt, &p.MemberCount, &p.RepoCount,
			&p.TotalSize, &role); err != nil {
			return nil, err
		}
		if role.Valid {
			r := domain.Role(role.String)
			p.MyRole = &r
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpdateProject 修改项目属性。
func (d *DB) UpdateProject(ctx context.Context, id int64, displayName, description *string, visibility *domain.Visibility) error {
	sets := []string{}
	args := []any{}
	if displayName != nil {
		sets = append(sets, "display_name = ?")
		args = append(args, *displayName)
	}
	if description != nil {
		sets = append(sets, "description = ?")
		args = append(args, *description)
	}
	if visibility != nil {
		sets = append(sets, "visibility = ?")
		args = append(args, string(*visibility))
	}
	if len(sets) == 0 {
		return nil
	}
	sets = append(sets, "updated_at = ?")
	args = append(args, Now(), id)
	_, err := d.Exec(ctx, `UPDATE projects SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
	return err
}

// DeleteProject 删除项目（级联删成员、仓库记录、缓存）。
func (d *DB) DeleteProject(ctx context.Context, id int64) error {
	_, err := d.Exec(ctx, `DELETE FROM projects WHERE id = ?`, id)
	return err
}

package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
)

// personal_tokens

// nullableTime 把可空时间戳列转成 *int64。
//
// 两类令牌的 expires_at / last_used_at 都是这个语义：
// NULL = 永久有效 / 从没用过，跟"时间戳是 0"是两回事。
func nullableTime(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	t := v.Int64
	return &t
}

// CreatePersonalToken 落库一枚个人访问令牌。
//
// 调用方负责生成原始值与散列；这里只存散列与展示前缀。
//
// expiresAt 为 nil 时落 NULL = **永久有效**。
func (d *DB) CreatePersonalToken(ctx context.Context, t *domain.PersonalToken) error {
	now := Now()
	t.CreatedAt = now
	var expires any
	if t.ExpiresAt != nil {
		expires = *t.ExpiresAt
	}
	id, err := d.insertAndGetID(ctx, `INSERT INTO personal_tokens
		(user_id, name, token_hash, token_prefix, created_at, expires_at, last_used_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		t.UserID, t.Name, t.TokenHash, t.TokenPrefix, now, expires, nil)
	if err != nil {
		return err
	}
	t.ID = id
	return nil
}

// ListPersonalTokens 列出某个用户的令牌（按创建时间倒序）。
func (d *DB) ListPersonalTokens(ctx context.Context, userID int64) ([]domain.PersonalToken, error) {
	rows, err := d.Query(ctx, `SELECT id, user_id, name, token_hash, token_prefix,
		created_at, expires_at, last_used_at
		FROM personal_tokens WHERE user_id = ? ORDER BY created_at DESC, id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.PersonalToken{}
	for rows.Next() {
		var t domain.PersonalToken
		var expires, lastUsed sql.NullInt64
		if err := rows.Scan(&t.ID, &t.UserID, &t.Name, &t.TokenHash, &t.TokenPrefix,
			&t.CreatedAt, &expires, &lastUsed); err != nil {
			return nil, err
		}
		t.ExpiresAt = nullableTime(expires)
		t.LastUsedAt = nullableTime(lastUsed)
		out = append(out, t)
	}
	return out, rows.Err()
}

// PersonalTokenByHash 按散列查令牌（认证热路径）。
//
// 令牌不存在或已过期都返回 ErrNotFound——
// 调用方不需要区分"没有这枚令牌"与"这枚令牌过期了"。
func (d *DB) PersonalTokenByHash(ctx context.Context, hash string) (*domain.PersonalToken, error) {
	t := &domain.PersonalToken{}
	var expires, lastUsed sql.NullInt64
	err := d.QueryRow(ctx, `SELECT id, user_id, name, token_hash, token_prefix,
		created_at, expires_at, last_used_at
		FROM personal_tokens
		WHERE token_hash = ? AND (expires_at IS NULL OR expires_at > ?)`, hash, Now()).
		Scan(&t.ID, &t.UserID, &t.Name, &t.TokenHash, &t.TokenPrefix,
			&t.CreatedAt, &expires, &lastUsed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	t.ExpiresAt = nullableTime(expires)
	t.LastUsedAt = nullableTime(lastUsed)
	return t, nil
}

// PersonalTokenOwner 按散列查令牌的属主账户。
func (d *DB) PersonalTokenOwner(ctx context.Context, hash string) (*domain.User, error) {
	u := &domain.User{}
	err := d.QueryRow(ctx, `SELECT u.id, u.username, u.password_hash, u.source, u.display_name,
		u.is_system_admin, u.disabled, u.created_at, u.updated_at
		FROM personal_tokens t JOIN users u ON u.id = t.user_id
		WHERE t.token_hash = ? AND (t.expires_at IS NULL OR t.expires_at > ?) AND u.disabled = 0`, hash, Now()).
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

// SetPersonalTokenExpiry 改一枚令牌的失效时间（限本人）。
//
// expiresAt 为 nil = 改成**永久有效**。
//
// ⚠️ 只改失效时间，不动散列——令牌本体不变，CI 里已经配好的凭据不用重配。
func (d *DB) SetPersonalTokenExpiry(ctx context.Context, id, userID int64, expiresAt *int64) error {
	var expires any
	if expiresAt != nil {
		expires = *expiresAt
	}
	res, err := d.Exec(ctx, `UPDATE personal_tokens SET expires_at = ? WHERE id = ? AND user_id = ?`,
		expires, id, userID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	// ⚠️ MySQL 默认只报"值**真的变了**"的行数：把失效时间改成原来的值，
	//    RowsAffected 会是 0——但令牌明明还在。
	//    所以补一次存在性检查，把"没改动"和"令牌不存在"区分开。
	var exists int
	err = d.QueryRow(ctx, `SELECT 1 FROM personal_tokens WHERE id = ? AND user_id = ?`,
		id, userID).Scan(&exists)
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// TouchPersonalToken 记录令牌的最近使用时间。
func (d *DB) TouchPersonalToken(ctx context.Context, id int64) error {
	_, err := d.Exec(ctx, `UPDATE personal_tokens SET last_used_at = ? WHERE id = ?`, Now(), id)
	return err
}

// DeletePersonalToken 删掉一枚令牌（限本人）。
//
// 限 user_id 是为了防越权：别人的令牌不该被我的会话删掉。
func (d *DB) DeletePersonalToken(ctx context.Context, id, userID int64) error {
	res, err := d.Exec(ctx, `DELETE FROM personal_tokens WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeletePersonalTokensOfUser 吊销某个用户的全部令牌。
//
// 用于"吊销该用户所有凭据"这类操作：改密、封禁、删除账户时用。
func (d *DB) DeletePersonalTokensOfUser(ctx context.Context, userID int64) (int64, error) {
	res, err := d.Exec(ctx, `DELETE FROM personal_tokens WHERE user_id = ?`, userID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// project_tokens

// projectTokenColumns 是 project_tokens 的查询列。
//
// 三种按不同条件取单枚令牌的查询共用它——列少一个，
// scanProjectToken 的参数顺序就跟着错位。
const projectTokenColumns = `id, project_id, name, role, token_hash, token_prefix,
	created_by, created_at, expires_at, last_used_at`

// scanProjectToken 扫一行项目令牌记录。
//
// 三种查询的列序完全一致，扫描与字段搬运也一致——差别只在 WHERE 条件，
// 放在各自函数里各写一遍，改列时必然漏改一处。
func scanProjectToken(scan func(dest ...any) error) (*domain.ProjectToken, error) {
	t := &domain.ProjectToken{}
	var role string
	var createdBy sql.NullInt64
	var expires, lastUsed sql.NullInt64
	if err := scan(&t.ID, &t.ProjectID, &t.Name, &role, &t.TokenHash, &t.TokenPrefix,
		&createdBy, &t.CreatedAt, &expires, &lastUsed); err != nil {
		return nil, err
	}
	t.Role = domain.Role(role)
	if createdBy.Valid {
		t.CreatedBy = createdBy.Int64
	}
	t.ExpiresAt = nullableTime(expires)
	t.LastUsedAt = nullableTime(lastUsed)
	return t, nil
}

// CreateProjectToken 落库一枚项目访问令牌。
//
// 调用方负责生成原始值与散列；这里只存散列与展示前缀。
//
// expiresAt 为 nil 时落 NULL = **永久有效**。
func (d *DB) CreateProjectToken(ctx context.Context, t *domain.ProjectToken) error {
	now := Now()
	t.CreatedAt = now
	var expires any
	if t.ExpiresAt != nil {
		expires = *t.ExpiresAt
	}
	id, err := d.insertAndGetID(ctx, `INSERT INTO project_tokens
		(project_id, name, role, token_hash, token_prefix, created_by, created_at, expires_at, last_used_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ProjectID, t.Name, string(t.Role), t.TokenHash, t.TokenPrefix, t.CreatedBy, now, expires, nil)
	if err != nil {
		return err
	}
	t.ID = id
	return nil
}

// ListProjectTokens 列出某个项目的令牌（按创建时间倒序）。
func (d *DB) ListProjectTokens(ctx context.Context, projectID int64) ([]domain.ProjectToken, error) {
	rows, err := d.Query(ctx, `SELECT id, project_id, name, role, token_hash, token_prefix,
		created_by, created_at, expires_at, last_used_at
		FROM project_tokens WHERE project_id = ? ORDER BY created_at DESC, id DESC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.ProjectToken{}
	for rows.Next() {
		var t domain.ProjectToken
		var role string
		var createdBy sql.NullInt64
		var expires, lastUsed sql.NullInt64
		if err := rows.Scan(&t.ID, &t.ProjectID, &t.Name, &role, &t.TokenHash, &t.TokenPrefix,
			&createdBy, &t.CreatedAt, &expires, &lastUsed); err != nil {
			return nil, err
		}
		t.Role = domain.Role(role)
		if createdBy.Valid {
			t.CreatedBy = createdBy.Int64
		}
		t.ExpiresAt = nullableTime(expires)
		t.LastUsedAt = nullableTime(lastUsed)
		out = append(out, t)
	}
	return out, rows.Err()
}

// ProjectTokenByHash 按散列查令牌（认证热路径）。
//
// 令牌不存在或已过期都返回 ErrNotFound——
// 调用方不需要区分"没有这枚令牌"与"这枚令牌过期了"。
func (d *DB) ProjectTokenByHash(ctx context.Context, hash string) (*domain.ProjectToken, error) {
	row := d.QueryRow(ctx, `SELECT `+projectTokenColumns+`
		FROM project_tokens
		WHERE token_hash = ? AND (expires_at IS NULL OR expires_at > ?)`, hash, Now())
	t, err := scanProjectToken(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}

// ProjectTokenByID 按 ID 查一枚令牌，**限定项目**。
//
// 限定 project_id 是为了防越权：别的项目的令牌不该被我的会话改期或吊销。
func (d *DB) ProjectTokenByID(ctx context.Context, id, projectID int64) (*domain.ProjectToken, error) {
	row := d.QueryRow(ctx, `SELECT `+projectTokenColumns+`
		FROM project_tokens WHERE id = ? AND project_id = ?`, id, projectID)
	t, err := scanProjectToken(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}

// ProjectTokenByTokenID 按令牌 ID 查（**不限项目**）。
//
// 用于"按 token_id 定位具体那枚令牌"的场景：解析 registry token 里的
// 项目主体、吊销时删签名密钥。
//
// ⚠️ 与 ProjectTokenByID 的区别：那个限定项目，是防越权用的；
//
//	这个不限定，只在**已经确定这枚令牌是谁**（凭据本身即证明）时用。
func (d *DB) ProjectTokenByTokenID(ctx context.Context, id int64) (*domain.ProjectToken, error) {
	row := d.QueryRow(ctx, `SELECT `+projectTokenColumns+`
		FROM project_tokens WHERE id = ?`, id)
	t, err := scanProjectToken(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}

// ProjectTokenIDs 返回全部项目令牌的 ID（缓存后端搬迁时用）。
//
// 搬密钥要按令牌 ID 逐个搬——密钥只在令牌存在时才有意义，
// 令牌没了之后残留的密钥没有搬迁价值。
func (d *DB) ProjectTokenIDs(ctx context.Context) ([]int64, error) {
	rows, err := d.Query(ctx, `SELECT id FROM project_tokens`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// SetProjectTokenExpiry 改一枚令牌的失效时间（限本项目）。
//
// expiresAt 为 nil = 改成**永久有效**。
//
// ⚠️ 只改失效时间，不动散列——令牌本体不变，CI 里已经配好的凭据不用重配。
func (d *DB) SetProjectTokenExpiry(ctx context.Context, id, projectID int64, expiresAt *int64) error {
	var expires any
	if expiresAt != nil {
		expires = *expiresAt
	}
	res, err := d.Exec(ctx, `UPDATE project_tokens SET expires_at = ? WHERE id = ? AND project_id = ?`,
		expires, id, projectID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	// 同 SetPersonalTokenExpiry：MySQL 只报"值真的变了"的行数，
	// 这里也要补一次存在性检查，把"没改动"和"令牌不存在"区分开。
	var exists int
	err = d.QueryRow(ctx, `SELECT 1 FROM project_tokens WHERE id = ? AND project_id = ?`,
		id, projectID).Scan(&exists)
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// TouchProjectToken 记录令牌的最近使用时间。
func (d *DB) TouchProjectToken(ctx context.Context, id int64) error {
	_, err := d.Exec(ctx, `UPDATE project_tokens SET last_used_at = ? WHERE id = ?`, Now(), id)
	return err
}

// DeleteProjectToken 删掉一枚令牌（限本项目）。
//
// 限 project_id 是为了防越权：别的项目的令牌不该被我的会话删掉。
func (d *DB) DeleteProjectToken(ctx context.Context, id, projectID int64) error {
	res, err := d.Exec(ctx, `DELETE FROM project_tokens WHERE id = ? AND project_id = ?`, id, projectID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteProjectTokensOfProject 吊销某个项目的全部令牌。
//
// 用于"项目整体作废"这类操作：删除项目时用。
func (d *DB) DeleteProjectTokensOfProject(ctx context.Context, projectID int64) (int64, error) {
	res, err := d.Exec(ctx, `DELETE FROM project_tokens WHERE project_id = ?`, projectID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
)

// sessions

// HashSessionID 计算会话 ID 的存储形式。
//
// 数据库里只存 SHA-256，原始随机值只存在于 Cookie 中——
// 即使数据库泄露也无法直接冒用会话。
func HashSessionID(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// CreateSession 落库一条会话（id 为 SHA-256 后的值）。
func (d *DB) CreateSession(ctx context.Context, id string, userID int64, expiresAt int64, ip, ua string) error {
	now := Now()
	_, err := d.Exec(ctx, `INSERT INTO sessions
		(id, user_id, created_at, expires_at, last_seen_at, client_ip, user_agent)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, id, userID, now, expiresAt, now, ip, ua)
	return err
}

// SessionUser 按会话 ID 查关联用户；会话不存在或过期返回 nil。
func (d *DB) SessionUser(ctx context.Context, id string) (*domain.User, error) {
	now := Now()
	u := &domain.User{}
	err := d.QueryRow(ctx, `SELECT u.id, u.username, u.password_hash, u.source, u.display_name,
		u.is_system_admin, u.disabled, u.created_at, u.updated_at
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.id = ? AND s.expires_at > ? AND u.disabled = 0`, id, now).
		Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Source, &u.DisplayName,
			&u.IsSystemAdmin, &u.Disabled, &u.CreatedAt, &u.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

// TouchSession 刷新会话活跃时间。
func (d *DB) TouchSession(ctx context.Context, id string) error {
	_, err := d.Exec(ctx, `UPDATE sessions SET last_seen_at = ? WHERE id = ?`, Now(), id)
	return err
}

// DeleteSession 删除会话（登出）。
func (d *DB) DeleteSession(ctx context.Context, id string) error {
	_, err := d.Exec(ctx, `DELETE FROM sessions WHERE id = ?`, id)
	return err
}

// DeleteOtherSessions 删除某用户的其他会话，保留 keepID 这一条。
//
// 改密后要把**别处**登录的会话一起吊销：口令变了，
// 用旧口令登进来的会话不该继续有效。当前这条（keepID）
// 保留，用户不必重新登录。
func (d *DB) DeleteOtherSessions(ctx context.Context, userID int64, keepID string) (int64, error) {
	var res sql.Result
	var err error
	if keepID == "" {
		res, err = d.Exec(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID)
	} else {
		res, err = d.Exec(ctx, `DELETE FROM sessions WHERE user_id = ? AND id <> ?`, userID, keepID)
	}
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// settings

// SettingValue 是一条设置的当前值与来源信息。
type SettingValue struct {
	Key       string
	Value     any
	Version   int64
	UpdatedAt int64
	UpdatedBy string
}

// settingsKeyColumn 返回 system_settings 表 `key` 列的方言化写法。
//
// `key` 是 MySQL 的保留字，不加反引号直接 1064 语法错误（见 Dialect.QuoteIdent）。
func settingsKeyColumn(d Dialect) string { return d.QuoteIdent("key") }

// GetSetting 读单个设置。
func (d *DB) GetSetting(ctx context.Context, key string) (*SettingValue, error) {
	s := &SettingValue{}
	var raw string
	col := settingsKeyColumn(d.Dialect())
	err := d.QueryRow(ctx, fmt.Sprintf(`SELECT %s, value, version, updated_at, updated_by
		FROM system_settings WHERE %s = ?`, col, col), key).
		Scan(&s.Key, &raw, &s.Version, &s.UpdatedAt, &s.UpdatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(raw), &s.Value); err != nil {
		return nil, fmt.Errorf("解析设置 %s: %w", key, err)
	}
	return s, nil
}

// AllSettings 读全部设置。
func (d *DB) AllSettings(ctx context.Context) (map[string]SettingValue, error) {
	return allSettings(ctx, d.Dialect(), d.Query)
}

// AllSettings 在事务里读全部设置。
//
// ⚠️ 它存在的意义是让"写设置 + 重算快照"能在**同一个事务**里完成：
//
//	写入还没提交就能读到新值，重算失败可以直接回滚，
//	不会留下"库里已落库、运行期快照还是旧值"的分裂状态。
func (t *Tx) AllSettings(ctx context.Context) (map[string]SettingValue, error) {
	return allSettings(ctx, t.Dialect, t.QueryContext)
}

// allSettings 读全部设置（DB 与 Tx 共用）。
func allSettings(ctx context.Context, dialect Dialect, query rowsQuerier) (map[string]SettingValue, error) {
	col := settingsKeyColumn(dialect)
	rows, err := query(ctx, fmt.Sprintf(
		`SELECT %s, value, version, updated_at, updated_by FROM system_settings`, col))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSettings(rows)
}

// scanSettings 把查询结果扫成设置表。
func scanSettings(rows *sql.Rows) (map[string]SettingValue, error) {
	out := map[string]SettingValue{}
	for rows.Next() {
		var s SettingValue
		var raw string
		if err := rows.Scan(&s.Key, &raw, &s.Version, &s.UpdatedAt, &s.UpdatedBy); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &s.Value); err != nil {
			return nil, fmt.Errorf("解析设置 %s: %w", s.Key, err)
		}
		out[s.Key] = s
	}
	return out, rows.Err()
}

// UpsertSetting 写入单个设置并把全局版本 +1。
func (d *DB) UpsertSetting(ctx context.Context, key string, value any, updatedBy string) error {
	return upsertSetting(ctx, d.Dialect(), d.Exec, key, value, updatedBy)
}

// UpsertSetting 在事务里写入单个设置。
func (t *Tx) UpsertSetting(ctx context.Context, key string, value any, updatedBy string) error {
	return upsertSetting(ctx, t.Dialect, t.Exec, key, value, updatedBy)
}

// upsertSetting 写入单个设置并把全局版本 +1（DB 与 Tx 共用）。
func upsertSetting(ctx context.Context, dialect Dialect, exec execer, key string, value any, updatedBy string) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	now := Now()
	_, err = exec(ctx, upsertSettingSQL(dialect), key, string(raw), now, updatedBy)
	return err
}

// upsertSettingSQL 返回 upsert 单条设置的 SQL（按方言）。
func upsertSettingSQL(d Dialect) string {
	col := settingsKeyColumn(d)
	if d == DialectMySQL {
		return fmt.Sprintf(`INSERT INTO system_settings (%s, value, version, updated_at, updated_by)
		VALUES (?, ?, 1, ?, ?)
		ON DUPLICATE KEY UPDATE
			value = VALUES(value),
			version = version + 1,
			updated_at = VALUES(updated_at),
			updated_by = VALUES(updated_by)`, col)
	}
	return fmt.Sprintf(`INSERT INTO system_settings (%s, value, version, updated_at, updated_by)
		VALUES (?, ?, 1, ?, ?)
		ON CONFLICT (%s) DO UPDATE SET
			value = EXCLUDED.value,
			version = system_settings.version + 1,
			updated_at = EXCLUDED.updated_at,
			updated_by = EXCLUDED.updated_by`, col, col)
}

// upsertInternalSettingSQL 返回写内部设置项的 SQL（按方言）。
//
// ⚠️ 为什么不动 version：system_settings.version 是设置页的乐观锁版本
//
//	（GlobalSettingsVersion 取的是 MAX(version)）。汇总游标每小时都要写一次，
//	跟着涨版本的话，管理员开着设置页时一次后台汇总就能让保存报
//	"设置已被他人修改"——那是假冲突，必须避免。
func upsertInternalSettingSQL(d Dialect) string {
	col := settingsKeyColumn(d)
	if d == DialectMySQL {
		return fmt.Sprintf(`INSERT INTO system_settings (%s, value, version, updated_at, updated_by)
		VALUES (?, ?, 1, ?, ?)
		ON DUPLICATE KEY UPDATE
			value = VALUES(value),
			updated_at = VALUES(updated_at),
			updated_by = VALUES(updated_by)`, col)
	}
	return fmt.Sprintf(`INSERT INTO system_settings (%s, value, version, updated_at, updated_by)
		VALUES (?, ?, 1, ?, ?)
		ON CONFLICT (%s) DO UPDATE SET
			value = EXCLUDED.value,
			updated_at = EXCLUDED.updated_at,
			updated_by = EXCLUDED.updated_by`, col, col)
}

// upsertInternalSetting 写内部设置项，**不动 version**（DB 与 Tx 共用）。
func upsertInternalSetting(ctx context.Context, dialect Dialect, exec execer, key string, value any, updatedBy string) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = exec(ctx, upsertInternalSettingSQL(dialect), key, string(raw), Now(), updatedBy)
	return err
}

// DeleteSetting 删除设置（恢复默认）。
func (d *DB) DeleteSetting(ctx context.Context, key string) error {
	return deleteSetting(ctx, d.Dialect(), d.Exec, key)
}

// DeleteSetting 在事务里删除设置（恢复默认）。
func (t *Tx) DeleteSetting(ctx context.Context, key string) error {
	return deleteSetting(ctx, t.Dialect, t.Exec, key)
}

// deleteSetting 删除一条设置（DB 与 Tx 共用）。
func deleteSetting(ctx context.Context, dialect Dialect, exec execer, key string) error {
	col := settingsKeyColumn(dialect)
	_, err := exec(ctx, fmt.Sprintf(`DELETE FROM system_settings WHERE %s = ?`, col), key)
	return err
}

// GlobalSettingsVersion 返回全局乐观锁版本。
func (d *DB) GlobalSettingsVersion(ctx context.Context) (int64, error) {
	var v int64
	err := d.QueryRow(ctx, `SELECT COALESCE(MAX(version), 0) FROM system_settings`).Scan(&v)
	return v, err
}

// audit

// ListAudit 按条件查审计。
func (d *DB) ListAudit(ctx context.Context, actor, action string, limit, offset int) ([]domain.AuditLog, error) {
	q := `SELECT id, ts, actor, action, target, scope, client_ip, success, detail FROM audit_logs`
	var conds []string
	var args []any
	if actor != "" {
		conds = append(conds, "actor = ?")
		args = append(args, actor)
	}
	if action != "" {
		conds = append(conds, "action = ?")
		args = append(args, action)
	}
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += " ORDER BY ts DESC, id DESC"
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
		if offset > 0 {
			q += " OFFSET ?"
			args = append(args, offset)
		}
	}
	rows, err := d.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.AuditLog
	for rows.Next() {
		var a domain.AuditLog
		var detail sql.NullString
		var target, scope, ip sql.NullString
		if err := rows.Scan(&a.ID, &a.TS, &a.Actor, &a.Action, &target, &scope, &ip, &a.Success, &detail); err != nil {
			return nil, err
		}
		a.Target, a.Scope, a.ClientIP = target.String, scope.String, ip.String
		a.Detail = detail.String
		out = append(out, a)
	}
	return out, rows.Err()
}

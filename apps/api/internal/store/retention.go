package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
)

// 保留策略与清理执行记录的持久化。
//
// 设计依据见 docs/design/03-registry.md §5。

// GlobalRetentionScope 是全局默认策略的 scope_key。
const GlobalRetentionScope = ""

// ProjectRetentionScope 返回项目级策略的 scope_key。
//
// 用显式的 scope_key 而不是直接对 project_id 建唯一索引：
// project_id 可空，而多数数据库的唯一索引**不认为多个 NULL 冲突**，
// 直接建唯一索引会让全局默认策略被插进去好几行。
func ProjectRetentionScope(projectID int64) string {
	return "p:" + strconv.FormatInt(projectID, 10)
}

// RetentionPolicyByScope 按 scope_key 读策略；不存在返回 nil, nil。
func (d *DB) RetentionPolicyByScope(ctx context.Context, scopeKey string) (*domain.RetentionPolicy, error) {
	row := d.QueryRow(ctx, `SELECT id, scope_key, project_id, enabled, algorithm, rules,
		tag_selector, created_at, updated_at
		FROM retention_policies WHERE scope_key = ?`, scopeKey)
	p, err := scanRetentionPolicy(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

// RetentionPolicyByProject 读某项目的策略；不存在返回 nil, nil。
func (d *DB) RetentionPolicyByProject(ctx context.Context, projectID int64) (*domain.RetentionPolicy, error) {
	return d.RetentionPolicyByScope(ctx, ProjectRetentionScope(projectID))
}

// ListRetentionPolicies 列出全部策略（含全局默认）。
func (d *DB) ListRetentionPolicies(ctx context.Context) ([]domain.RetentionPolicy, error) {
	rows, err := d.Query(ctx, `SELECT p.id, p.scope_key, p.project_id, p.enabled, p.algorithm, p.rules,
		p.tag_selector, p.created_at, p.updated_at, COALESCE(pr.name, '')
		FROM retention_policies p
		LEFT JOIN projects pr ON pr.id = p.project_id
		ORDER BY p.project_id IS NOT NULL, p.project_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.RetentionPolicy{}
	for rows.Next() {
		var p domain.RetentionPolicy
		var projectID sql.NullInt64
		var rules string
		if err := rows.Scan(&p.ID, &p.ScopeKey, &projectID, &p.Enabled, &p.Algorithm, &rules,
			&p.TagSelector, &p.CreatedAt, &p.UpdatedAt, &p.ProjectName); err != nil {
			return nil, err
		}
		if projectID.Valid {
			id := projectID.Int64
			p.ProjectID = &id
		}
		if err := decodeRetentionRules(rules, &p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpsertRetentionPolicy 写入（覆盖）一条策略。
func (d *DB) UpsertRetentionPolicy(ctx context.Context, p *domain.RetentionPolicy) error {
	rules, err := json.Marshal(p.Rules)
	if err != nil {
		return fmt.Errorf("序列化保留策略规则: %w", err)
	}
	if p.Rules == nil {
		rules = []byte("[]")
	}
	now := Now()
	q := d.Dialect().UpsertOverwrite("retention_policies", "scope_key",
		[]string{"scope_key", "project_id", "enabled", "algorithm", "rules", "tag_selector", "created_at", "updated_at"},
		[]string{"project_id", "enabled", "algorithm", "rules", "tag_selector", "updated_at"},
		Placeholders(8))
	_, err = d.Exec(ctx, q,
		p.ScopeKey, p.ProjectID, boolToInt(p.Enabled), p.Algorithm, string(rules),
		p.TagSelector, now, now)
	return err
}

// DeleteRetentionPolicy 删除一条策略；不存在不是错误。
func (d *DB) DeleteRetentionPolicy(ctx context.Context, scopeKey string) error {
	_, err := d.Exec(ctx, `DELETE FROM retention_policies WHERE scope_key = ?`, scopeKey)
	return err
}

// RetentionTag 读单个 tag 快照；不存在返回 nil, nil。
func (d *DB) RetentionTag(ctx context.Context, repositoryID int64, tag string) (*domain.RetentionTag, error) {
	row := d.QueryRow(ctx, `SELECT repository_id, tag, digest, size, pushed_at, pulled_at
		FROM retention_tags WHERE repository_id = ? AND tag = ?`, repositoryID, tag)
	var t domain.RetentionTag
	var pushedAt, pulledAt sql.NullInt64
	err := row.Scan(&t.RepositoryID, &t.Tag, &t.Digest, &t.Size, &pushedAt, &pulledAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if pushedAt.Valid {
		v := pushedAt.Int64
		t.PushedAt = &v
	}
	if pulledAt.Valid {
		v := pulledAt.Int64
		t.PulledAt = &v
	}
	return &t, nil
}

// RetentionTagsOf 列出某仓库的全部 tag 快照，按 tag 名排序。
func (d *DB) RetentionTagsOf(ctx context.Context, repositoryID int64) ([]domain.RetentionTag, error) {
	rows, err := d.Query(ctx, `SELECT repository_id, tag, digest, size, pushed_at, pulled_at
		FROM retention_tags WHERE repository_id = ? ORDER BY tag`, repositoryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.RetentionTag{}
	for rows.Next() {
		var t domain.RetentionTag
		var pushedAt, pulledAt sql.NullInt64
		if err := rows.Scan(&t.RepositoryID, &t.Tag, &t.Digest, &t.Size, &pushedAt, &pulledAt); err != nil {
			return nil, err
		}
		if pushedAt.Valid {
			v := pushedAt.Int64
			t.PushedAt = &v
		}
		if pulledAt.Valid {
			v := pulledAt.Int64
			t.PulledAt = &v
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// UpsertRetentionTag 写入（覆盖）一个 tag 快照。
//
// ⚠️ **推送时间的处理是有方向的**：
//
//	t.PushedAt 非 nil → 直接写入（代理层观测到 push，权威）。
//	t.PushedAt 为 nil 且 digest 未变 → 保留旧值（还是同一个镜像）。
//	t.PushedAt 为 nil 且 digest 变了 → **清空**（推送时间未知）。
//
// 第三种情况必须清空：旧时间会让这个 tag 看起来"更老"，
// 于是更容易被保留策略删掉——那是**危险的方向**。
// 不知道就说不知道，不知道的 tag 一律保留（03-registry.md §5.2.4 ②）。
func (d *DB) UpsertRetentionTag(ctx context.Context, t *domain.RetentionTag) error {
	pushedAt := t.PushedAt
	if pushedAt == nil {
		old, err := d.RetentionTag(ctx, t.RepositoryID, t.Tag)
		if err != nil {
			return err
		}
		if old != nil && old.Digest == t.Digest {
			pushedAt = old.PushedAt
		}
	}
	now := Now()
	q := d.Dialect().UpsertOverwrite("retention_tags", "repository_id, tag",
		[]string{"repository_id", "tag", "digest", "size", "pushed_at", "pulled_at", "created_at", "updated_at"},
		[]string{"digest", "size", "pushed_at", "pulled_at", "updated_at"},
		Placeholders(8))
	_, err := d.Exec(ctx, q,
		t.RepositoryID, t.Tag, t.Digest, t.Size, nullInt64(pushedAt), nullInt64(t.PulledAt), now, now)
	return err
}

// DeleteRetentionTag 删除一个 tag 快照；不存在不是错误。
func (d *DB) DeleteRetentionTag(ctx context.Context, repositoryID int64, tag string) error {
	_, err := d.Exec(ctx, `DELETE FROM retention_tags WHERE repository_id = ? AND tag = ?`,
		repositoryID, tag)
	return err
}

// PruneRetentionTagsNotIn 删除不在给定集合里的 tag 快照，返回删除行数。
//
// 用途：清理时以上游的 tag 列表为准，把**上游已没有**的快照行清掉。
//
// ⚠️ 这不只是省空间——残留快照会让"删掉又用**同一个 digest** 重建"的 tag
// 带着**过期的推送时间**，于是它显得更老、更容易被保留策略删掉。
// 那是危险的方向（`03-registry.md` §5.2.4 ②）。
func (d *DB) PruneRetentionTagsNotIn(ctx context.Context, repositoryID int64, keep []string) (int64, error) {
	if len(keep) == 0 {
		res, err := d.Exec(ctx, `DELETE FROM retention_tags WHERE repository_id = ?`, repositoryID)
		if err != nil {
			return 0, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, err
		}
		return n, nil
	}
	args := make([]any, 0, len(keep)+1)
	args = append(args, repositoryID)
	for _, t := range keep {
		args = append(args, t)
	}
	q := `DELETE FROM retention_tags WHERE repository_id = ? AND tag NOT IN (` +
		Placeholders(len(keep)) + `)`
	res, err := d.Exec(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return n, nil
}

// CreateCleanupRun 建一条执行记录，返回它的 ID。
func (d *DB) CreateCleanupRun(ctx context.Context, r *domain.CleanupRun) (int64, error) {
	return d.insertAndGetID(ctx, fmt.Sprintf(`INSERT INTO cleanup_runs
		(%s, scope, project_name, dry_run, status, started_at)
		VALUES (?, ?, ?, ?, ?, ?)`, d.Dialect().QuoteIdent("trigger")),
		r.Trigger, r.Scope, r.ProjectName, boolToInt(r.DryRun), r.Status, runStartedAt(r.StartedAt))
}

// FinishCleanupRun 收尾一条执行记录。
func (d *DB) FinishCleanupRun(ctx context.Context, r *domain.CleanupRun) error {
	finished := Now()
	r.FinishedAt = &finished
	_, err := d.Exec(ctx, `UPDATE cleanup_runs SET
		status = ?, tags_deleted = ?, manifests_deleted = ?, tags_kept = ?,
		skipped_unknown_time = ?, skipped_shared_digest = ?, failed_count = ?,
		error = ?, detail = ?, finished_at = ?
		WHERE id = ?`,
		r.Status, r.TagsDeleted, r.ManifestsDeleted, r.TagsKept,
		r.SkippedUnknownTime, r.SkippedSharedDigest, r.FailedCount,
		nullString(r.Error), nullString(r.Detail), finished, r.ID)
	return err
}

// ListCleanupRuns 列出执行历史（新的在前）。
func (d *DB) ListCleanupRuns(ctx context.Context, limit, offset int) ([]domain.CleanupRun, error) {
	limit, offset = clampRunPage(limit, offset)
	return listRunHistory(ctx, d.Query, scanCleanupRun, fmt.Sprintf(`SELECT id, %s, scope, project_name, dry_run, status,
		tags_deleted, manifests_deleted, tags_kept, skipped_unknown_time, skipped_shared_digest,
		failed_count, error, detail, started_at, finished_at
		FROM cleanup_runs ORDER BY started_at DESC, id DESC LIMIT ? OFFSET ?`,
		d.Dialect().QuoteIdent("trigger")), limit, offset)
}

// CleanupRunByID 读一条执行记录；不存在返回 nil, nil。
func (d *DB) CleanupRunByID(ctx context.Context, id int64) (*domain.CleanupRun, error) {
	return runByID(ctx, d.QueryRow, scanCleanupRun, fmt.Sprintf(`SELECT id, %s, scope, project_name, dry_run, status,
		tags_deleted, manifests_deleted, tags_kept, skipped_unknown_time, skipped_shared_digest,
		failed_count, error, detail, started_at, finished_at
		FROM cleanup_runs WHERE id = ?`, d.Dialect().QuoteIdent("trigger")), id)
}

// scanCleanupRun 把一行扫成 CleanupRun。
func scanCleanupRun(s rowScanner) (domain.CleanupRun, error) {
	var r domain.CleanupRun
	var dryRun int
	var errMsg, detail sql.NullString
	var finished sql.NullInt64
	if err := s.Scan(&r.ID, &r.Trigger, &r.Scope, &r.ProjectName, &dryRun, &r.Status,
		&r.TagsDeleted, &r.ManifestsDeleted, &r.TagsKept, &r.SkippedUnknownTime,
		&r.SkippedSharedDigest, &r.FailedCount,
		&errMsg, &detail, &r.StartedAt, &finished); err != nil {
		return domain.CleanupRun{}, err
	}
	r.DryRun = dryRun != 0
	r.Error = errMsg.String
	r.Detail = detail.String
	if finished.Valid {
		v := finished.Int64
		r.FinishedAt = &v
	}
	return r, nil
}

func scanRetentionPolicy(row *sql.Row) (*domain.RetentionPolicy, error) {
	var p domain.RetentionPolicy
	var projectID sql.NullInt64
	var rules string
	if err := row.Scan(&p.ID, &p.ScopeKey, &projectID, &p.Enabled, &p.Algorithm, &rules,
		&p.TagSelector, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, err
	}
	if projectID.Valid {
		id := projectID.Int64
		p.ProjectID = &id
	}
	if err := decodeRetentionRules(rules, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// decodeRetentionRules 解析规则 JSON。
func decodeRetentionRules(raw string, p *domain.RetentionPolicy) error {
	p.Rules = []domain.RetentionRule{}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(raw), &p.Rules); err != nil {
		return fmt.Errorf("解析保留策略规则: %w", err)
	}
	if p.Rules == nil {
		p.Rules = []domain.RetentionRule{}
	}
	return nil
}

// boolToInt 把 bool 转成 0/1。
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// nullInt64 把 *int64 转成可空参数。
func nullInt64(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

// nullString 把空串转成 NULL。
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

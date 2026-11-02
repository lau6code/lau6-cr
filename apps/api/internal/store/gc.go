package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
)

// blob 回收执行记录的持久化。
//
// 设计依据见 docs/design/03-registry.md §5.5。

// CreateGCRun 建一条执行记录，返回它的 ID。
func (d *DB) CreateGCRun(ctx context.Context, r *domain.GCRun) (int64, error) {
	return d.insertAndGetID(ctx, fmt.Sprintf(`INSERT INTO gc_runs
		(%s, dry_run, status, storage_root, started_at)
		VALUES (?, ?, ?, ?, ?)`, d.Dialect().QuoteIdent("trigger")),
		r.Trigger, boolToInt(r.DryRun), r.Status, r.StorageRoot, runStartedAt(r.StartedAt))
}

// FinishGCRun 收尾一条执行记录。
func (d *DB) FinishGCRun(ctx context.Context, r *domain.GCRun) error {
	finished := Now()
	r.FinishedAt = &finished
	_, err := d.Exec(ctx, `UPDATE gc_runs SET
		status = ?, repositories = ?, manifests_found = ?, blobs_marked = ?,
		blobs_scanned = ?, blobs_deleted = ?, bytes_reclaimed = ?,
		layer_links_deleted = ?, untagged_deleted = ?, error = ?, finished_at = ?
		WHERE id = ?`,
		r.Status, r.Repositories, r.ManifestsFound, r.BlobsMarked,
		r.BlobsScanned, r.BlobsDeleted, r.BytesReclaimed,
		r.LayerLinksDeleted, r.UntaggedDeleted, nullString(r.Error), finished, r.ID)
	return err
}

// ListGCRuns 列出执行历史（新的在前）。
func (d *DB) ListGCRuns(ctx context.Context, limit, offset int) ([]domain.GCRun, error) {
	limit, offset = clampRunPage(limit, offset)
	return listRunHistory(ctx, d.Query, scanGCRun, fmt.Sprintf(`SELECT id, %s, dry_run, status, storage_root,
		repositories, manifests_found, blobs_marked, blobs_scanned, blobs_deleted,
		bytes_reclaimed, layer_links_deleted, untagged_deleted, error, started_at, finished_at
		FROM gc_runs ORDER BY started_at DESC, id DESC LIMIT ? OFFSET ?`,
		d.Dialect().QuoteIdent("trigger")), limit, offset)
}

// GCRunByID 读一条执行记录；不存在返回 nil, nil。
func (d *DB) GCRunByID(ctx context.Context, id int64) (*domain.GCRun, error) {
	return runByID(ctx, d.QueryRow, scanGCRun, fmt.Sprintf(`SELECT id, %s, dry_run, status, storage_root,
		repositories, manifests_found, blobs_marked, blobs_scanned, blobs_deleted,
		bytes_reclaimed, layer_links_deleted, untagged_deleted, error, started_at, finished_at
		FROM gc_runs WHERE id = ?`, d.Dialect().QuoteIdent("trigger")), id)
}

// scanGCRun 把一行扫成 GCRun。
func scanGCRun(s rowScanner) (domain.GCRun, error) {
	var r domain.GCRun
	var dryRun int
	var errMsg sql.NullString
	var finished sql.NullInt64
	if err := s.Scan(&r.ID, &r.Trigger, &dryRun, &r.Status, &r.StorageRoot,
		&r.Repositories, &r.ManifestsFound, &r.BlobsMarked, &r.BlobsScanned,
		&r.BlobsDeleted, &r.BytesReclaimed, &r.LayerLinksDeleted, &r.UntaggedDeleted,
		&errMsg, &r.StartedAt, &finished); err != nil {
		return domain.GCRun{}, err
	}
	r.DryRun = dryRun != 0
	r.Error = errMsg.String
	if finished.Valid {
		v := finished.Int64
		r.FinishedAt = &v
	}
	return r, nil
}

// LastGCRun 返回最近一次 blob 回收记录；没有记录时返回 nil, nil。
func (d *DB) LastGCRun(ctx context.Context) (*domain.GCRun, error) {
	runs, err := d.ListGCRuns(ctx, 1, 0)
	if err != nil {
		return nil, fmt.Errorf("读取最近一次 blob 回收记录失败: %w", err)
	}
	if len(runs) == 0 {
		return nil, nil
	}
	return &runs[0], nil
}

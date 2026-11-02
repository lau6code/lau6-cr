package store

import (
	"context"
	"database/sql"
	"errors"
)

// 执行记录（blob 回收 / 镜像清理）的通用读写。
//
// 两张表的列不一样（gc_runs 记 blob 统计，cleanup_runs 记 tag 统计），
// 但"建一条 → 收尾 → 列历史 → 按 ID 读"的骨架完全相同，
// 差异集中在列清单与扫描回调上。

// rowScanner 是能扫一行的东西（*sql.Row 与 *sql.Rows 都满足）。
type rowScanner interface {
	Scan(dest ...any) error
}

const (
	// runPageSize 是执行历史列表的默认条数。
	runPageSize = 50
	// runMaxPageSize 是执行历史列表的单页上限。
	runMaxPageSize = 200
)

// clampRunPage 把分页参数收敛到合法范围。
func clampRunPage(limit, offset int) (int, int) {
	if limit <= 0 || limit > runMaxPageSize {
		limit = runPageSize
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

// runStartedAt 返回执行记录的开始时间（没给就用当前时间）。
func runStartedAt(started int64) int64 {
	if started == 0 {
		return Now()
	}
	return started
}

// listRunHistory 列出执行历史（新的在前）。
//
// listSQL 由调用方给出（两张表的列不一样），scan 把一行扫成记录。
func listRunHistory[T any](ctx context.Context, query rowsQuerier, scan func(rowScanner) (T, error), listSQL string, args ...any) ([]T, error) {
	rows, err := query(ctx, listSQL, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []T{}
	for rows.Next() {
		r, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// runByID 读一条执行记录；不存在返回 nil, nil。
func runByID[T any](ctx context.Context, queryRow rowQuerier, scan func(rowScanner) (T, error), byIDSQL string, id int64) (*T, error) {
	r, err := scan(queryRow(ctx, byIDSQL, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

package store

import (
	"context"
	"testing"
)

// TestUpsertIncludesConflictColumns 锁定 upsert 语句的列清单。
//
// ⚠️ INSERT 的列清单必须是「冲突键列 + 待更新列」：
//
//	调用方给的 values 就是这个顺序。少列冲突键会让**值数与列数对不上**，
//	SQLite 直接报 "N values for M columns"，
//	流量统计于是**永远写不进去**（而且错误被吞掉，页面全是 0）。
func TestUpsertIncludesConflictColumns(t *testing.T) {
	sql := DialectSQLite.Upsert(
		"traffic_stats_hourly",
		"repository_id, bucket_at",
		[]string{"repository_id", "bucket_at", "pulls", "pushes", "deletes",
			"bytes_pulled", "bytes_pushed", "updated_at"},
		[]string{"pulls", "pushes", "deletes", "bytes_pulled", "bytes_pushed"},
		"?, ?, ?, ?, ?, ?, ?, ?",
	)
	want := "INSERT INTO traffic_stats_hourly (repository_id, bucket_at, pulls, pushes, deletes, bytes_pulled, bytes_pushed, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)"
	if got := sql[:len(want)]; got != want {
		t.Fatalf("INSERT 列清单不对：\n得到 %q\n期望 %q", got, want)
	}
}

// TestUpsertTrafficStatsRoundTrip 覆盖统计桶真正落库。
//
// 第一次插入建行，第二次写同一 (repository_id, bucket_at) 必须**累加**而不是覆盖。
func TestUpsertTrafficStatsRoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	const repoID = int64(1)
	const bucket = int64(2026102912)
	insertCols := []string{"repository_id", "bucket_at", "pulls", "pushes", "deletes",
		"bytes_pulled", "bytes_pushed", "updated_at"}
	setCols := []string{"pulls", "pushes", "deletes", "bytes_pulled", "bytes_pushed"}

	if err := db.Upsert(ctx, "traffic_stats_hourly", "repository_id, bucket_at", insertCols, setCols,
		[]any{repoID, bucket, 3, 1, 0, 100, 200, 1}); err != nil {
		t.Fatalf("首次写入: %v", err)
	}
	// 同一小时再刷一次 → 累加。
	if err := db.Upsert(ctx, "traffic_stats_hourly", "repository_id, bucket_at", insertCols, setCols,
		[]any{repoID, bucket, 2, 0, 0, 10, 20, 2}); err != nil {
		t.Fatalf("二次写入: %v", err)
	}

	var pulls, pushes, bytesPulled, bytesPushed int64
	var rows int
	row := db.QueryRow(ctx, `SELECT COUNT(*), COALESCE(SUM(pulls),0), COALESCE(SUM(pushes),0),
		COALESCE(SUM(bytes_pulled),0), COALESCE(SUM(bytes_pushed),0)
		FROM traffic_stats_hourly WHERE repository_id = ? AND bucket_at = ?`, repoID, bucket)
	if err := row.Scan(&rows, &pulls, &pushes, &bytesPulled, &bytesPushed); err != nil {
		t.Fatalf("读回统计: %v", err)
	}
	if rows != 1 {
		t.Fatalf("行数 = %d，期望 1（同一小时桶只有一行）", rows)
	}
	if pulls != 5 || pushes != 1 {
		t.Fatalf("pulls=%d pushes=%d，期望 5/1（必须累加，不是覆盖）", pulls, pushes)
	}
	if bytesPulled != 110 || bytesPushed != 220 {
		t.Fatalf("bytes_pulled=%d bytes_pushed=%d，期望 110/220", bytesPulled, bytesPushed)
	}
}

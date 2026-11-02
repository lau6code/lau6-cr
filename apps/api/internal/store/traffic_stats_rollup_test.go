package store

import (
	"context"
	"strings"
	"testing"

	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
)

// newStatsRepo 建一个项目 + 仓库，返回仓库 ID。
//
// ⚠️ 用真仓库而不是硬编码 ID：统计行上挂着 repository_id 外键，
//
//	写一个不存在的仓库会让"外键一开就插不进去"。
func newStatsRepo(t *testing.T, db *DB) int64 {
	t.Helper()
	ctx := context.Background()
	project, err := db.CreateProject(ctx, "team-a", "项目 A", "", domain.VisibilityPrivate, nil)
	if err != nil {
		t.Fatalf("创建项目: %v", err)
	}
	repo, err := db.CreateRepository(ctx, project.ID, "app", domain.VisibilityPrivate, "")
	if err != nil {
		t.Fatalf("创建仓库: %v", err)
	}
	return repo.ID
}

// ───────────────────────── 累加式 upsert ─────────────────────────

// TestUpsertDailyStatsAccumulates 锁定天桶是**累加**而不是覆盖。
//
// ⚠️ 天桶由 24 个小时桶累加而成，重复汇总会再写一次。
//
//	写成覆盖（SET pulls = EXCLUDED.pulls）会让天表只剩最后一次的值——
//	趋势图看着有数据，量却全是错的。
func TestUpsertDailyStatsAccumulates(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repoID := newStatsRepo(t, db)

	row := DailyStatsRow{
		RepositoryID: repoID,
		StatDate:     "2026-10-21",
		Pulls:        3,
		Pushes:       1,
		Deletes:      0,
		BytesPulled:  100,
		BytesPushed:  200,
		UpdatedAt:    1,
	}
	if err := db.UpsertDailyStats(ctx, row); err != nil {
		t.Fatalf("首次写入天桶: %v", err)
	}
	row.Pulls = 2
	row.Pushes = 0
	row.BytesPulled = 10
	row.BytesPushed = 0
	row.UpdatedAt = 2
	if err := db.UpsertDailyStats(ctx, row); err != nil {
		t.Fatalf("二次写入天桶: %v", err)
	}

	var rows int
	var pulls, pushes, bytesPulled, bytesPushed int64
	if err := db.QueryRow(ctx, `SELECT COUNT(*), pulls, pushes, bytes_pulled, bytes_pushed
		FROM traffic_stats_daily WHERE repository_id = ? AND stat_date = ?`,
		repoID, "2026-10-21").Scan(&rows, &pulls, &pushes, &bytesPulled, &bytesPushed); err != nil {
		t.Fatalf("读回天桶: %v", err)
	}
	if rows != 1 {
		t.Fatalf("行数 = %d，期望 1（同一 (仓库, 日期) 只有一行）", rows)
	}
	if pulls != 5 || pushes != 1 {
		t.Fatalf("pulls=%d pushes=%d，期望 5/1（必须累加，不是覆盖）", pulls, pushes)
	}
	if bytesPulled != 110 || bytesPushed != 200 {
		t.Fatalf("bytes_pulled=%d bytes_pushed=%d，期望 110/200", bytesPulled, bytesPushed)
	}
}

// TestUpsertTrafficIPAccumulates 锁定 IP 命中数是累加。
//
// ⚠️ 同一个网段在一小时里拉 100 次，应该是 hits=100 的一行，
//
//	而不是 100 行——否则这张表会按请求数膨胀。
func TestUpsertTrafficIPAccumulates(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repoID := newStatsRepo(t, db)

	row := TrafficIPRow{
		RepositoryID: repoID,
		BucketAt:     2026102912,
		IPHash:       "aabbccdd",
		Hits:         3,
		UpdatedAt:    1,
	}
	if err := db.UpsertTrafficIP(ctx, row); err != nil {
		t.Fatalf("首次写入 IP 行: %v", err)
	}
	row.Hits = 4
	row.UpdatedAt = 2
	if err := db.UpsertTrafficIP(ctx, row); err != nil {
		t.Fatalf("二次写入 IP 行: %v", err)
	}

	var rows int
	var hits int64
	if err := db.QueryRow(ctx, `SELECT COUNT(*), hits FROM traffic_stats_ip
		WHERE repository_id = ? AND bucket_at = ? AND ip_hash = ?`,
		repoID, int64(2026102912), "aabbccdd").Scan(&rows, &hits); err != nil {
		t.Fatalf("读回 IP 行: %v", err)
	}
	if rows != 1 {
		t.Fatalf("行数 = %d，期望 1", rows)
	}
	if hits != 7 {
		t.Fatalf("hits = %d，期望 7（必须累加，不是覆盖）", hits)
	}
}

// TestUpsertSQLPerDialect 锁定三方言的累加式 upsert 形状。
//
// ⚠️ SQLite/PostgreSQL 是 ON CONFLICT，MySQL 是 ON DUPLICATE KEY UPDATE。
//
//	写成覆盖（SET pulls = EXCLUDED.pulls）在任一方言下都是错的。
func TestUpsertSQLPerDialect(t *testing.T) {
	cases := []struct {
		dialect Dialect
		want    string
	}{
		{DialectSQLite, "ON CONFLICT (repository_id, stat_date) DO UPDATE SET pulls = traffic_stats_daily.pulls + EXCLUDED.pulls"},
		{DialectPostgres, "ON CONFLICT (repository_id, stat_date) DO UPDATE SET pulls = traffic_stats_daily.pulls + EXCLUDED.pulls"},
		{DialectMySQL, "ON DUPLICATE KEY UPDATE pulls = pulls + VALUES(pulls)"},
	}
	for _, tc := range cases {
		t.Run(string(tc.dialect), func(t *testing.T) {
			got := tc.dialect.Upsert("traffic_stats_daily", "repository_id, stat_date",
				[]string{"repository_id", "stat_date", "pulls", "pushes", "deletes",
					"bytes_pulled", "bytes_pushed", "updated_at"},
				[]string{"pulls", "pushes", "deletes", "bytes_pulled", "bytes_pushed"},
				"?, ?, ?, ?, ?, ?, ?, ?")
			if !strings.Contains(got, tc.want) {
				t.Fatalf("SQL 不含 %q：\n%s", tc.want, got)
			}
		})
	}
}

// ───────────────────────── 保留期清理 ─────────────────────────

// TestPruneHourlyStatsBoundary 锁定小时表清理的边界。
//
// ⚠️ 边界必须是**严格早于** cutoff：等于 cutoff 的那一桶要留着
//
//	（保留期是"最近 N 小时"，不是"更早于 N 小时前"）。
func TestPruneHourlyStatsBoundary(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repoID := newStatsRepo(t, db)

	const cutoff = int64(2026102912)
	old := cutoff - 3600
	edge := cutoff
	keep := cutoff + 3600
	for _, bucket := range []int64{old, edge, keep} {
		if err := db.UpsertHourlyStats(ctx, HourlyStatsRow{
			RepositoryID: repoID, BucketAt: bucket, Pulls: 1, UpdatedAt: 1,
		}); err != nil {
			t.Fatalf("写入小时桶 %d: %v", bucket, err)
		}
	}

	n, err := db.PruneHourlyStats(ctx, cutoff)
	if err != nil {
		t.Fatalf("清理小时桶: %v", err)
	}
	if n != 1 {
		t.Fatalf("删了 %d 行，期望 1（只有严格早于 cutoff 的那行）", n)
	}
	var left int
	if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM traffic_stats_hourly
		WHERE bucket_at >= ?`, cutoff).Scan(&left); err != nil {
		t.Fatalf("读回小时桶: %v", err)
	}
	if left != 2 {
		t.Fatalf("剩下 %d 行，期望 2（等于 cutoff 的必须留着）", left)
	}
}

// TestPruneDailyStatsBoundary 锁定天表清理的边界（按 stat_date 文本比较）。
func TestPruneDailyStatsBoundary(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repoID := newStatsRepo(t, db)

	for _, date := range []string{"2026-10-01", "2026-10-02", "2026-10-03"} {
		if err := db.UpsertDailyStats(ctx, DailyStatsRow{
			RepositoryID: repoID, StatDate: date, Pulls: 1, UpdatedAt: 1,
		}); err != nil {
			t.Fatalf("写入天桶 %s: %v", date, err)
		}
	}
	n, err := db.PruneDailyStats(ctx, "2026-10-03")
	if err != nil {
		t.Fatalf("清理天桶: %v", err)
	}
	if n != 2 {
		t.Fatalf("删了 %d 行，期望 2（早于 2026-10-03 的两行）", n)
	}
	var left int
	if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM traffic_stats_daily`).Scan(&left); err != nil {
		t.Fatalf("读回天桶: %v", err)
	}
	if left != 1 {
		t.Fatalf("剩下 %d 行，期望 1", left)
	}
}

// TestPruneTrafficIPBoundary 锁定 IP 表清理的边界。
func TestPruneTrafficIPBoundary(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repoID := newStatsRepo(t, db)

	const cutoff = int64(2026102912)
	for _, bucket := range []int64{cutoff - 3600, cutoff, cutoff + 3600} {
		if err := db.UpsertTrafficIP(ctx, TrafficIPRow{
			RepositoryID: repoID, BucketAt: bucket, IPHash: "hash", Hits: 1, UpdatedAt: 1,
		}); err != nil {
			t.Fatalf("写入 IP 行 %d: %v", bucket, err)
		}
	}
	n, err := db.PruneTrafficIP(ctx, cutoff)
	if err != nil {
		t.Fatalf("清理 IP 行: %v", err)
	}
	if n != 1 {
		t.Fatalf("删了 %d 行，期望 1", n)
	}
}

// ───────────────────────── 内部设置项（汇总游标） ─────────────────────────

// TestUpsertInternalSettingKeepsVersion 锁定内部设置项**不动 version**。
//
// ⚠️ system_settings.version 是设置页的乐观锁版本（取 MAX(version)）。
//
//	汇总游标每小时写一次，跟着涨版本的话，
//	管理员开着设置页时一次后台汇总就能让保存报"设置已被他人修改"。
func TestUpsertInternalSettingKeepsVersion(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	if err := db.UpsertSetting(ctx, "core.public_url", "http://x", "test"); err != nil {
		t.Fatalf("写普通设置: %v", err)
	}
	before, err := db.GlobalSettingsVersion(ctx)
	if err != nil {
		t.Fatalf("读全局版本: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := db.UpsertInternalSetting(ctx, "stats.rollup_cursor", int64(i), "rollup"); err != nil {
			t.Fatalf("写内部设置: %v", err)
		}
	}
	after, err := db.GlobalSettingsVersion(ctx)
	if err != nil {
		t.Fatalf("读全局版本: %v", err)
	}
	if after != before {
		t.Fatalf("全局版本从 %d 变成 %d，内部设置项不该动它", before, after)
	}
	s, err := db.GetSetting(ctx, "stats.rollup_cursor")
	if err != nil {
		t.Fatalf("读内部设置: %v", err)
	}
	if s.Value != float64(2) {
		t.Fatalf("游标值 = %v（%T），期望 2", s.Value, s.Value)
	}
}

// ───────────────────────── 迁移 ─────────────────────────

// TestIPMigrationAppliedOnSQLite 锁定 SQLite 上的迁移真的建了表。
func TestIPMigrationAppliedOnSQLite(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	var applied int
	if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM schema_migrations
		WHERE version = ?`, "0006_stats_rollup.sql").Scan(&applied); err != nil {
		t.Fatalf("读 schema_migrations: %v", err)
	}
	if applied != 1 {
		t.Fatal("0006_stats_rollup.sql 没有被记录为已应用")
	}
	var table string
	if err := db.QueryRow(ctx, `SELECT name FROM sqlite_master
		WHERE type = 'table' AND name = 'traffic_stats_ip'`).Scan(&table); err != nil {
		t.Fatalf("查 traffic_stats_ip: %v", err)
	}
	if table != "traffic_stats_ip" {
		t.Fatalf("表名 = %q", table)
	}
}

// TestIPMigrationEquivalentAcrossDialects 锁定三方言的迁移语义等价。
//
// ⚠️ postgres / mysql 本机跑不了，只能静态比对：
//
//	三份必须有同样的列、同样的唯一键、同样的外键级联。
//	少一列 = 那个方言上写入直接报错（"no such column"）。
func TestIPMigrationEquivalentAcrossDialects(t *testing.T) {
	cols := []string{"repository_id", "bucket_at", "ip_hash", "hits", "updated_at"}
	for _, dialect := range []string{"sqlite", "postgres", "mysql"} {
		t.Run(dialect, func(t *testing.T) {
			b, err := migrationFS.ReadFile("migrations/" + dialect + "/0006_stats_rollup.sql")
			if err != nil {
				t.Fatalf("读迁移: %v", err)
			}
			s := string(b)
			for _, c := range cols {
				if !strings.Contains(s, c) {
					t.Fatalf("%s 方言的迁移缺列 %s", dialect, c)
				}
			}
			if !strings.Contains(s, "UNIQUE") {
				t.Fatalf("%s 方言的迁移缺唯一键（累加式 upsert 靠它）", dialect)
			}
			if !strings.Contains(s, "ON DELETE CASCADE") {
				t.Fatalf("%s 方言的迁移缺外键级联（仓库删了 IP 记录要跟着删）", dialect)
			}
			if !strings.Contains(s, "traffic_stats_ip") {
				t.Fatalf("%s 方言的迁移建的不是 traffic_stats_ip", dialect)
			}
		})
	}
}

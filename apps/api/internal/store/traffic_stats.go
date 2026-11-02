package store

import (
	"context"
	"database/sql"
	"fmt"
)

// 流量统计的持久化。
//
// 设计依据：docs/design/10-traffic-stats.md §3（存储）、§3.2（累加式 upsert）。
//
// ⚠️ 三张表都是**累加**语义：同一行会被写多次（刷盘、汇总、重试），
//    所以写入一律走 Upsert 的累加分支，绝不覆盖——
//    写成 `SET pulls = EXCLUDED.pulls` 会把同一小时里先前的数据抹掉。
//
// ⚠️ 方言差异（ON CONFLICT / ON DUPLICATE KEY）由 Dialect.Upsert 屏蔽，
//    这里不出现任何方言分支。

// HourlyStatsRow 是小时粒度流量统计的一行。
type HourlyStatsRow struct {
	RepositoryID int64
	// BucketAt 是小时桶起点（unix 秒，整小时）。
	BucketAt    int64
	Pulls       int64
	Pushes      int64
	Deletes     int64
	BytesPulled int64
	BytesPushed int64
	UpdatedAt   int64
}

// DailyStatsRow 是天粒度流量统计的一行。
type DailyStatsRow struct {
	RepositoryID int64
	// StatDate 是 'YYYY-MM-DD'（UTC）。
	StatDate    string
	Pulls       int64
	Pushes      int64
	Deletes     int64
	BytesPulled int64
	BytesPushed int64
	UpdatedAt   int64
}

// TrafficIPRow 是匿名客户端 IP 的一行（截断+哈希后的网段）。
type TrafficIPRow struct {
	RepositoryID int64
	// BucketAt 是小时桶起点（unix 秒，整小时）。
	BucketAt int64
	// IPHash 是截断后地址的 sha256 十六进制散列（64 字符）。
	IPHash    string
	Hits      int64
	UpdatedAt int64
}

// UpsertHourlyStats 累加式写入小时桶。
func (d *DB) UpsertHourlyStats(ctx context.Context, r HourlyStatsRow) error {
	return d.Upsert(ctx, "traffic_stats_hourly", "repository_id, bucket_at",
		[]string{"repository_id", "bucket_at", "pulls", "pushes", "deletes",
			"bytes_pulled", "bytes_pushed", "updated_at"},
		[]string{"pulls", "pushes", "deletes", "bytes_pulled", "bytes_pushed"},
		[]any{r.RepositoryID, r.BucketAt, r.Pulls, r.Pushes, r.Deletes,
			r.BytesPulled, r.BytesPushed, r.UpdatedAt})
}

// UpsertDailyStats 累加式写入天桶。
//
// ⚠️ 天桶也是**累加**：一天由 24 个小时桶累加而成，
//
//	重复汇总会重复计数——所以汇总必须幂等（见 service/stats_rollup.go）。
func (d *DB) UpsertDailyStats(ctx context.Context, r DailyStatsRow) error {
	return d.Upsert(ctx, "traffic_stats_daily", "repository_id, stat_date",
		[]string{"repository_id", "stat_date", "pulls", "pushes", "deletes",
			"bytes_pulled", "bytes_pushed", "updated_at"},
		[]string{"pulls", "pushes", "deletes", "bytes_pulled", "bytes_pushed"},
		[]any{r.RepositoryID, r.StatDate, r.Pulls, r.Pushes, r.Deletes,
			r.BytesPulled, r.BytesPushed, r.UpdatedAt})
}

// UpsertDailyStats 在事务里累加式写入天桶。
//
// 汇总任务用它把"写天桶"与"推游标"放进同一个事务：
// 两者必须同时成功或同时不发生，否则崩在中间会重复计数。
func (t *Tx) UpsertDailyStats(ctx context.Context, r DailyStatsRow) error {
	q := t.Dialect.Upsert("traffic_stats_daily", "repository_id, stat_date",
		[]string{"repository_id", "stat_date", "pulls", "pushes", "deletes",
			"bytes_pulled", "bytes_pushed", "updated_at"},
		[]string{"pulls", "pushes", "deletes", "bytes_pulled", "bytes_pushed"},
		Placeholders(8))
	_, err := t.Exec(ctx, q, r.RepositoryID, r.StatDate, r.Pulls, r.Pushes, r.Deletes,
		r.BytesPulled, r.BytesPushed, r.UpdatedAt)
	return err
}

// UpsertTrafficIP 累加式写入匿名 IP 命中数。
func (d *DB) UpsertTrafficIP(ctx context.Context, r TrafficIPRow) error {
	return d.Upsert(ctx, "traffic_stats_ip", "repository_id, bucket_at, ip_hash",
		[]string{"repository_id", "bucket_at", "ip_hash", "hits", "updated_at"},
		[]string{"hits"},
		[]any{r.RepositoryID, r.BucketAt, r.IPHash, r.Hits, r.UpdatedAt})
}

// HourlyStatsRange 取 [from, to) 区间内的小时桶（按时间、仓库排序）。
func (d *DB) HourlyStatsRange(ctx context.Context, from, to int64) ([]HourlyStatsRow, error) {
	rows, err := d.Query(ctx, `SELECT repository_id, bucket_at, pulls, pushes, deletes,
		bytes_pulled, bytes_pushed
		FROM traffic_stats_hourly
		WHERE bucket_at >= ? AND bucket_at < ?
		ORDER BY bucket_at, repository_id`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HourlyStatsRow
	for rows.Next() {
		var r HourlyStatsRow
		if err := rows.Scan(&r.RepositoryID, &r.BucketAt, &r.Pulls, &r.Pushes, &r.Deletes,
			&r.BytesPulled, &r.BytesPushed); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MinHourlyBucketAt 返回 before 之前最早的小时桶。
//
// 没有数据时返回 ok=false（不是 0）——0 是一个合法的桶起点，
// 拿它当"没有数据"会让调用方误判。
func (d *DB) MinHourlyBucketAt(ctx context.Context, before int64) (int64, bool, error) {
	var v sql.NullInt64
	if err := d.QueryRow(ctx,
		`SELECT MIN(bucket_at) FROM traffic_stats_hourly WHERE bucket_at < ?`, before).
		Scan(&v); err != nil {
		return 0, false, err
	}
	if !v.Valid {
		return 0, false, nil
	}
	return v.Int64, true, nil
}

// pruneBatchSize 是单条 DELETE 最多删的行数。
//
// ⚠️ SQLite 上 DELETE 会一直持有写锁：一次删几十万行会把整个库的写都堵死
//
//	（docs/design/10-traffic-stats.md §3.4）。分批把持锁时间切碎。
const pruneBatchSize = 500

// pruneMaxBatches 是单次清理的最大批数（防止删不干净时无限循环）。
const pruneMaxBatches = 10000

// PruneHourlyStats 删掉 bucket_at < before 的小时桶，返回删掉的行数。
func (d *DB) PruneHourlyStats(ctx context.Context, before int64) (int64, error) {
	return d.pruneByCond(ctx, "traffic_stats_hourly", "bucket_at < ?", before)
}

// PruneTrafficIP 删掉 bucket_at < before 的匿名 IP 记录，返回删掉的行数。
func (d *DB) PruneTrafficIP(ctx context.Context, before int64) (int64, error) {
	return d.pruneByCond(ctx, "traffic_stats_ip", "bucket_at < ?", before)
}

// PruneDailyStats 删掉 stat_date < beforeDate 的天桶，返回删掉的行数。
//
// stat_date 是 'YYYY-MM-DD' 文本，三方言的字符串比较都是字典序，
// 与日期的先后一致。
func (d *DB) PruneDailyStats(ctx context.Context, beforeDate string) (int64, error) {
	return d.pruneByCond(ctx, "traffic_stats_daily", "stat_date < ?", beforeDate)
}

// pruneByCond 分批删掉满足 cond 的行。
//
// 分两步：先按主键取一批 id，再用 `id IN (...)` 删。
// 为什么不直接 `DELETE ... WHERE cond`：
//   - 单条语句要删的行数不可控（SQLite 上持锁时间随之不可控）；
//   - `IN (SELECT ... LIMIT n)` 在 MySQL 上不被支持（各版本行为不一），
//     显式列出 id 三个方言行为一致。
func (d *DB) pruneByCond(ctx context.Context, table, cond string, arg any) (int64, error) {
	var total int64
	for i := 0; i < pruneMaxBatches; i++ {
		ids, err := d.idsForPrune(ctx, table, cond, arg)
		if err != nil {
			return total, err
		}
		if len(ids) == 0 {
			return total, nil
		}
		args := make([]any, len(ids))
		for j, id := range ids {
			args[j] = id
		}
		res, err := d.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE id IN (%s)`,
			table, Placeholders(len(ids))), args...)
		if err != nil {
			return total, fmt.Errorf("清理 %s: %w", table, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, fmt.Errorf("读取 %s 的删除行数: %w", table, err)
		}
		total += n
		if len(ids) < pruneBatchSize {
			return total, nil
		}
	}
	return total, fmt.Errorf("清理 %s：超过 %d 批仍未删完", table, pruneMaxBatches)
}

// idsForPrune 取一批待删的主键。
func (d *DB) idsForPrune(ctx context.Context, table, cond string, arg any) ([]int64, error) {
	rows, err := d.Query(ctx, fmt.Sprintf(
		`SELECT id FROM %s WHERE %s ORDER BY id LIMIT ?`, table, cond), arg, pruneBatchSize)
	if err != nil {
		return nil, fmt.Errorf("查询待清理行: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// UpsertInternalSetting 写内部设置项，**不动 version**。
//
// ⚠️ 为什么不动 version：system_settings.version 是设置页的乐观锁版本
//
//	（GlobalSettingsVersion 取的是 MAX(version)）。汇总游标每小时都要写一次，
//	跟着涨版本的话，管理员开着设置页时一次后台汇总就能让保存报
//	"设置已被他人修改"——那是假冲突，必须避免。
func (d *DB) UpsertInternalSetting(ctx context.Context, key string, value any, updatedBy string) error {
	return upsertInternalSetting(ctx, d.Dialect(), d.Exec, key, value, updatedBy)
}

// UpsertInternalSetting 在事务里写内部设置项（不动 version）。
func (t *Tx) UpsertInternalSetting(ctx context.Context, key string, value any, updatedBy string) error {
	return upsertInternalSetting(ctx, t.Dialect, t.Exec, key, value, updatedBy)
}

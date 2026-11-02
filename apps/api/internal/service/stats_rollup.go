package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/lau6code/lau6-cr/apps/api/internal/store"
)

// 流量统计的**汇总与清理**后台任务。
//
// 设计依据：docs/design/10-traffic-stats.md §3.3（小时→天汇总）、
// §3.4（保留期清理）、§10 ADR 9/10/11/13。
//
// 三条不可让步的规则：
//
//  1. **只汇总已关闭的小时**（bucket_at < 当前小时起点）。
//     当前小时的桶还在累加，提前汇总会让它在下一轮再被累加一次。
//  2. **日期按 UTC 切分**（'YYYY-MM-DD'）。按本地日切会让"今天"依赖查询方时区。
//  3. **幂等**：累加式 upsert 不可重入，所以"写天桶"与"推游标"必须在同一个事务里，
//     否则崩在中间会让同一小时被累加两次。

// rollupCursorKey 是小时→天汇总的水位游标。
//
// 存在 system_settings 表里（docs/design/10-traffic-stats.md §3.3）。
//
// ⚠️ 它是**内部键**：不进设置页、也不能被 PATCH 改
//
//	（docs/design/14-settings.md §3.9）。
const rollupCursorKey = "stats.rollup_cursor"

// rollupDelay 是"整点之后等多久再汇总"。
//
// 为什么等：小时桶的数据是**刷盘**写进去的（默认 60s 一轮）。
// 整点一到就汇总，看到的可能是一个还没落库的小时——
// 那一小时的量就永远进不了天表（游标只推过真正汇总过的桶）。
const rollupDelay = 2 * time.Minute

// StatsRollupOptions 是汇总/清理任务的配置。
type StatsRollupOptions struct {
	// HourlyRetention 返回小时表的保留时长。
	HourlyRetention func() time.Duration
	// DailyRetention 返回天表的保留时长。
	DailyRetention func() time.Duration
	// PruneHour 返回每日清理的 UTC 小时（0..23）。
	PruneHour func() int
	// Logger 记日志；为 nil 时用 slog.Default()。
	Logger *slog.Logger
}

// StatsRollup 是流量统计的汇总/清理任务。
type StatsRollup struct {
	db   *store.DB
	opts StatsRollupOptions
	// nowFn 返回当前时间。
	//
	// 默认 time.Now；测试注入固定时钟，让"当前小时"的边界可复现。
	nowFn func() time.Time
}

// NewStatsRollup 构造汇总任务。
func NewStatsRollup(db *store.DB, o StatsRollupOptions) *StatsRollup {
	if o.Logger == nil {
		// ⚠️ 不能让任务"没有日志"：失败只发生在后台，
		//    没人看得到的话就是静默丢数据。
		o.Logger = slog.Default()
	}
	return &StatsRollup{db: db, opts: o, nowFn: time.Now}
}

// nowHour 返回当前整点小时的 unix 秒。
func (r *StatsRollup) nowHour() int64 {
	now := r.nowFn().Unix()
	return now - now%3600
}

// Run 跑汇总/清理循环，直到 ctx 结束才返回。
func (r *StatsRollup) Run(ctx context.Context) {
	if r.db == nil {
		r.opts.Logger.Error("流量汇总任务没有数据库连接，已退出")
		return
	}
	if ctx.Err() != nil {
		return
	}
	// 启动时先跑一轮：重启后不用干等到下一个整点。
	r.rollupOnce(ctx)
	r.pruneOnce(ctx)

	for {
		now := r.nowFn().UTC()
		// 下一个整点 + 汇总延迟。
		nextRollup := now.Truncate(time.Hour).Add(time.Hour).Add(rollupDelay)
		next, doPrune := nextRollup, false
		if hour, ok := r.pruneHour(); ok {
			nextPrune := nextPruneTime(now, hour)
			if nextPrune.Before(next) {
				next, doPrune = nextPrune, true
			}
		}
		// ⚠️ 清理时刻非法时 pruneHour() 已经报过错了：这一轮不排清理，
		//    汇总照常按整点走（下一轮再试，配置可能已被改好）。

		timer := time.NewTimer(next.Sub(now))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if doPrune {
			r.pruneOnce(ctx)
		} else {
			r.rollupOnce(ctx)
		}
	}
}

// nextPruneTime 返回下一个清理时刻（UTC 的 hour:00）。
func nextPruneTime(now time.Time, hour int) time.Time {
	t := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, time.UTC)
	if !t.After(now) {
		t = t.AddDate(0, 0, 1)
	}
	return t
}

// rollupOnce 把**已关闭**的小时桶累加进天桶。
//
// ⚠️ 失败时不推进游标：下一轮从同一个桶重来，
//
//	而"写天桶 + 推游标"在同一个事务里，所以重来不会重复计数。
func (r *StatsRollup) rollupOnce(ctx context.Context) {
	nowHour := r.nowHour()
	cursor, err := r.loadCursor(ctx)
	if err != nil {
		r.opts.Logger.Error("读取汇总游标失败，本轮跳过汇总", "err", err)
		return
	}
	from, ok, err := r.rollupFrom(ctx, cursor, nowHour)
	if err != nil {
		r.opts.Logger.Error("确定汇总起点失败，本轮跳过汇总", "err", err)
		return
	}
	if !ok {
		return // 没有已关闭的小时要汇总
	}
	rows, err := r.db.HourlyStatsRange(ctx, from, nowHour)
	if err != nil {
		r.opts.Logger.Error("读取小时桶失败，本轮跳过汇总", "err", err)
		return
	}
	if len(rows) == 0 {
		return
	}
	byBucket := map[int64][]store.HourlyStatsRow{}
	for _, row := range rows {
		byBucket[row.BucketAt] = append(byBucket[row.BucketAt], row)
	}
	buckets := make([]int64, 0, len(byBucket))
	for b := range byBucket {
		buckets = append(buckets, b)
	}
	sort.Slice(buckets, func(i, j int) bool { return buckets[i] < buckets[j] })

	rolled := 0
	for _, b := range buckets {
		if err := r.rollupBucket(ctx, b, byBucket[b]); err != nil {
			r.opts.Logger.Error("汇总小时桶失败，本轮中止汇总", "err", err, "bucket", b)
			return
		}
		rolled++
	}
	r.opts.Logger.Info("流量统计已汇总到天表",
		"buckets", rolled, "from", from, "to", nowHour)
}

// rollupFrom 返回本轮要汇总的起点（不含）。
//
// ok=false 表示"没有要汇总的小时"。
func (r *StatsRollup) rollupFrom(ctx context.Context, cursor, nowHour int64) (int64, bool, error) {
	if cursor > 0 {
		from := cursor + 3600
		if from >= nowHour {
			return 0, false, nil
		}
		return from, true, nil
	}
	// 从来没汇总过：从最早的小时桶开始（不能假设 0，
	// 否则会把成千上万个空小时当成待汇总区间）。
	min, ok, err := r.db.MinHourlyBucketAt(ctx, nowHour)
	if err != nil {
		return 0, false, err
	}
	if !ok {
		return 0, false, nil
	}
	from := alignHour(min)
	if from >= nowHour {
		return 0, false, nil
	}
	return from, true, nil
}

// rollupBucket 把一个小时桶累加进天桶，并推进游标。
//
// ⚠️ 天桶写入与游标推进在**同一个事务**里：
//
//	累加式 upsert 不可重入。分成两步的话，崩在两步之间时
//	下一轮会把同一个小时再累加一次——天表直接翻倍。
func (r *StatsRollup) rollupBucket(ctx context.Context, bucket int64, rows []store.HourlyStatsRow) error {
	date := time.Unix(bucket, 0).UTC().Format("2006-01-02")
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("开启汇总事务: %w", err)
	}
	defer func() {
		// 提交后再回滚会返回 sql.ErrTxDone，那是正常路径，不是故障。
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			r.opts.Logger.Error("回滚汇总事务失败", "err", err)
		}
	}()
	for _, row := range rows {
		daily := store.DailyStatsRow{
			RepositoryID: row.RepositoryID,
			StatDate:     date,
			Pulls:        row.Pulls,
			Pushes:       row.Pushes,
			Deletes:      row.Deletes,
			BytesPulled:  row.BytesPulled,
			BytesPushed:  row.BytesPushed,
			UpdatedAt:    store.Now(),
		}
		if err := tx.UpsertDailyStats(ctx, daily); err != nil {
			return fmt.Errorf("写天桶 %s: %w", date, err)
		}
	}
	if err := tx.UpsertInternalSetting(ctx, rollupCursorKey, bucket, "stats-rollup"); err != nil {
		return fmt.Errorf("写汇总游标: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交汇总事务: %w", err)
	}
	return nil
}

// loadCursor 读汇总游标。
//
// 游标不存在 = 从来没汇总过（不是错误，返回 0）。
func (r *StatsRollup) loadCursor(ctx context.Context) (int64, error) {
	s, err := r.db.GetSetting(ctx, rollupCursorKey)
	if errors.Is(err, store.ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	switch v := s.Value.(type) {
	case float64:
		return int64(v), nil
	case int64:
		return v, nil
	case int:
		return int64(v), nil
	}
	return 0, fmt.Errorf("汇总游标的值类型不对: %T", s.Value)
}

// pruneOnce 按保留期清理过期的统计行。
func (r *StatsRollup) pruneOnce(ctx context.Context) {
	hourly, daily, ok := r.retentions()
	if !ok {
		return
	}
	now := r.nowFn()
	hourlyCutoff := now.Add(-hourly).Unix()

	// 小时表只删**已汇总**的小时：没汇总的删了就永远进不了天表。
	cursor, err := r.loadCursor(ctx)
	if err != nil {
		r.opts.Logger.Error("读取汇总游标失败，跳过小时表清理", "err", err)
		return
	}
	if cursor <= 0 {
		// 一小时都没汇总过 → 小时表里的数据全都还没进天表，删了就丢。
		r.opts.Logger.Warn("尚未汇总过任何小时，跳过小时表清理")
	} else {
		cutoff := hourlyCutoff
		if cursor < cutoff {
			cutoff = cursor
		}
		n, err := r.db.PruneHourlyStats(ctx, cutoff)
		if err != nil {
			r.opts.Logger.Error("清理过期小时桶失败", "err", err)
		} else {
			r.opts.Logger.Info("已清理过期小时桶", "rows", n, "before", cutoff)
		}
	}

	// 天表是终态（没有更细的粒度再往上汇总），直接按保留期删。
	dailyCutoff := now.Add(-daily).UTC().Format("2006-01-02")
	n, err := r.db.PruneDailyStats(ctx, dailyCutoff)
	if err != nil {
		r.opts.Logger.Error("清理过期天桶失败", "err", err)
	} else {
		r.opts.Logger.Info("已清理过期天桶", "rows", n, "before", dailyCutoff)
	}

	// IP 表是小时粒度，跟着小时表的保留期走。
	n, err = r.db.PruneTrafficIP(ctx, hourlyCutoff)
	if err != nil {
		r.opts.Logger.Error("清理过期 IP 记录失败", "err", err)
	} else {
		r.opts.Logger.Info("已清理过期 IP 记录", "rows", n, "before", hourlyCutoff)
	}
}

// retentions 取两个保留期。
//
// ⚠️ 保留期 <= 0 是**非法配置**：当成"保留 0"会把整张表删光。
//
//	这里报错并跳过本轮清理，绝不拿 0 当"全删"。
func (r *StatsRollup) retentions() (time.Duration, time.Duration, bool) {
	if r.opts.HourlyRetention == nil || r.opts.DailyRetention == nil {
		r.opts.Logger.Error("保留期未接线，跳过本轮清理")
		return 0, 0, false
	}
	hourly := r.opts.HourlyRetention()
	daily := r.opts.DailyRetention()
	if hourly <= 0 || daily <= 0 {
		r.opts.Logger.Error("保留期非法，跳过本轮清理", "hourly", hourly, "daily", daily)
		return 0, 0, false
	}
	return hourly, daily, true
}

// pruneHour 取每日清理的 UTC 小时。
//
// 非法值（不在 0..23）报错并跳过本轮——
// time.Date 会把 25 归一成次日 1 点，那样清理会在**错误的时刻**静默发生。
func (r *StatsRollup) pruneHour() (int, bool) {
	if r.opts.PruneHour == nil {
		r.opts.Logger.Error("清理时刻未接线，跳过本轮清理")
		return 0, false
	}
	hour := r.opts.PruneHour()
	if hour < 0 || hour > 23 {
		r.opts.Logger.Error("清理时刻非法，跳过本轮清理", "prune_hour", hour)
		return 0, false
	}
	return hour, true
}

// alignHour 把时间戳向上对齐到整小时起点。
//
// 小时桶本来就是整点（刷盘时按 currentHour() 取整），
// 这里只是防御：万一桶起点不是整点，按 3600 步进会漏桶。
func alignHour(unix int64) int64 {
	const hour = 3600
	if rem := unix % hour; rem != 0 {
		return unix + (hour - rem)
	}
	return unix
}

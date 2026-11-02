package service

import (
	"context"
	"time"

	"github.com/lau6code/lau6-cr/apps/api/internal/store"
)

// 流量取数（只读查询）。
//
// 与 StatsAggregator 分开：聚合器只持有内存桶与一把锁，
// 取数服务只持有 *store.DB——两者不共享状态，耦合在一处只会互相干扰。

// TrafficPoint 是取数接口的一行。
type TrafficPoint struct {
	Date        string `json:"date"`
	Pulls       int64  `json:"pulls"`
	Pushes      int64  `json:"pushes"`
	Deletes     int64  `json:"deletes"`
	BytesPulled int64  `json:"bytes_pulled"`
	BytesPushed int64  `json:"bytes_pushed"`
}

// TrafficTotals 是汇总行。
type TrafficTotals struct {
	Pulls       int64 `json:"pulls"`
	Pushes      int64 `json:"pushes"`
	Deletes     int64 `json:"deletes"`
	BytesPulled int64 `json:"bytes_pulled"`
	BytesPushed int64 `json:"bytes_pushed"`
}

// TrafficSeries 是一段时间的流量序列。
type TrafficSeries struct {
	Repository  string         `json:"repository,omitempty"`
	Granularity string         `json:"granularity"`
	Totals      TrafficTotals  `json:"totals"`
	Series      []TrafficPoint `json:"series"`
}

// TrafficService 提供流量取数。
type TrafficService struct {
	db *store.DB
}

// NewTrafficService 构造流量服务。
func NewTrafficService(db *store.DB) *TrafficService {
	return &TrafficService{db: db}
}

// RepositoryTraffic 取单仓库的流量序列。
func (t *TrafficService) RepositoryTraffic(ctx context.Context, repoID int64, from, to int64, granularity string) (*TrafficSeries, error) {
	series := &TrafficSeries{Granularity: granularity}
	if granularity == "day" {
		rows, err := t.db.Query(ctx, `SELECT stat_date, pulls, pushes, deletes, bytes_pulled, bytes_pushed
			FROM traffic_stats_daily
			WHERE repository_id = ? AND stat_date >= ? AND stat_date <= ?
			ORDER BY stat_date`, repoID, dayOf(from), dayOf(to))
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var p TrafficPoint
			if err := rows.Scan(&p.Date, &p.Pulls, &p.Pushes, &p.Deletes, &p.BytesPulled, &p.BytesPushed); err != nil {
				return nil, err
			}
			series.Series = append(series.Series, p)
			accumulate(series, p)
		}
		return series, rows.Err()
	}

	rows, err := t.db.Query(ctx, `SELECT bucket_at, pulls, pushes, deletes, bytes_pulled, bytes_pushed
		FROM traffic_stats_hourly
		WHERE repository_id = ? AND bucket_at >= ? AND bucket_at <= ?
		ORDER BY bucket_at`, repoID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var bucket int64
		var p TrafficPoint
		if err := rows.Scan(&bucket, &p.Pulls, &p.Pushes, &p.Deletes, &p.BytesPulled, &p.BytesPushed); err != nil {
			return nil, err
		}
		p.Date = time.Unix(bucket, 0).UTC().Format("2006-01-02T15:00:00Z")
		series.Series = append(series.Series, p)
		accumulate(series, p)
	}
	return series, rows.Err()
}

// SystemTraffic 取全站汇总。
func (t *TrafficService) SystemTraffic(ctx context.Context, from, to int64) (*TrafficSeries, error) {
	series := &TrafficSeries{Granularity: "hour"}
	rows, err := t.db.Query(ctx, `SELECT bucket_at, pulls, pushes, deletes, bytes_pulled, bytes_pushed
		FROM traffic_stats_hourly WHERE bucket_at >= ? AND bucket_at <= ? ORDER BY bucket_at`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var bucket int64
		var p TrafficPoint
		if err := rows.Scan(&bucket, &p.Pulls, &p.Pushes, &p.Deletes, &p.BytesPulled, &p.BytesPushed); err != nil {
			return nil, err
		}
		p.Date = time.Unix(bucket, 0).UTC().Format("2006-01-02T15:00:00Z")
		series.Series = append(series.Series, p)
		accumulate(series, p)
	}
	return series, rows.Err()
}

// TopRepositories 取流量最高的仓库。
func (t *TrafficService) TopRepositories(ctx context.Context, metric string, limit int, from, to int64) ([]map[string]any, error) {
	col := "bytes_pulled"
	switch metric {
	case "bytes_pulled", "bytes_pushed", "pulls", "pushes":
		col = metric
	}
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	// ⚠️ 用 CONCAT() 而不是 `||`：
	//    postgres / sqlite 里 `||` 是字符串拼接，
	//    但 **MySQL 里 `||` 是逻辑 OR**（除非开 PIPES_AS_CONCAT）——
	//    那样拼出来的是 0/1，不是 "项目/仓库"。
	//    CONCAT() 三个方言都有（sqlite 3.44+）。
	rows, err := t.db.Query(ctx, `SELECT CONCAT(p.name, '/', r.name) AS full_name,
			SUM(h.`+col+`) AS total
		FROM traffic_stats_hourly h
		JOIN repositories r ON r.id = h.repository_id
		JOIN projects p ON p.id = r.project_id
		WHERE h.bucket_at >= ? AND h.bucket_at <= ?
		GROUP BY full_name ORDER BY total DESC LIMIT ?`, from, to, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var name string
		var total int64
		if err := rows.Scan(&name, &total); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"repository": name, "total": total})
	}
	return out, rows.Err()
}

func accumulate(s *TrafficSeries, p TrafficPoint) {
	s.Totals.Pulls += p.Pulls
	s.Totals.Pushes += p.Pushes
	s.Totals.Deletes += p.Deletes
	s.Totals.BytesPulled += p.BytesPulled
	s.Totals.BytesPushed += p.BytesPushed
}

func dayOf(unix int64) string {
	return time.Unix(unix, 0).UTC().Format("2006-01-02")
}

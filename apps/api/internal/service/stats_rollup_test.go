package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lau6code/lau6-cr/apps/api/internal/config"
	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
	"github.com/lau6code/lau6-cr/apps/api/internal/store"
)

// ───────────────────────── 测试脚手架 ─────────────────────────

// newStatsRepo 建一个项目 + 仓库，返回仓库 ID。
func newStatsRepo(t *testing.T, db *store.DB) int64 {
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

// fixedNow 返回一个固定的 UTC 时间（测试用）。
func fixedNow() time.Time {
	return time.Date(2026, 10, 29, 12, 30, 0, 0, time.UTC)
}

// newTestRollup 造一个时钟固定的汇总任务。
func newTestRollup(t *testing.T, db *store.DB) *StatsRollup {
	t.Helper()
	return newTestRollupAt(t, db, fixedNow())
}

// newTestRollupAt 造一个时钟固定在指定时刻的汇总任务。
func newTestRollupAt(t *testing.T, db *store.DB, now time.Time) *StatsRollup {
	t.Helper()
	r := NewStatsRollup(db, StatsRollupOptions{
		HourlyRetention: func() time.Duration { return 336 * time.Hour },
		DailyRetention:  func() time.Duration { return 9600 * time.Hour },
		PruneHour:       func() int { return 3 },
		// 日志丢弃：测试输出里只留失败信息。
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	r.nowFn = func() time.Time { return now }
	return r
}

// ipBuckets 取出聚合器当前的 IP 桶。
func ipBuckets(a *StatsAggregator) map[ipKey]int64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := map[ipKey]int64{}
	for k, b := range a.ipBuckets {
		out[k] = b.hits.Load()
	}
	return out
}

// ───────────────────────── IP 截断 + 哈希 ─────────────────────────

// TestTruncateIP 锁定截断规则：IPv4 → /24，IPv6 → /48。
//
// ⚠️ 截断规则是隐私边界：截多了能定位到个人，截少了区分不出网段
//
//	（docs/design/10-traffic-stats.md §6）。
func TestTruncateIP(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"192.168.1.55", "192.168.1.0"},
		{"10.0.0.1", "10.0.0.0"},
		{"203.0.113.200", "203.0.113.0"},
		{"2001:db8:1234:5678::1", "2001:db8:1234::"},
		{"2001:db8:1234:5678:9abc:def0:1234:5678", "2001:db8:1234::"},
		{"::1", "::"},
		// IPv4 映射形式按 IPv4 算。
		{"::ffff:192.168.1.55", "192.168.1.0"},
		// 解析不了 → 空串（不记），不能把原始字符串当地址。
		{"not-an-ip", ""},
		{"", ""},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := truncateIP(tc.in); got != tc.want {
				t.Fatalf("truncateIP(%q) = %q，期望 %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestHashTruncatedIP 锁定散列是"截断之后"的 sha256。
//
// ⚠️ 顺序反了（先哈希再截断）就等于没截断——
//
//	散列不可逆，截断一个散列毫无意义。
func TestHashTruncatedIP(t *testing.T) {
	got := hashTruncatedIP("192.168.1.55")
	sum := sha256.Sum256([]byte("192.168.1.0"))
	want := hex.EncodeToString(sum[:])
	if got != want {
		t.Fatalf("hashTruncatedIP = %q，期望 %q", got, want)
	}
	if len(got) != 64 {
		t.Fatalf("散列长度 = %d，期望 64（sha256 十六进制）", len(got))
	}
	// 同一网段的两个地址必须落到同一个散列。
	if hashTruncatedIP("192.168.1.56") != got {
		t.Fatal("同一 /24 的两个地址散列必须相同")
	}
	// 不同网段必须不同。
	if hashTruncatedIP("192.168.2.56") == got {
		t.Fatal("不同 /24 的地址散列必须不同")
	}
	// 解析不了的地址不产生散列。
	if hashTruncatedIP("nope") != "" {
		t.Fatal("非法地址不该产生散列")
	}
}

// ───────────────────────── IP 记录链路 ─────────────────────────

// TestNoteIPDisabledIsNoop 锁定"默认关闭 = 零开销"。
//
// ⚠️ stats.record_ip 默认 false（有隐私后果，ADR 14）。
//
//	关闭时连桶都不能建，更不能落库。
func TestNoteIPDisabledIsNoop(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	newStatsRepo(t, db)

	a := NewStatsAggregator(StatsOptions{Enabled: true, DB: db})
	defer a.Close()

	a.NoteIP("team-a/app", "192.168.1.55")
	if got := ipBuckets(a); len(got) != 0 {
		t.Fatalf("关闭时建了 %d 个 IP 桶，期望 0", len(got))
	}
	if err := a.Flush(ctx); err != nil {
		t.Fatalf("刷盘: %v", err)
	}
	var rows int
	if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM traffic_stats_ip`).Scan(&rows); err != nil {
		t.Fatalf("读 IP 表: %v", err)
	}
	if rows != 0 {
		t.Fatalf("关闭时写了 %d 行 IP 记录，期望 0", rows)
	}
}

// TestNoteIPGroupsByNetwork 锁定同一个网段合并成一个桶。
func TestNoteIPGroupsByNetwork(t *testing.T) {
	a := NewStatsAggregator(StatsOptions{Enabled: true, RecordIP: true})
	defer a.Close()

	a.NoteIP("team-a/app", "192.168.1.55")
	a.NoteIP("team-a/app", "192.168.1.56")
	a.NoteIP("team-a/app", "192.168.2.56")

	buckets := ipBuckets(a)
	if len(buckets) != 2 {
		t.Fatalf("建了 %d 个桶，期望 2（两个网段）", len(buckets))
	}
	total := int64(0)
	for _, hits := range buckets {
		total += hits
	}
	if total != 3 {
		t.Fatalf("命中总数 = %d，期望 3", total)
	}
	for k, hits := range buckets {
		if k.hash == hashIP("192.168.1.0") && hits != 2 {
			t.Fatalf("192.168.1.0 的命中数 = %d，期望 2", hits)
		}
		if k.hash == hashIP("192.168.2.0") && hits != 1 {
			t.Fatalf("192.168.2.0 的命中数 = %d，期望 1", hits)
		}
	}
}

// TestFlushPersistsIPRows 锁定刷盘把 IP 桶写成累加行。
func TestFlushPersistsIPRows(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repoID := newStatsRepo(t, db)

	a := NewStatsAggregator(StatsOptions{Enabled: true, RecordIP: true, DB: db})
	defer a.Close()

	a.NoteIP("team-a/app", "192.168.1.55")
	a.NoteIP("team-a/app", "192.168.1.56")
	if err := a.Flush(ctx); err != nil {
		t.Fatalf("刷盘: %v", err)
	}
	// 再来一次：命中数要**累加**（刷盘多次不能把先前那次的抹掉）。
	a.NoteIP("team-a/app", "192.168.1.57")
	if err := a.Flush(ctx); err != nil {
		t.Fatalf("二次刷盘: %v", err)
	}

	var rows int
	var hits int64
	var hash string
	if err := db.QueryRow(ctx, `SELECT COUNT(*), hits, ip_hash FROM traffic_stats_ip
		WHERE repository_id = ?`, repoID).Scan(&rows, &hits, &hash); err != nil {
		t.Fatalf("读 IP 表: %v", err)
	}
	if rows != 1 {
		t.Fatalf("写了 %d 行，期望 1", rows)
	}
	if hits != 3 {
		t.Fatalf("hits = %d，期望 3（跨刷盘必须累加）", hits)
	}
	if hash != hashIP("192.168.1.0") {
		t.Fatalf("ip_hash = %q，期望 %q", hash, hashIP("192.168.1.0"))
	}
}

// ───────────────────────── 小时 → 天汇总 ─────────────────────────

// TestRollupOnlyClosedHours 锁定汇总**只处理已关闭的小时**。
//
// ⚠️ 当前小时（以及更晚）还在累加，提前汇总会让它下一轮再被累加一次
//
//	（docs/design/10-traffic-stats.md §3.3 / ADR 10）。
func TestRollupOnlyClosedHours(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repoID := newStatsRepo(t, db)

	nowHour := fixedNow().Unix() - fixedNow().Unix()%3600
	closed := nowHour - 3600
	future := nowHour + 3600

	for _, bucket := range []int64{closed, future} {
		if err := db.UpsertHourlyStats(ctx, store.HourlyStatsRow{
			RepositoryID: repoID, BucketAt: bucket, Pulls: 7, BytesPulled: 70, UpdatedAt: 1,
		}); err != nil {
			t.Fatalf("写入小时桶 %d: %v", bucket, err)
		}
	}

	r := newTestRollup(t, db)
	r.rollupOnce(ctx)

	// 已关闭的小时进了天表。
	date := time.Unix(closed, 0).UTC().Format("2006-01-02")
	var pulls int64
	if err := db.QueryRow(ctx, `SELECT pulls FROM traffic_stats_daily
		WHERE repository_id = ? AND stat_date = ?`, repoID, date).Scan(&pulls); err != nil {
		t.Fatalf("读天桶: %v", err)
	}
	if pulls != 7 {
		t.Fatalf("天表 pulls = %d，期望 7", pulls)
	}

	// 还没关闭的小时**不能**进天表。
	var total int64
	if err := db.QueryRow(ctx, `SELECT COALESCE(SUM(pulls),0) FROM traffic_stats_daily`).Scan(&total); err != nil {
		t.Fatalf("读天表合计: %v", err)
	}
	if total != 7 {
		t.Fatalf("天表合计 = %d，期望 7（未关闭的小时不该被汇总）", total)
	}
}

// TestRollupIsIdempotent 锁定汇总**幂等**：重复跑不会重复计数。
//
// ⚠️ 天桶是累加式 upsert，重复汇总会让天表翻倍
//
//	（docs/design/10-traffic-stats.md §3.3）。
func TestRollupIsIdempotent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repoID := newStatsRepo(t, db)

	closed := fixedNow().Unix() - fixedNow().Unix()%3600 - 3600
	if err := db.UpsertHourlyStats(ctx, store.HourlyStatsRow{
		RepositoryID: repoID, BucketAt: closed, Pulls: 5, BytesPulled: 50, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("写入小时桶: %v", err)
	}

	r := newTestRollup(t, db)
	for i := 0; i < 3; i++ {
		r.rollupOnce(ctx)
	}

	var pulls int64
	if err := db.QueryRow(ctx, `SELECT COALESCE(SUM(pulls),0) FROM traffic_stats_daily`).Scan(&pulls); err != nil {
		t.Fatalf("读天表合计: %v", err)
	}
	if pulls != 5 {
		t.Fatalf("跑 3 次后天表 pulls = %d，期望 5（必须幂等）", pulls)
	}

	// 游标要推到已汇总的桶。
	cursor, err := r.loadCursor(ctx)
	if err != nil {
		t.Fatalf("读游标: %v", err)
	}
	if cursor != closed {
		t.Fatalf("游标 = %d，期望 %d", cursor, closed)
	}
}

// TestRollupUsesUTCDate 锁定日期按 **UTC** 切分。
//
// ⚠️ 按本地日切会让"今天"依赖查询方时区，汇总出现重复或遗漏
//
//	（docs/design/10-traffic-stats.md §3.1 / ADR 11）。
func TestRollupUsesUTCDate(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repoID := newStatsRepo(t, db)

	// 23:00 UTC 的桶 → 日期是 10-29（本地时区可能是 10-30）。
	bucket := time.Date(2026, 10, 29, 23, 0, 0, 0, time.UTC).Unix()
	if err := db.UpsertHourlyStats(ctx, store.HourlyStatsRow{
		RepositoryID: repoID, BucketAt: bucket, Pulls: 2, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("写入小时桶: %v", err)
	}
	// 时钟放在 10-30 00:30 UTC：这个桶已经关闭。
	r := newTestRollupAt(t, db, time.Date(2026, 10, 30, 0, 30, 0, 0, time.UTC))
	r.rollupOnce(ctx)

	var date string
	if err := db.QueryRow(ctx, `SELECT stat_date FROM traffic_stats_daily
		WHERE repository_id = ?`, repoID).Scan(&date); err != nil {
		t.Fatalf("读天桶: %v", err)
	}
	if date != "2026-10-29" {
		t.Fatalf("stat_date = %q，期望 2026-10-29（UTC 切分）", date)
	}
}

// ───────────────────────── 保留期清理 ─────────────────────────

// TestPruneRespectsRetention 锁定保留期清理的边界。
func TestPruneRespectsRetention(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repoID := newStatsRepo(t, db)

	now := fixedNow()
	expired := now.Add(-15 * 24 * time.Hour).Unix()
	fresh := now.Add(-13 * 24 * time.Hour).Unix()
	for _, bucket := range []int64{expired, fresh} {
		if err := db.UpsertHourlyStats(ctx, store.HourlyStatsRow{
			RepositoryID: repoID, BucketAt: bucket, Pulls: 1, UpdatedAt: 1,
		}); err != nil {
			t.Fatalf("写入小时桶 %d: %v", bucket, err)
		}
	}
	// 游标推到最新：说明这些小时都已经汇总进天表了。
	if err := db.UpsertInternalSetting(ctx, rollupCursorKey, fresh, "test"); err != nil {
		t.Fatalf("写游标: %v", err)
	}

	r := newTestRollup(t, db)
	r.pruneOnce(ctx)

	var left int
	if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM traffic_stats_hourly
		WHERE bucket_at = ?`, expired).Scan(&left); err != nil {
		t.Fatalf("读小时桶: %v", err)
	}
	if left != 0 {
		t.Fatal("超过保留期的小时桶没被删掉")
	}
	if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM traffic_stats_hourly
		WHERE bucket_at = ?`, fresh).Scan(&left); err != nil {
		t.Fatalf("读小时桶: %v", err)
	}
	if left != 1 {
		t.Fatal("保留期内的小时桶被删掉了")
	}
}

// TestPruneSkipsUnrolledHours 锁定"没汇总过的小时不删"。
//
// ⚠️ 小时表里的数据还没进天表时删掉它 = 永久丢数据。
func TestPruneSkipsUnrolledHours(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repoID := newStatsRepo(t, db)

	old := fixedNow().Add(-30 * 24 * time.Hour).Unix()
	if err := db.UpsertHourlyStats(ctx, store.HourlyStatsRow{
		RepositoryID: repoID, BucketAt: old, Pulls: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("写入小时桶: %v", err)
	}

	r := newTestRollup(t, db)
	r.pruneOnce(ctx)

	var left int
	if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM traffic_stats_hourly`).Scan(&left); err != nil {
		t.Fatalf("读小时桶: %v", err)
	}
	if left != 1 {
		t.Fatalf("剩 %d 行，期望 1（没汇总过的小时不能删）", left)
	}
}

// ───────────────────────── 设置层的游标接线 ─────────────────────────

// captureHandler 收集 slog 记录（设置层内部问题的唯一可观测手段）。
type captureHandler struct {
	mu   sync.Mutex
	msgs []string
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.msgs = append(h.msgs, r.Level.String()+" "+r.Message)
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

// hasSubstr 判断是否有一条包含子串的记录。
func (h *captureHandler) hasSubstr(substr string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, m := range h.msgs {
		if strings.Contains(m, substr) {
			return true
		}
	}
	return false
}

// TestRollupCursorIsNotAnUnknownSetting 锁定汇总游标不会被设置层当成"未知设置项"。
//
// ⚠️ 游标存在 system_settings 表里，而设置层启动时会把表里的每一行
//
//	都 apply 一遍。没接住的话，它会被当成"已删除的遗留设置项"跳过并告警——
//	每小时一条噪音告警，还是个假的"设置项不存在"信号。
//
// ⚠️ 游标不进配置快照：它由汇总任务直接读 DB，
// 投影进快照只会得到一个"改了没人读"的字段。
func TestRollupCursorIsNotAnUnknownSetting(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := db.UpsertInternalSetting(ctx, rollupCursorKey, int64(1793271600), "rollup"); err != nil {
		t.Fatalf("写游标: %v", err)
	}

	handler := &captureHandler{}
	mgr, err := NewSettingsManager(db, config.Default(), "",
		WithLogger(slog.New(handler)))
	if err != nil {
		t.Fatalf("构造设置管理器: %v", err)
	}
	if handler.hasSubstr("设置项已不存在") {
		t.Fatal("游标被当成了未知设置项（不该有这条告警）")
	}
	view, err := mgr.View(ctx)
	if err != nil {
		t.Fatalf("读取设置视图: %v", err)
	}
	for _, s := range view.Settings {
		if s.Key == rollupCursorKey {
			t.Fatal("游标出现在了设置页视图里")
		}
	}
}

// TestRollupCursorIsNotEditable 锁定游标不能被 PATCH 改。
//
// ⚠️ 人手改游标会让汇总跳过或重复一段数据——
//
//	累加式天表一旦重复累加，量就永远错了，且没法自动纠正。
func TestRollupCursorIsNotEditable(t *testing.T) {
	db := newTestDB(t)
	mgr, err := NewSettingsManager(db, config.Default(), "")
	if err != nil {
		t.Fatalf("构造设置管理器: %v", err)
	}
	version, err := db.GlobalSettingsVersion(context.Background())
	if err != nil {
		t.Fatalf("读全局版本: %v", err)
	}
	if _, err := mgr.Apply(context.Background(), "admin", version,
		map[string]any{rollupCursorKey: 123}); err == nil {
		t.Fatal("只读的游标不该能被修改")
	}
}

// ───────────────────────── 代理链路的 IP 记录 ─────────────────────────

// newStatsRegistry 拼一套带统计的 registry 服务。
func newStatsRegistry(t *testing.T, db *store.DB, up *upstreamRecorder, stats *StatsAggregator) *RegistryService {
	t.Helper()
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	return NewRegistryService(RegistryDeps{
		Upstream:    srv.Client(),
		UpstreamURL: srv.URL,
		Perm: NewPermissionEngine(db, PermissionOptions{
			AnonymousReadEnabled: func() bool { return true },
		}),
		Issuer:  newTestIssuer(t),
		DB:      db,
		Stats:   stats,
		PATs:    NewPersonalTokenService(db, PersonalTokenOptions{}),
		RealmFn: func() string { return "http://registry.example.com/token" },
	})
}

// TestAnonymousPullRecordsIP 覆盖代理链路上的匿名 IP 记录。
//
// ⚠️ 这条锁的是**接线**：没带令牌（claims 为 nil）就是匿名，
//
//	匿名请求的来源 IP 要被截断+哈希后记进桶里
//	（docs/design/10-traffic-stats.md §6）。
func TestAnonymousPullRecordsIP(t *testing.T) {
	db := newTestDB(t)
	up := &upstreamRecorder{status: http.StatusOK, body: `{}`}
	stats := NewStatsAggregator(StatsOptions{Enabled: true, RecordIP: true, DB: db})
	t.Cleanup(func() { stats.Close() })
	svc := newStatsRegistry(t, db, up, stats)
	seedProject(t, db, "team-a", domain.VisibilityPublic)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v2/team-a/app/manifests/v1", nil)
	req.RemoteAddr = "192.168.1.55:54321"
	svc.Forward(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("匿名拉取状态码 = %d，期望 200", rec.Code)
	}
	buckets := ipBuckets(stats)
	if len(buckets) != 1 {
		t.Fatalf("IP 桶数 = %d，期望 1", len(buckets))
	}
	for k, hits := range buckets {
		if k.hash != hashIP("192.168.1.0") {
			t.Fatalf("散列 = %q，期望 %q（/24 截断后的 sha256）", k.hash, hashIP("192.168.1.0"))
		}
		if hits != 1 {
			t.Fatalf("命中数 = %d，期望 1", hits)
		}
	}
}

// TestAnonymousPullWithoutRecordIPRecordsNothing 锁定关闭时代理链路不留痕迹。
//
// ⚠️ stats.record_ip 默认 false：热路径上连哈希都不该算
//
//	（ADR 13：关闭时零开销）。
func TestAnonymousPullWithoutRecordIPRecordsNothing(t *testing.T) {
	db := newTestDB(t)
	up := &upstreamRecorder{status: http.StatusOK, body: `{}`}
	stats := NewStatsAggregator(StatsOptions{Enabled: true, DB: db})
	t.Cleanup(func() { stats.Close() })
	svc := newStatsRegistry(t, db, up, stats)
	seedProject(t, db, "team-a", domain.VisibilityPublic)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v2/team-a/app/manifests/v1", nil)
	req.RemoteAddr = "192.168.1.55:54321"
	svc.Forward(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("匿名拉取状态码 = %d，期望 200", rec.Code)
	}
	if got := ipBuckets(stats); len(got) != 0 {
		t.Fatalf("关闭时建了 %d 个 IP 桶，期望 0", len(got))
	}
}

// TestAuthenticatedPullDoesNotRecordIP 锁定带身份的请求不记 IP。
//
// ⚠️ 已经有身份了还收 IP 是白收集个人数据（设计文档 §6）。
func TestAuthenticatedPullDoesNotRecordIP(t *testing.T) {
	db := newTestDB(t)
	up := &upstreamRecorder{status: http.StatusOK, body: `{}`}
	stats := NewStatsAggregator(StatsOptions{Enabled: true, RecordIP: true, DB: db})
	t.Cleanup(func() { stats.Close() })
	svc := newStatsRegistry(t, db, up, stats)
	seedProject(t, db, "team-a", domain.VisibilityPublic)

	token, err := svc.issuer.Issue("alice", []domain.Access{{
		Type: "repository", Name: "team-a/app", Actions: []string{"pull"},
	}})
	if err != nil {
		t.Fatalf("签发 token: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v2/team-a/app/manifests/v1", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.RemoteAddr = "192.168.1.55:54321"
	svc.Forward(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("带令牌拉取状态码 = %d，期望 200", rec.Code)
	}
	if got := ipBuckets(stats); len(got) != 0 {
		t.Fatalf("带身份的请求建了 %d 个 IP 桶，期望 0", len(got))
	}
}

// TestRecordIPFlagRefreshesOnFlush 锁定设置页改开关后能热生效。
func TestRecordIPFlagRefreshesOnFlush(t *testing.T) {
	db := newTestDB(t)
	enabled := false
	a := NewStatsAggregator(StatsOptions{
		Enabled:    true,
		DB:         db,
		RecordIPFn: func() bool { return enabled },
	})
	t.Cleanup(func() { a.Close() })

	a.NoteIP("team-a/app", "192.168.1.55")
	if got := ipBuckets(a); len(got) != 0 {
		t.Fatalf("开关还关着却建了 %d 个桶", len(got))
	}
	enabled = true
	if err := a.Flush(context.Background()); err != nil {
		t.Fatalf("刷盘: %v", err)
	}
	if !a.RecordIPEnabled() {
		t.Fatal("刷盘后开关没被刷新")
	}
	a.NoteIP("team-a/app", "192.168.1.55")
	if got := ipBuckets(a); len(got) != 1 {
		t.Fatalf("开关打开后建了 %d 个桶，期望 1", len(got))
	}
}

// TestRunStopsOnContextCancel 锁定 Run 是阻塞循环，ctx 结束就返回。
func TestRunStopsOnContextCancel(t *testing.T) {
	db := newTestDB(t)
	r := newTestRollup(t, db)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("ctx 取消后 Run 没返回")
	}
}

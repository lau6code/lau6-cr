package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lau6code/lau6-cr/apps/api/internal/store"
)

// counterKey 是聚合桶的键。
type counterKey struct {
	repo   string
	bucket int64 // 小时桶起点（unix 秒）
}

type counterBucket struct {
	pulls    atomic.Int64
	pushes   atomic.Int64
	deletes  atomic.Int64
	bytesIn  atomic.Int64
	bytesOut atomic.Int64
}

// ipKey 是匿名 IP 命中桶的键。
//
// ⚠️ hash 存的是**截断+哈希**后的值，不是明文地址
//
//	（docs/design/10-traffic-stats.md §6 / ADR 14）。
type ipKey struct {
	repo   string
	bucket int64 // 小时桶起点（unix 秒）
	hash   string
}

// ipBucket 统计某个 (仓库, 小时, 网段) 被命中了多少次。
type ipBucket struct {
	hits atomic.Int64
}

// StatsAggregator 聚合流量计数并定期落库。
//
// 热路径只做 atomic 累加；刷盘在锁外做数据库 I/O。
type StatsAggregator struct {
	db      *store.DB
	enabled atomic.Bool
	// interval 是刷盘周期（纳秒）。
	//
	// ⚠️ 用 atomic 存而不是普通字段：stats.flush_interval 是 T0 热生效设置，
	//    启动时冻住的话，设置页能改、能显示"已保存"，运行期却还是旧周期——
	//    而它恰恰是"崩溃时的统计丢失窗口"。
	interval atomic.Int64
	// maxBucket 是内存桶数上限。同上，stats.max_buckets 是 T0 热生效设置。
	maxBucket atomic.Int64

	// recordIP 控制是否记录匿名客户端 IP（截断+哈希）。
	//
	// ⚠️ 默认关闭（stats.record_ip=false）：关闭时热路径**连哈希都不算**，
	//    只多一次 atomic.Bool 加载。
	recordIP atomic.Bool
	// recordIPFn 可选：每轮刷盘时刷新 recordIP，
	// 让设置页改开关后最多一个刷盘周期生效（不必重启）。
	recordIPFn func() bool

	mu      sync.RWMutex
	buckets map[counterKey]*counterBucket
	// ipBuckets 是匿名 IP 的命中桶，与 buckets 同一把锁。
	ipBuckets map[ipKey]*ipBucket

	// logger 记录刷盘失败。
	//
	// ⚠️ 不能省：刷盘失败若只 `_ = a.Flush(...)` 吞掉，
	// 就是"统计一直在丢数据但没人知道"——排障时完全看不出来。
	logger Logger

	// restoredBuckets 累计因落库失败而放回待重试的桶数。
	//
	// ⚠️ 可观测性：失败日志必须能回答"丢了多少"，
	//    否则只有一句"刷盘失败"，运维无从判断影响面。
	restoredBuckets atomic.Int64
	// restoredRepoList 记录最近一次失败时放回待重试的仓库名。
	//
	// 与 buckets 共用 mu 保护。
	restoredRepoList []string

	stopCh chan struct{}
	once   sync.Once
	// retuneCh 通知刷盘循环"周期变了，按新周期重新计时"。
	//
	// 容量 1：重复通知会合并成一次（循环醒来时读到的永远是最新值）。
	retuneCh chan struct{}
}

// StatsOptions 是统计聚合器的配置。
type StatsOptions struct {
	DB      *store.DB
	Enabled bool
	// Interval 是刷盘周期的**初始值**；运行期用 SetInterval 调整。
	Interval time.Duration
	// MaxBucket 是内存桶数上限的**初始值**；运行期用 SetMaxBuckets 调整。
	MaxBucket int
	// RecordIP 是"是否记录匿名 IP"的初始值。
	RecordIP bool
	// RecordIPFn 可选：每轮刷盘时用它刷新 RecordIP。
	//
	// 有了它，设置页改 stats.record_ip 后最多一个刷盘周期生效，
	// 而热路径上仍然只是一次 atomic.Bool 加载。
	RecordIPFn func() bool
	// Logger 用于记录刷盘失败；可为 nil（此时失败仍会返回给调用方）。
	Logger Logger
}

// NewStatsAggregator 构造聚合器并启动刷盘协程。
func NewStatsAggregator(o StatsOptions) *StatsAggregator {
	if o.Interval <= 0 {
		o.Interval = 60 * time.Second
	}
	if o.MaxBucket <= 0 {
		o.MaxBucket = 10000
	}
	a := &StatsAggregator{
		db:         o.DB,
		buckets:    map[counterKey]*counterBucket{},
		ipBuckets:  map[ipKey]*ipBucket{},
		stopCh:     make(chan struct{}),
		retuneCh:   make(chan struct{}, 1),
		logger:     o.Logger,
		recordIPFn: o.RecordIPFn,
	}
	a.interval.Store(int64(o.Interval))
	a.maxBucket.Store(int64(o.MaxBucket))
	a.enabled.Store(o.Enabled)
	a.recordIP.Store(o.RecordIP)
	go a.loop()
	return a
}

func (a *StatsAggregator) SetEnabled(v bool) { a.enabled.Store(v) }

// SetInterval 动态调整刷盘周期。
//
// ⚠️ 立即生效：新周期从**此刻**开始计时，不是等旧周期跑完。
//
// 实现方式是给刷盘循环发一个"重新计时"的信号：
// 只改 atomic 的话，正在等待的那个定时器仍按旧周期走，
// 改完要等一整个旧周期才生效——那不是热生效。
func (a *StatsAggregator) SetInterval(d time.Duration) {
	if d <= 0 {
		return
	}
	a.interval.Store(int64(d))
	select {
	case a.retuneCh <- struct{}{}:
	default:
		// 已经有一次待处理的重新计时：合并成一次即可
		//（循环醒来时读到的永远是最新值）。
	}
}

func (a *StatsAggregator) Interval() time.Duration { return time.Duration(a.interval.Load()) }

func (a *StatsAggregator) SetMaxBuckets(n int) {
	if n <= 0 {
		return
	}
	a.maxBucket.Store(int64(n))
}

func (a *StatsAggregator) maxBuckets() int { return int(a.maxBucket.Load()) }

// SetRecordIP 动态开关匿名 IP 记录。
func (a *StatsAggregator) SetRecordIP(v bool) { a.recordIP.Store(v) }

// RecordIPEnabled 返回"是否记录匿名 IP"。
//
// 热路径只多这一次 atomic 加载（纳秒级）。
func (a *StatsAggregator) RecordIPEnabled() bool { return a.recordIP.Load() }

// Note 记录一次请求（拉/推/删计数）。
//
// ⚠️ 口径严格照 `docs/design/10-traffic-stats.md` §1.3 的采集点对照表：
//
//   - HEAD 一律不计——守卫的内部探测与客户端探活都是 HEAD，计进去数字全失真。
//   - manifest 的 PUT 计 pushes、DELETE 计 deletes、GET 计 pulls。
//   - 上传会话（/blobs/uploads/）的 POST 只建会话不计；PATCH/PUT 的字节走 AddIn。
//   - tags/list 与 _catalog 不计（管理类，非镜像数据流）。
//
// 调用方**只在请求成功时**调它（守卫拒绝的请求根本没发生，计进去
// 会让"推送次数"和仓库实际内容对不上）。
func (a *StatsAggregator) Note(rp RegistryPath, method string) {
	if !a.enabled.Load() || rp.Repo == "" {
		return
	}
	// HEAD 不计。
	if method == http.MethodHead {
		return
	}
	b := a.bucket(rp.Repo)
	switch rp.Kind {
	case "manifests":
		switch method {
		case http.MethodGet:
			b.pulls.Add(1)
		case http.MethodPut:
			b.pushes.Add(1)
		case http.MethodDelete:
			b.deletes.Add(1)
		}
	case "blob":
		if method == http.MethodGet {
			b.pulls.Add(1)
		}
	case "upload":
		// POST 只建立上传会话，没有数据；PUT 是分块的最后一击，算一次推送。
		if method == http.MethodPut {
			b.pushes.Add(1)
		}
	}
}

func (a *StatsAggregator) AddOut(repo string, n int64) {
	if !a.enabled.Load() || repo == "" || n <= 0 {
		return
	}
	a.bucket(repo).bytesOut.Add(n)
}

func (a *StatsAggregator) AddIn(repo string, n int64) {
	if !a.enabled.Load() || repo == "" || n <= 0 {
		return
	}
	a.bucket(repo).bytesIn.Add(n)
}

// NoteIP 记一次匿名请求的来源 IP。
//
// ⚠️ 记的是**截断+哈希**后的值：IPv4 截到 /24、IPv6 截到 /48，再取 sha256。
//
//	库里没有明文 IP，定位不到具体个人，但还能区分"同一个网段"
//	（docs/design/10-traffic-stats.md §6 / ADR 14）。
//
// ⚠️ 关闭时（stats.record_ip 默认 false）**什么都不做**：
//
//	热路径上连截断与哈希都不算，只多一次 atomic.Bool 加载。
//
// ⚠️ 只记匿名请求：带令牌的用户请求已经有身份，
//
//	再记 IP 是白收集个人数据（设计文档 §6）。
func (a *StatsAggregator) NoteIP(repo, ip string) {
	if !a.enabled.Load() || !a.recordIP.Load() || repo == "" {
		return
	}
	hash := hashTruncatedIP(ip)
	if hash == "" {
		// 地址解析不了（不是合法 IP）：不记。
		// ⚠️ 不能把原始字符串塞进去——那等于存明文。
		return
	}
	key := ipKey{repo: repo, bucket: currentHour(), hash: hash}
	a.mu.Lock()
	defer a.mu.Unlock()
	if b, ok := a.ipBuckets[key]; ok {
		b.hits.Add(1)
		return
	}
	if len(a.ipBuckets) >= a.maxBuckets() {
		a.evictOldestIPLocked()
	}
	b := &ipBucket{}
	b.hits.Store(1)
	a.ipBuckets[key] = b
}

// evictOldestIPLocked 淘汰最旧的 IP 桶。
//
// ⚠️ 与小时桶同款取舍：桶数有上限，超了就丢最旧的整点桶
//
//	（设计文档 §9「内存压力」）。IP 桶的基数由网段数决定，
//	遇到扫全网段的攻击会暴涨——没有上限就是 unbounded。
func (a *StatsAggregator) evictOldestIPLocked() {
	var oldest ipKey
	first := true
	for k := range a.ipBuckets {
		if first || k.bucket < oldest.bucket {
			oldest, first = k, false
		}
	}
	if !first {
		delete(a.ipBuckets, oldest)
	}
}

// truncateIP 把客户端地址截断到网段：IPv4 → /24，IPv6 → /48。
//
// 为什么截断：设计文档要求库里**不出现**能定位到个人的地址，
// 但又要能区分"同一个网段"（限流联动、滥用溯源）。
//
// 解析不了返回空串（调用方据此不记）——不能把原始字符串当地址用。
func truncateIP(ip string) string {
	parsed := net.ParseIP(strings.TrimSpace(ip))
	if parsed == nil {
		return ""
	}
	// IPv4（含 ::ffff:a.b.c.d 这种 IPv4 映射形式）走 /24。
	if v4 := parsed.To4(); v4 != nil {
		return v4.Mask(net.CIDRMask(24, 32)).String()
	}
	return parsed.Mask(net.CIDRMask(48, 128)).String()
}

// hashIP 返回 sha256 的十六进制散列。
func hashIP(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// hashTruncatedIP 截断 + 哈希。地址不合法时返回空串（不记）。
func hashTruncatedIP(ip string) string {
	t := truncateIP(ip)
	if t == "" {
		return ""
	}
	return hashIP(t)
}

// bucket 取（并按需创建）某仓库当前小时的计数桶。
func (a *StatsAggregator) bucket(repo string) *counterBucket {
	key := counterKey{repo: repo, bucket: currentHour()}
	a.mu.RLock()
	b := a.buckets[key]
	a.mu.RUnlock()
	if b != nil {
		return b
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if b = a.buckets[key]; b != nil {
		return b
	}
	if len(a.buckets) >= a.maxBuckets() {
		a.evictOldestLocked()
	}
	b = &counterBucket{}
	a.buckets[key] = b
	return b
}

func (a *StatsAggregator) evictOldestLocked() {
	var oldest counterKey
	first := true
	for k := range a.buckets {
		if first || k.bucket < oldest.bucket {
			oldest, first = k, false
		}
	}
	if !first {
		delete(a.buckets, oldest)
	}
}

func (a *StatsAggregator) loop() {
	for {
		// 每轮按**当前**周期计时：周期是热生效设置，
		// 用固定 ticker 的话改完得等旧周期跑完才生效。
		timer := time.NewTimer(a.Interval())
		select {
		case <-timer.C:
			a.flushAndLog()
		case <-a.retuneCh:
			// 周期变了：按新周期重新计时。
			// 不在这里补一次刷盘——上一轮已经落库了，
			// 因为改了设置就多刷一次没有意义。
			timer.Stop()
			continue
		case <-a.stopCh:
			timer.Stop()
			a.flushAndLog()
			return
		}
	}
}

// flushAndLog 刷盘并把失败记下来。
//
// ⚠️ 失败**不能吞**：吞掉就是"统计一直在丢数据、页面上全是 0、
// 而后端一片安静"。这里记 error 级日志——统计是可牺牲的，
// 但"丢了且没人知道"是不可接受的。
//
// ⚠️ 日志要带**丢了多少、丢了哪些仓库**：
//
//	一条泛化的 "err" 只能说明"有一件事失败了"，
//	运维看不出影响面有多大、该去查哪个仓库。
func (a *StatsAggregator) flushAndLog() {
	if err := a.Flush(context.Background()); err != nil && a.logger != nil {
		a.logger.Error("流量统计刷盘失败", "err", err,
			"restored_buckets", a.restoredBuckets.Load(),
			"restored_repos", a.restoredRepos())
	}
}

// Close 停止聚合器并做最后一次刷盘。
func (a *StatsAggregator) Close() error {
	a.once.Do(func() { close(a.stopCh) })
	return nil
}

// statRow 是一条待落库的仓库统计。
type statRow struct {
	key counterKey
	b   *counterBucket
	row store.HourlyStatsRow
}

// ipStatRow 是一条待落库的 IP 命中统计。
type ipStatRow struct {
	key ipKey
	b   *ipBucket
	row store.TrafficIPRow
}

// Flush 把内存里的计数落库。
//
// **换 map + 锁外 I/O**：持锁做数据库往返会让所有镜像请求排队。
//
// ⚠️ 失败时**本轮没落库的桶全部放回去**（下轮重试）：
//
//	只放回触发错误的那一个，其余仓库的计数就此丢失——
//	那是"执行到一半失败、最终却看起来成功"的静默数据丢失
//	（AGENTS.md 规则 3）。
func (a *StatsAggregator) Flush(ctx context.Context) error {
	if a.db == nil {
		return nil
	}
	// 刷盘前刷新 record_ip 开关：设置页改了之后最多一个周期生效。
	a.refreshRecordIP()

	a.mu.Lock()
	snapshot := a.buckets
	a.buckets = map[counterKey]*counterBucket{}
	ipSnapshot := a.ipBuckets
	a.ipBuckets = map[ipKey]*ipBucket{}
	a.mu.Unlock()

	now := store.Now()
	// 仓库名 → ID 的当轮缓存：
	// IP 桶按 (仓库, 小时, 网段) 展开，不缓存的话同一仓库要反复查库。
	repoIDs := map[string]int64{}
	repoID := func(name string) (int64, error) {
		if id, ok := repoIDs[name]; ok {
			return id, nil
		}
		id, err := a.repoID(ctx, name)
		if err != nil {
			return 0, err
		}
		repoIDs[name] = id
		return id, nil
	}

	// ① 解析仓库 ID，组装待落库的行
	//
	// 先组装再落库：落库失败时才知道"还剩哪些没写"，
	// 才能把它们**全部**放回去（而不是只放回失败的那一个）。
	//
	// settled 标记"已经落库或明确丢弃"的桶——
	// 出错时只把没 settled 的放回去，一桶都不多、一桶都不少。
	settled := map[counterKey]bool{}
	rows := make([]statRow, 0, len(snapshot))
	for key, b := range snapshot {
		pulls := b.pulls.Load()
		pushes := b.pushes.Load()
		deletes := b.deletes.Load()
		bytesIn := b.bytesIn.Load()
		bytesOut := b.bytesOut.Load()
		if pulls == 0 && pushes == 0 && deletes == 0 && bytesIn == 0 && bytesOut == 0 {
			settled[key] = true // 没有计数可落
			continue
		}
		repo, err := repoID(key.repo)
		if err != nil {
			// 连仓库都查不到 → 本轮无法落库。
			// 本轮**还没落库的桶全部**放回去（含还没处理到的）。
			restored := a.restoreUnsettled(snapshot, settled)
			a.restoreAllIP(ipSnapshot)
			a.noteRestored(restored)
			return err
		}
		if repo == 0 {
			settled[key] = true // 仓库已删除，丢弃
			continue
		}
		rows = append(rows, statRow{
			key: key,
			b:   b,
			row: store.HourlyStatsRow{
				RepositoryID: repo,
				BucketAt:     key.bucket,
				Pulls:        pulls,
				Pushes:       pushes,
				Deletes:      deletes,
				BytesPulled:  bytesOut,
				BytesPushed:  bytesIn,
				UpdatedAt:    now,
			},
		})
	}

	// ② 落库
	for i, r := range rows {
		if err := a.db.UpsertHourlyStats(ctx, r.row); err != nil {
			// 从失败的这一条开始，**剩下的全部**放回去下轮重试。
			//
			// ⚠️ 失败的那一条也必须放回去——它**没有**落库，
			//    标成"已处理"就等于把它的计数丢了。
			//    ① 阶段已经把快照里的每个键处理干净了
			//    （要么进了 rows，要么进了 settled），
			//    所以这里要恢复的就是 rows[i:]。
			restored := a.restoreStatRows(rows[i:])
			a.restoreAllIP(ipSnapshot)
			a.noteRestored(restored)
			return err
		}
	}

	// ③ IP 桶
	ipSettled := map[ipKey]bool{}
	ipRows := make([]ipStatRow, 0, len(ipSnapshot))
	for key, b := range ipSnapshot {
		hits := b.hits.Load()
		if hits == 0 {
			ipSettled[key] = true
			continue
		}
		repo, err := repoID(key.repo)
		if err != nil {
			restored := a.restoreUnsettledIP(ipSnapshot, ipSettled)
			a.noteRestoredIP(restored)
			return err
		}
		if repo == 0 {
			ipSettled[key] = true // 仓库已删除，丢弃
			continue
		}
		ipRows = append(ipRows, ipStatRow{
			key: key,
			b:   b,
			row: store.TrafficIPRow{
				RepositoryID: repo,
				BucketAt:     key.bucket,
				IPHash:       key.hash,
				Hits:         hits,
				UpdatedAt:    now,
			},
		})
	}
	for i, r := range ipRows {
		if err := a.db.UpsertTrafficIP(ctx, r.row); err != nil {
			// 同上：失败的那一条也没落库，必须一起放回去。
			restored := a.restoreIPRows(ipRows[i:])
			a.noteRestoredIP(restored)
			return err
		}
	}
	return nil
}

// restoreStatRows 把这些仓库统计桶放回去，下轮重试，返回放回的桶。
func (a *StatsAggregator) restoreStatRows(rows []statRow) []statRow {
	return restoreRows(rows, func(r statRow) { a.restore(r.key, r.b) })
}

// restoreIPRows 把这些 IP 桶放回去，下轮重试，返回放回的桶。
func (a *StatsAggregator) restoreIPRows(rows []ipStatRow) []ipStatRow {
	return restoreRows(rows, func(r ipStatRow) { a.restoreIP(r.key, r.b) })
}

// restoreRows 把一批桶放回聚合器，下轮重试，返回放回的桶。
func restoreRows[R any](rows []R, restore func(R)) []R {
	out := make([]R, 0, len(rows))
	for _, r := range rows {
		restore(r)
		out = append(out, r)
	}
	return out
}

// restoreUnsettled 把本轮**还没落库**的仓库桶全部放回去，返回放回的那些。
//
// settled 里的桶已经落库（或明确丢弃），放回去会重复计数。
func (a *StatsAggregator) restoreUnsettled(snapshot map[counterKey]*counterBucket, settled map[counterKey]bool) []statRow {
	return restoreUnsettledBuckets(snapshot, settled,
		func(key counterKey, b *counterBucket) statRow { return statRow{key: key, b: b} },
		a.restore)
}

// restoreUnsettledIP 把本轮**还没落库**的 IP 桶全部放回去。
func (a *StatsAggregator) restoreUnsettledIP(snapshot map[ipKey]*ipBucket, settled map[ipKey]bool) []ipStatRow {
	return restoreUnsettledBuckets(snapshot, settled,
		func(key ipKey, b *ipBucket) ipStatRow { return ipStatRow{key: key, b: b} },
		a.restoreIP)
}

// restoreUnsettledBuckets 把本轮**还没落库**的桶全部放回去，返回放回的那些。
//
// settled 里的桶已经落库（或明确丢弃），放回去会重复计数。
func restoreUnsettledBuckets[K comparable, B any, R any](
	snapshot map[K]B,
	settled map[K]bool,
	row func(K, B) R,
	restore func(K, B),
) []R {
	out := make([]R, 0, len(snapshot))
	for key, b := range snapshot {
		if settled[key] {
			continue
		}
		out = append(out, row(key, b))
		restore(key, b)
	}
	return out
}

// restoreAllIP 把本轮全部 IP 桶放回去（仓库桶落库失败时，IP 桶同样还没写）。
func (a *StatsAggregator) restoreAllIP(snapshot map[ipKey]*ipBucket) {
	for key, b := range snapshot {
		a.restoreIP(key, b)
	}
}

// noteRestored 记录本轮放回多少个仓库桶、涉及哪些仓库。
//
// ⚠️ 可观测性：失败日志必须能回答"丢了多少、丢了哪些仓库"，
//
//	否则运维只能看到一句"刷盘失败"。
func (a *StatsAggregator) noteRestored(rows []statRow) {
	if len(rows) == 0 {
		return
	}
	a.restoredBuckets.Add(int64(len(rows)))
	repos := make([]string, 0, len(rows))
	for _, r := range rows {
		repos = append(repos, r.key.repo)
	}
	a.mu.Lock()
	a.restoredRepoList = appendUnique(a.restoredRepoList, repos)
	a.mu.Unlock()
}

// noteRestoredIP 只记桶数、不记仓库名：IP 桶是 (仓库, 小时, 网段) 三元组，
// 同一仓库可能有多个网段桶，把它们的仓库名混进 restoredRepoList 会让
// "丢了哪些仓库"失真。仓库维度的影响面由仓库桶那条路径记录。
func (a *StatsAggregator) noteRestoredIP(rows []ipStatRow) {
	if len(rows) == 0 {
		return
	}
	a.restoredBuckets.Add(int64(len(rows)))
}

func appendUnique(dst []string, add []string) []string {
	seen := make(map[string]bool, len(dst))
	for _, s := range dst {
		seen[s] = true
	}
	for _, s := range add {
		if !seen[s] {
			seen[s] = true
			dst = append(dst, s)
		}
	}
	return dst
}

// restoredRepos 返回最近一次失败时放回的仓库名。
func (a *StatsAggregator) restoredRepos() []string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]string, len(a.restoredRepoList))
	copy(out, a.restoredRepoList)
	return out
}

// refreshRecordIP 按设置刷新"是否记录匿名 IP"。
//
// 每轮刷盘刷新一次即可：热路径上不做函数调用（那不是"一次 atomic 加载"）。
func (a *StatsAggregator) refreshRecordIP() {
	if a.recordIPFn == nil {
		return
	}
	a.recordIP.Store(a.recordIPFn())
}

func (a *StatsAggregator) restore(key counterKey, b *counterBucket) {
	a.mu.Lock()
	defer a.mu.Unlock()
	restoreBucket(a.buckets, key, b, a.maxBuckets(), a.evictOldestLocked)
}

// restoreIP 把 IP 桶放回去，下轮重试。
func (a *StatsAggregator) restoreIP(key ipKey, b *ipBucket) {
	a.mu.Lock()
	defer a.mu.Unlock()
	restoreBucket(a.ipBuckets, key, b, a.maxBuckets(), a.evictOldestIPLocked)
}

// restoreBucket 把一个桶放回聚合器的桶表。
//
// 桶数到上限时先淘汰最旧的桶——仓库桶与 IP 桶的淘汰条件相同，
// 但桶表不同（各自的 map 与各自的淘汰函数），所以由调用方传入。
//
// ⚠️ 调用方必须持有 a.mu。
func restoreBucket[K comparable, B any](buckets map[K]B, key K, b B, max int, evict func()) {
	if len(buckets) >= max {
		evict()
	}
	buckets[key] = b
}

func (a *StatsAggregator) repoID(ctx context.Context, full string) (int64, error) {
	ref, ok := ParseRepoName(full)
	if !ok {
		return 0, nil
	}
	project, err := a.db.ProjectByName(ctx, ref.ProjectName)
	if err == store.ErrNotFound {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	repo, err := a.db.RepositoryByPath(ctx, project.ID, ref.RepoPath)
	if err == store.ErrNotFound {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return repo.ID, nil
}

func currentHour() int64 {
	now := time.Now().Unix()
	return now - now%3600
}

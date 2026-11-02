package domain

// 保留策略与清理执行的领域模型。
//
// 设计依据见 docs/design/03-registry.md §5。

// 保留策略的规则模板。
//
// 与 Harbor 的模板对照见 03-registry.md §5.1。
const (
	// RuleKeepLatestK 保留最近推送的 K 个 tag（对应 Harbor 的 latestPushedK）。
	RuleKeepLatestK = "keep_latest_k"
	// RuleKeepWithinDays 保留最近 N 天内推送过的 tag（对应 Harbor 的 nDaysSinceLastPush）。
	RuleKeepWithinDays = "keep_within_days"
	// RuleKeepAlways 永不自动删除（对应 Harbor 的 always）。
	RuleKeepAlways = "keep_always"
)

// 清理的触发方式。
const (
	TriggerSchedule = "schedule"
	TriggerManual   = "manual"
	TriggerPush     = "push"
)

// 清理执行的状态。
//
// ⚠️ 只有三态，**没有**"部分成功"：
// 只要有任何一个删除失败，整次执行就是 failed（03-registry.md §5.7）。
const (
	CleanupStatusRunning = "running"
	CleanupStatusSuccess = "success"
	CleanupStatusFailed  = "failed"
)

// RetentionRule 是保留策略里的一条规则。
//
// 规则之间是 OR 关系：命中任意一条即保留。
type RetentionRule struct {
	// Template 是规则模板，见 RuleKeepLatestK 等常量。
	Template string `json:"template"`
	// Params 是模板参数：
	//
	//	keep_latest_k   → {"k": 5}
	//	keep_within_days → {"days": 7}
	//	keep_always     → {}
	Params map[string]int `json:"params"`
	// TagSelector 是该规则的 tag 选择器（glob）。
	//
	// 空串表示用策略级的 TagSelector。
	TagSelector string `json:"tag_selector,omitempty"`
}

// RetentionPolicy 是一条保留策略。
//
// 作用域由 ScopeKey 决定：空串 = 全局默认，"p:<project_id>" = 项目级。
type RetentionPolicy struct {
	ID        int64  `json:"id"`
	ScopeKey  string `json:"scope_key"`
	ProjectID *int64 `json:"project_id,omitempty"`
	// ProjectName 是项目级策略的项目名（全局默认为空）。
	ProjectName string `json:"project_name,omitempty"`
	// Enabled 是启用开关。默认关闭——清理是破坏性操作，必须显式开启。
	Enabled bool `json:"enabled"`
	// Algorithm 是组合算法，只支持 "or"。
	Algorithm string `json:"algorithm"`
	// Rules 是规则列表。
	Rules []RetentionRule `json:"rules"`
	// TagSelector 是策略级默认选择器（规则没写自己的选择器时用它）。
	TagSelector string `json:"tag_selector"`
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
}

// RetentionTag 是 tag 级快照。
//
// 上游 v2 API 查不到推送时间，所以 lau6-cr 自己维护这份快照
// （03-registry.md §5.3）。
type RetentionTag struct {
	RepositoryID int64  `json:"-"`
	Tag          string `json:"tag"`
	Digest       string `json:"digest"`
	Size         int64  `json:"size"`
	// PushedAt 是推送时间。**nil = 不知道**。
	//
	// 不知道 ≠ 很老。策略遇到 nil 一律保留（§5.2.4 ②）。
	PushedAt *int64 `json:"pushed_at,omitempty"`
	// PulledAt 是最近拉取时间。nil = 不知道（当前未按拉取时间做决策）。
	PulledAt *int64 `json:"pulled_at,omitempty"`
}

// CleanupRun 是一次清理执行记录。
type CleanupRun struct {
	ID          int64  `json:"id"`
	Trigger     string `json:"trigger"`
	Scope       string `json:"scope"`
	ProjectName string `json:"project_name,omitempty"`
	DryRun      bool   `json:"dry_run"`
	Status      string `json:"status"`
	// TagsDeleted 是删掉的 tag 数（一个 digest 可能对应多个 tag）。
	TagsDeleted int `json:"tags_deleted"`
	// ManifestsDeleted 是删掉的 manifest 数（按 digest 计）。
	ManifestsDeleted int `json:"manifests_deleted"`
	// TagsKept 是保留下来的 tag 数。
	TagsKept int `json:"tags_kept"`
	// SkippedUnknownTime 是"推送时间未知而保留"的 tag 数。
	SkippedUnknownTime int `json:"skipped_unknown_time"`
	// SkippedSharedDigest 是"同 digest 下有别的 tag 要保留，整组跳过"的 tag 数。
	SkippedSharedDigest int `json:"skipped_shared_digest"`
	// FailedCount 是删除失败的条目数。
	FailedCount int `json:"failed_count"`
	// Error 是整体错误原文；单条失败在 Detail 里。
	Error  string `json:"error,omitempty"`
	Detail string `json:"detail,omitempty"`
	// StartedAt / FinishedAt 是起止时间（unix 秒）。
	StartedAt  int64  `json:"started_at"`
	FinishedAt *int64 `json:"finished_at,omitempty"`
}

// CleanupFailure 是一次删除失败的具体记录。
type CleanupFailure struct {
	Repo   string `json:"repo"`
	Tag    string `json:"tag"`
	Digest string `json:"digest"`
	Error  string `json:"error"`
}

// CleanupDetail 是执行明细（序列化进 cleanup_runs.detail）。
type CleanupDetail struct {
	Failures []CleanupFailure `json:"failures,omitempty"`
	// AuditFailures 记**审计写失败**的条目。
	//
	// tag 已经删掉了（删不掉的东西回滚不了），但审计链上没留下痕迹——
	// 这类条目单列一栏，执行状态也会因此落成 failed，
	// 不能跟"删干净了"长得一模一样。
	AuditFailures []CleanupFailure `json:"audit_failures,omitempty"`
	// Deleted 列出被删的 manifest。
	Deleted []CleanupDeleted `json:"deleted,omitempty"`
	// Skipped 列出被跳过的条目及原因。
	Skipped []CleanupSkipped `json:"skipped,omitempty"`
	// GC 是本次清理附带的 blob 回收结果。
	//
	// 为 nil 表示没跑回收（未启用，或这是一次试运行）。
	GC *CleanupGC `json:"gc,omitempty"`
}

// CleanupGC 是清理执行里附带的 blob 回收结果。
type CleanupGC struct {
	// RunID 是 gc_runs 里的执行记录 ID，可以据此查详情。
	RunID  int64  `json:"run_id"`
	Status string `json:"status"`
	DryRun bool   `json:"dry_run"`
	// BlobsDeleted 是回收掉的 blob 数。
	BlobsDeleted int `json:"blobs_deleted"`
	// BytesReclaimed 是回收的字节数。
	BytesReclaimed int64 `json:"bytes_reclaimed"`
	// Error 是失败原因（成功时为空）。
	Error string `json:"error,omitempty"`
}

// CleanupDeleted 是一条删除记录。
type CleanupDeleted struct {
	Repo   string   `json:"repo"`
	Tags   []string `json:"tags"`
	Digest string   `json:"digest"`
	Size   int64    `json:"size"`
}

// CleanupSkipped 是一条跳过记录。
type CleanupSkipped struct {
	Repo   string `json:"repo"`
	Tag    string `json:"tag"`
	Digest string `json:"digest,omitempty"`
	// Reason 是跳过原因：unknown_time / shared_digest / protected_latest / no_digest。
	Reason string `json:"reason"`
}

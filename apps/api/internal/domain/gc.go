package domain

// blob 回收（GC）的领域模型。
//
// 设计依据见 docs/design/03-registry.md §5.5。

// GC 的触发方式。
const (
	// GCTriggerManual 是管理员在界面上手动触发。
	GCTriggerManual = "manual"
	// GCTriggerAfterCleanup 是保留策略执行完后自动触发。
	GCTriggerAfterCleanup = "after_cleanup"
	// GCTriggerSchedule 是每日定时触发（cleanup.gc.schedule_at 指定时间点）。
	GCTriggerSchedule = "schedule"
)

// GC 执行的状态。与清理执行一样，只有三态——
// 没有"部分成功"，有任何一个失败就是 failed。
const (
	GCStatusRunning = "running"
	GCStatusSuccess = "success"
	GCStatusFailed  = "failed"
)

// GCRun 是一次 blob 回收的执行记录。
type GCRun struct {
	ID int64 `json:"id"`
	// Trigger 是触发方式：manual / after_cleanup。
	Trigger string `json:"trigger"`
	// DryRun 为 true 时只评估不删除。
	DryRun bool `json:"dry_run"`
	// Status 是执行状态：running / success / failed。
	Status string `json:"status"`
	// StorageRoot 是本次回收针对的存储标识。
	//
	// 本地存储 → 存储根目录（如 /data/registry）；
	// 对象存储 → s3://<bucket>。
	StorageRoot string `json:"storage_root"`
	// Repositories 是扫到的仓库数。
	Repositories int `json:"repositories"`
	// ManifestsFound 是扫到的 manifest 修订数。
	ManifestsFound int `json:"manifests_found"`
	// BlobsMarked 是被引用的 blob 数（标记集合大小）。
	BlobsMarked int `json:"blobs_marked"`
	// BlobsScanned 是扫到的 blob 总数。
	BlobsScanned int `json:"blobs_scanned"`
	// BlobsDeleted 是删掉的 blob 数。
	BlobsDeleted int `json:"blobs_deleted"`
	// BytesReclaimed 是回收的字节数。
	BytesReclaimed int64 `json:"bytes_reclaimed"`
	// LayerLinksDeleted 是删掉的层链接数。
	LayerLinksDeleted int `json:"layer_links_deleted"`
	// UntaggedDeleted 是删掉的无 tag manifest 数。
	UntaggedDeleted int `json:"untagged_deleted"`
	// Error 是整体错误原文。
	Error string `json:"error,omitempty"`
	// StartedAt / FinishedAt 是起止时间（unix 秒）。
	StartedAt  int64  `json:"started_at"`
	FinishedAt *int64 `json:"finished_at,omitempty"`
}

// Package gc 负责 blob 回收（把"已解引用但仍占着空间"的 blob 清掉）。
//
// **回收动作由 distribution 自己执行**：lau6-cr 调度
// `distribution garbage-collect <config>` 子命令，
// 由上游的 mark-and-sweep 扫当前 storage 驱动。
//
// ⚠️ 为什么不是 lau6-cr 自己扫盘：
//
//   - 对象存储（S3）下 blob 在桶里，没有本地目录可扫——
//     lau6-cr 自己扫只能扫到"空"，跑完什么都没删（11-storage.md §7）；
//   - 上游那份实现是唯一清楚"哪些 blob 还被引用"的代码，
//     自己重写一遍等于把"漏判引用 → 删掉还在用的层"的事故风险
//     搬到本仓库（03-registry.md §5.5）。
//
// ⚠️ 回收是**破坏性**的：它直接删对象。调用方必须保证：
//
//  1. 回收期间没有并发写入（用 Barrier 关闸，见 barrier.go）；
//  2. 用的是 **registry 自己在用的那份** distribution 配置。
package gc

import (
	"context"
	"fmt"
	"time"

	"github.com/lau6code/lau6-cr/apps/api/internal/distribution"
)

// Logger 是本包需要的日志接口。
//
// 只声明用到的三个级别，避免把 service 包的 Logger 类型引进底层包。
type Logger interface {
	Error(msg string, args ...any)
	Warn(msg string, args ...any)
	Info(msg string, args ...any)
}

// CollectOptions 是一次回收的执行参数。
type CollectOptions struct {
	// DryRun 为 true 时只评估不删除（上游 --dry-run）。
	DryRun bool
	// DeleteUntagged 删除无 tag 的 manifest（上游 --delete-untagged）。
	//
	// 默认关闭：无 tag 的镜像仍可按 digest 拉取，删掉等于毁掉它。
	DeleteUntagged bool
}

// Report 是 blob 回收的执行报告。
type Report struct {
	// Repositories 是扫到的仓库数。
	Repositories int `json:"repositories"`
	// ManifestsFound 是扫到的 manifest 修订数。
	ManifestsFound int `json:"manifests_found"`
	// BlobsMarked 是被引用的 blob 数（标记集合大小）。
	BlobsMarked int `json:"blobs_marked"`
	// BlobsScanned 是存储里的 blob 总数。
	BlobsScanned int `json:"blobs_scanned"`
	// BlobsDeleted 是删掉的 blob 数（dry_run 时是"将被删掉的"）。
	BlobsDeleted int `json:"blobs_deleted"`
	// BytesReclaimed 是回收的字节数（dry_run 时是"将回收的"）。
	BytesReclaimed int64 `json:"bytes_reclaimed"`
	// LayerLinksDeleted 是删掉的层链接数。
	LayerLinksDeleted int `json:"layer_links_deleted"`
	// UntaggedDeleted 是删掉的无 tag manifest 数。
	UntaggedDeleted int `json:"untagged_deleted"`
	// DurationMs 是耗时（毫秒）。
	DurationMs int64 `json:"duration_ms"`
}

// Collect 跑一次 blob 回收。
//
// ⚠️ 失败一律返回 error，不做"部分成功"的伪装。
func (e *Executor) Collect(ctx context.Context, opts CollectOptions) (*Report, error) {
	start := time.Now()

	if err := e.Precheck(); err != nil {
		return nil, err
	}
	cfg, err := distribution.LoadConfig(e.ConfigPath)
	if err != nil {
		return nil, err
	}
	st := storageFrom(cfg)

	// ① 回收前的存量：blob 总数与 manifest 修订数。
	//
	//    blob 总数用来填"扫到几个 blob"，也用来算释放了多少字节。
	beforeCount, beforeBytes, err := blobTotals(ctx, st)
	if err != nil {
		return nil, fmt.Errorf("统计回收前的 blob 总量失败: %w", err)
	}
	manifests, err := manifestCount(ctx, st)
	if err != nil {
		return nil, fmt.Errorf("统计 manifest 修订数失败: %w", err)
	}

	// ② 调度上游 GC。
	out, err := e.runGC(ctx, opts)
	if err != nil {
		return nil, err
	}

	// ③ 释放的字节数。
	reclaimed, err := e.reclaimedBytes(ctx, st, out, opts.DryRun, beforeBytes)
	if err != nil {
		return nil, err
	}

	return &Report{
		Repositories:      out.Repositories,
		ManifestsFound:    manifests,
		BlobsMarked:       out.BlobsMarked,
		BlobsScanned:      beforeCount,
		BlobsDeleted:      len(out.DeletedBlobs),
		BytesReclaimed:    reclaimed,
		LayerLinksDeleted: out.LayerLinks,
		UntaggedDeleted:   out.Untagged,
		DurationMs:        time.Since(start).Milliseconds(),
	}, nil
}

// reclaimedBytes 算出这次回收释放了多少字节。
//
// 两种口径，原因是对象删掉之后就量不到了：
//
//   - 正式回收：回收后的 blob 总量比回收前少了多少；
//   - 试运行：什么都没删，按上游给的"待删摘要清单"逐个量大小。
func (e *Executor) reclaimedBytes(ctx context.Context, st storage, out *gcOutput, dryRun bool, beforeBytes int64) (int64, error) {
	if dryRun {
		total, err := blobSizesOf(ctx, st, out.DeletedBlobs)
		if err != nil {
			return 0, fmt.Errorf("统计待删 blob 的大小失败: %w", err)
		}
		return total, nil
	}
	_, afterBytes, err := blobTotals(ctx, st)
	if err != nil {
		return 0, fmt.Errorf("统计回收后的 blob 总量失败: %w", err)
	}
	if afterBytes > beforeBytes {
		// 回收只会让 blob 变少。变多说明有别的东西在同时写——
		// 这时"释放字节"没有意义，记 0 并把异常说出来。
		if e.Logger != nil {
			e.Logger.Warn("回收后 blob 总量反而变大了，释放字节按 0 计",
				"before", beforeBytes, "after", afterBytes)
		}
		return 0, nil
	}
	return beforeBytes - afterBytes, nil
}

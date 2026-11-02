package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lau6code/lau6-cr/apps/api/internal/config"
	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
	"github.com/lau6code/lau6-cr/apps/api/internal/gc"
)

// blob 回收执行器的测试：真实 SQLite + 真实的磁盘布局 + 真的调起一个子进程。
//
// 回收动作由 distribution 执行，所以测试里有一个"假 distribution"：
// 测试二进制在特定环境变量下**假扮** distribution，
// 按上游的输出格式打印统计，并按需删掉指定的 blob。

// fakeDistributionEnv 让测试二进制**假扮** distribution。
const fakeDistributionEnv = "GC_FAKE_DISTRIBUTION"

// TestMain 在特定环境变量下把本测试二进制变成"假 distribution"。
func TestMain(m *testing.M) {
	if os.Getenv(fakeDistributionEnv) != "" {
		fakeDistributionMain()
		return
	}
	os.Exit(m.Run())
}

// fakeDistributionMain 模拟上游 `distribution garbage-collect`。
func fakeDistributionMain() {
	if argsFile := os.Getenv("GC_FAKE_ARGS_FILE"); argsFile != "" {
		_ = os.WriteFile(argsFile, []byte(strings.Join(os.Args[1:], " ")), 0o600)
	}
	if os.Getenv("GC_FAKE_FAIL") != "" {
		fmt.Fprintln(os.Stderr, "failed to garbage collect: fake failure")
		os.Exit(1)
	}

	dryRun := false
	for _, a := range os.Args[1:] {
		if a == "--dry-run" {
			dryRun = true
		}
	}

	blobPath := os.Getenv("GC_FAKE_BLOB_PATH")
	blobDigest := os.Getenv("GC_FAKE_BLOB_DIGEST")
	if !dryRun && blobPath != "" {
		if err := os.Remove(blobPath); err != nil {
			fmt.Fprintf(os.Stderr, "fake delete failed: %v\n", err)
			os.Exit(1)
		}
	}

	fmt.Println("test/postgres")
	fmt.Println()
	fmt.Println("1 blobs marked, 1 blobs and 0 manifests eligible for deletion")
	if blobDigest != "" {
		fmt.Printf("blob eligible for deletion: %s\n", blobDigest)
	}
}

// writeGCBlob 往假存储里写一个 blob，返回摘要与数据文件路径。
func writeGCBlob(t *testing.T, root string, content []byte) (digest string, dataPath string) {
	t.Helper()
	sum := sha256.Sum256(content)
	hexs := hex.EncodeToString(sum[:])
	dir := filepath.Join(root, "docker", "registry", "v2", "blobs", "sha256", hexs[:2], hexs)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建 blob 目录: %v", err)
	}
	dataPath = filepath.Join(dir, "data")
	if err := os.WriteFile(dataPath, content, 0o644); err != nil {
		t.Fatalf("写 blob: %v", err)
	}
	return "sha256:" + hexs, dataPath
}

// newGCFixture 搭一套 blob 回收环境：真实 DB + 真实存储布局 + 假 distribution。
func newGCFixture(t *testing.T) (*GCService, *config.Config, string) {
	t.Helper()
	db := newTestDB(t)
	root := t.TempDir()

	// distribution 用的配置文件（回收按它的 storage 段去清）。
	configPath := filepath.Join(root, "distribution-config.yml")
	body := fmt.Sprintf("version: 0.1\nstorage:\n  filesystem:\n    rootdirectory: %s\n  delete:\n    enabled: true\n", root)
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatalf("写 distribution 配置: %v", err)
	}

	t.Setenv(fakeDistributionEnv, "1")
	t.Setenv("GC_FAKE_ARGS_FILE", filepath.Join(t.TempDir(), "args.txt"))

	cfg := config.Default()
	cfg.Cleanup.GC.Enabled = true
	cfg.Cleanup.GC.DryRun = false
	cfg.Cleanup.GC.DrainTimeout = 5 * time.Second

	svc := NewGCService(GCDeps{
		DB:         db,
		Logger:     &fakeLogger{},
		SettingsFn: func() *config.Config { return cfg },
		Barrier:    gc.NewBarrier(),
		Executor:   gc.NewExecutor(os.Args[0], configPath, nil),
		Ctx:        context.Background(),
	})
	return svc, cfg, root
}

// waitGCRun 等一次回收跑到终态，返回最终的执行记录。
func waitGCRun(t *testing.T, svc *GCService, id int64) *domain.GCRun {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		run, err := svc.db.GCRunByID(context.Background(), id)
		if err != nil {
			t.Fatalf("读执行记录: %v", err)
		}
		if run.Status != domain.GCStatusRunning {
			return run
		}
		if time.Now().After(deadline) {
			t.Fatalf("回收一直停在 running")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestGCServiceRejectsWhenDisabled 覆盖"总开关没开 → 明确拒绝"。
func TestGCServiceRejectsWhenDisabled(t *testing.T) {
	db := newTestDB(t)
	cfg := config.Default()
	svc := NewGCService(GCDeps{
		DB:         db,
		Logger:     &fakeLogger{},
		SettingsFn: func() *config.Config { return cfg },
	})

	_, err := svc.Run(context.Background(), GCRunOptions{Trigger: domain.GCTriggerManual})
	if err == nil {
		t.Fatalf("GC 未启用时竟然执行成功")
	}
	if !errors.Is(err, ErrGCDisabled) {
		t.Fatalf("错误 = %v，期望 ErrGCDisabled", err)
	}
}

// TestGCServiceRequiresDistribution 覆盖"没有可调度的 distribution → 明确报错"。
//
// 这不是"跳过"：回收做不了时必须说出来，
// 静默跳过会让人以为"空间已经回收了"。
func TestGCServiceRequiresDistribution(t *testing.T) {
	db := newTestDB(t)
	cfg := config.Default()
	cfg.Cleanup.GC.Enabled = true
	svc := NewGCService(GCDeps{
		DB:         db,
		Logger:     &fakeLogger{},
		SettingsFn: func() *config.Config { return cfg },
	})

	_, err := svc.Run(context.Background(), GCRunOptions{Trigger: domain.GCTriggerManual})
	if err == nil {
		t.Fatalf("没有可调度的 distribution 时竟然报告成功")
	}
	if !strings.Contains(err.Error(), "distribution") {
		t.Fatalf("错误信息里没有指出 distribution: %v", err)
	}
}

// TestGCServiceRejectsMissingDistributionBinary 覆盖"distribution 不可用 → 同步报错"。
//
// 动手前就能发现的问题不能拖成后台任务——
// 否则用户点了"执行"只看到"已开始"，然后什么都没发生。
func TestGCServiceRejectsMissingDistributionBinary(t *testing.T) {
	db := newTestDB(t)
	cfg := config.Default()
	cfg.Cleanup.GC.Enabled = true
	svc := NewGCService(GCDeps{
		DB:         db,
		Logger:     &fakeLogger{},
		SettingsFn: func() *config.Config { return cfg },
		Executor:   gc.NewExecutor("/nonexistent/distribution", "/nonexistent/config.yml", nil),
	})

	_, err := svc.Run(context.Background(), GCRunOptions{Trigger: domain.GCTriggerManual})
	if err == nil {
		t.Fatalf("distribution 不可用时竟然报告成功")
	}
}

// TestGCServiceRunsInBackgroundAndRecords 覆盖"回收在后台跑 → 记录最终落库"。
func TestGCServiceRunsInBackgroundAndRecords(t *testing.T) {
	svc, _, root := newGCFixture(t)

	orphan, orphanPath := writeGCBlob(t, root, []byte("orphan blob"))
	t.Setenv("GC_FAKE_BLOB_PATH", orphanPath)
	t.Setenv("GC_FAKE_BLOB_DIGEST", orphan)

	run, err := svc.Run(context.Background(), GCRunOptions{Trigger: domain.GCTriggerManual})
	if err != nil {
		t.Fatalf("启动回收失败: %v", err)
	}
	// 立即返回的是"已开始"的记录。
	if run.Status != domain.GCStatusRunning {
		t.Fatalf("立即返回的状态 = %q，期望 running", run.Status)
	}

	final := waitGCRun(t, svc, run.ID)
	if final.Status != domain.GCStatusSuccess {
		t.Fatalf("状态 = %q，期望 success（错误：%s）", final.Status, final.Error)
	}
	if final.BlobsDeleted != 1 {
		t.Fatalf("删掉的 blob 数 = %d，期望 1", final.BlobsDeleted)
	}
	if final.BytesReclaimed != int64(len("orphan blob")) {
		t.Fatalf("回收字节数 = %d，期望 %d", final.BytesReclaimed, len("orphan blob"))
	}
	if final.StorageRoot != root {
		t.Fatalf("回收目标 = %q，期望 %q", final.StorageRoot, root)
	}

	// 孤儿 blob 的数据文件必须真的没了。
	if _, err := os.Stat(orphanPath); !os.IsNotExist(err) {
		t.Fatalf("孤儿 blob 还在盘上")
	}

	// 执行历史里能查到。
	runs, err := svc.db.ListGCRuns(context.Background(), 10, 0)
	if err != nil {
		t.Fatalf("读执行历史: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("执行历史条数 = %d，期望 1", len(runs))
	}
	if runs[0].BlobsDeleted != 1 {
		t.Fatalf("历史记录里的删除数 = %d，期望 1", runs[0].BlobsDeleted)
	}
}

// TestGCServiceDryRunKeepsFiles 覆盖"试运行不删任何东西"。
func TestGCServiceDryRunKeepsFiles(t *testing.T) {
	svc, cfg, root := newGCFixture(t)
	cfg.Cleanup.GC.DryRun = true

	orphan, orphanPath := writeGCBlob(t, root, []byte("orphan blob"))
	t.Setenv("GC_FAKE_BLOB_PATH", orphanPath)
	t.Setenv("GC_FAKE_BLOB_DIGEST", orphan)

	run, err := svc.Run(context.Background(), GCRunOptions{Trigger: domain.GCTriggerManual})
	if err != nil {
		t.Fatalf("启动回收失败: %v", err)
	}
	final := waitGCRun(t, svc, run.ID)
	if final.Status != domain.GCStatusSuccess {
		t.Fatalf("状态 = %q，期望 success（错误：%s）", final.Status, final.Error)
	}
	if final.BlobsDeleted != 1 {
		t.Fatalf("试运行报告的待删数 = %d，期望 1", final.BlobsDeleted)
	}
	if final.BytesReclaimed != int64(len("orphan blob")) {
		t.Fatalf("试运行的预计释放 = %d，期望 %d", final.BytesReclaimed, len("orphan blob"))
	}
	if _, err := os.Stat(orphanPath); err != nil {
		t.Fatalf("试运行竟然把文件删了: %v", err)
	}
}

// TestGCServiceRecordsFailure 覆盖"回收失败 → 执行记录落 failed + 原因"。
//
// 这是"禁止静默异常"的落点：回收在后台跑，
// 失败必须落在执行记录里（否则就是"点了没反应，也不知道成没成"）。
func TestGCServiceRecordsFailure(t *testing.T) {
	svc, _, _ := newGCFixture(t)
	t.Setenv("GC_FAKE_FAIL", "1")

	run, err := svc.Run(context.Background(), GCRunOptions{Trigger: domain.GCTriggerManual})
	if err != nil {
		t.Fatalf("启动回收失败: %v", err)
	}
	final := waitGCRun(t, svc, run.ID)
	if final.Status != domain.GCStatusFailed {
		t.Fatalf("状态 = %q，期望 failed", final.Status)
	}
	if final.Error == "" {
		t.Fatal("失败记录里必须有原因")
	}
}

// TestGCTriggerAfterCleanupSkipsWhenDisabled 覆盖"GC 没开 → 清理后不自动回收"。
func TestGCTriggerAfterCleanupSkipsWhenDisabled(t *testing.T) {
	db := newTestDB(t)
	cfg := config.Default()
	cfg.Cleanup.GC.Enabled = false
	svc := NewGCService(GCDeps{
		DB:         db,
		Logger:     &fakeLogger{},
		SettingsFn: func() *config.Config { return cfg },
	})

	run, err := svc.TriggerAfterCleanup(context.Background(), "system")
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if run != nil {
		t.Fatalf("GC 未启用时不该执行回收")
	}
}

// TestGCSchedulerFiresAtConfiguredTime 覆盖"到点自动跑一次"。
//
// 定时回收是这次功能的重点触发路径，必须实测——
// 配置的时间点、当天只跑一次、执行记录落库，三样都要对。
func TestGCSchedulerFiresAtConfiguredTime(t *testing.T) {
	svc, cfg, root := newGCFixture(t)

	orphan, orphanPath := writeGCBlob(t, root, []byte("orphan blob"))
	t.Setenv("GC_FAKE_BLOB_PATH", orphanPath)
	t.Setenv("GC_FAKE_BLOB_DIGEST", orphan)

	cfg.Cleanup.GC.ScheduleEnabled = true
	cfg.Cleanup.GC.ScheduleAt = time.Now().Format("15:04")
	cfg.Cleanup.GC.DryRun = true

	old := gcTickInterval
	gcTickInterval = 10 * time.Millisecond
	defer func() { gcTickInterval = old }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svc.RunScheduler(ctx)

	deadline := time.Now().Add(5 * time.Second)
	for {
		runs, err := svc.db.ListGCRuns(context.Background(), 10, 0)
		if err != nil {
			t.Fatalf("读执行历史: %v", err)
		}
		if len(runs) > 0 {
			// 记录在执行**开始**时就落库（status=running），
			// 收尾才改成终态——这里要等它跑完，别读到中间态。
			if runs[0].Status == domain.GCStatusRunning {
				if time.Now().After(deadline) {
					t.Fatalf("定时回收一直停在 running")
				}
				time.Sleep(20 * time.Millisecond)
				continue
			}
			if runs[0].Trigger != domain.GCTriggerSchedule {
				t.Fatalf("触发方式 = %q，期望 %q", runs[0].Trigger, domain.GCTriggerSchedule)
			}
			if runs[0].Status != domain.GCStatusSuccess {
				t.Fatalf("定时回收状态 = %q（错误：%s）", runs[0].Status, runs[0].Error)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("定时回收没有在配置的时间点触发")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestGCSchedulerTickReachesEveryMinute 锁定"唤醒间隔与分钟边界不对齐"这个坑。
//
// 整分钟的 ticker 只会在"启动时刻的分偏移"上醒——配置成别的分钟就**永远不触发**。
// 10 秒的间隔必须让每个"小时:分钟"都被扫到。
func TestGCSchedulerTickReachesEveryMinute(t *testing.T) {
	// 从一个"最不友好"的时刻起步：秒 = 37（分偏移最刁钻）。
	start := time.Date(2026, 10, 28, 3, 17, 37, 0, time.Local)
	seen := map[int]bool{}
	for i := 0; i < 400; i++ {
		now := start.Add(time.Duration(i) * 10 * time.Second)
		for m := 0; m < 60; m++ {
			if scheduleDue(now, now.Hour(), m) {
				seen[m] = true
			}
		}
	}
	for m := 0; m < 60; m++ {
		if !seen[m] {
			t.Fatalf("第 %d 分钟没被扫到", m)
		}
	}
}

// TestScheduleDue 覆盖"到点判断"本身。
func TestScheduleDue(t *testing.T) {
	now := time.Date(2026, 10, 30, 2, 0, 0, 0, time.Local)
	if !scheduleDue(now, 2, 0) {
		t.Fatal("02:00 应该算到点")
	}
	if scheduleDue(now, 2, 1) {
		t.Fatal("02:01 不该算到点")
	}
	if scheduleDue(now, 3, 0) {
		t.Fatal("03:00 不该算到点")
	}
}

// TestGCSchedulerDoesNotFireWhenDisabled 覆盖"GC 没开 → 定时器不触发"。
func TestGCSchedulerDoesNotFireWhenDisabled(t *testing.T) {
	db := newTestDB(t)
	cfg := config.Default()
	cfg.Cleanup.GC.Enabled = false
	svc := NewGCService(GCDeps{
		DB:         db,
		Logger:     &fakeLogger{},
		SettingsFn: func() *config.Config { return cfg },
	})

	old := gcTickInterval
	gcTickInterval = 10 * time.Millisecond
	defer func() { gcTickInterval = old }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svc.RunScheduler(ctx)

	time.Sleep(100 * time.Millisecond)
	runs, err := svc.db.ListGCRuns(context.Background(), 10, 0)
	if err != nil {
		t.Fatalf("读执行历史: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("GC 未启用却产生了 %d 条执行记录", len(runs))
	}
}

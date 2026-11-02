package gc

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/lau6code/lau6-cr/apps/api/internal/distribution"
)

// Executor 调度 distribution 完成 blob 回收。
type Executor struct {
	// Bin 是 distribution 可执行文件路径。
	Bin string
	// ConfigPath 是 distribution 的配置文件路径。
	//
	// ⚠️ 必须是 **registry 自己在用的那份**：
	//    GC 是按这份配置里的 storage 段去删对象的，
	//    不是同一份就会删到别的地方去
	//    （症状是"回收跑完了，桶里的东西还在"）。
	ConfigPath string
	// Logger 收上游的输出。为 nil 时丢弃。
	Logger Logger
}

// NewExecutor 构造回收执行器。
func NewExecutor(bin, configPath string, logger Logger) *Executor {
	return &Executor{Bin: bin, ConfigPath: configPath, Logger: logger}
}

// Precheck 校验回收的执行条件（不跑回收）。
//
// 目的：把"二进制不存在""配置读不到"这类**动手前就能发现**的问题
// 同步暴露给调用方，而不是让它变成一个跑起来才失败的后台任务。
func (e *Executor) Precheck() error {
	if strings.TrimSpace(e.Bin) == "" {
		return fmt.Errorf("未配置 distribution 可执行文件路径")
	}
	if strings.TrimSpace(e.ConfigPath) == "" {
		return fmt.Errorf("未配置 distribution 配置文件路径")
	}
	if _, err := os.Stat(e.Bin); err != nil {
		return fmt.Errorf("distribution 可执行文件 %s 不可用: %w", e.Bin, err)
	}
	if _, err := os.Stat(e.ConfigPath); err != nil {
		return fmt.Errorf("distribution 配置文件 %s 不可用: %w", e.ConfigPath, err)
	}
	return nil
}

// Target 描述这次回收针对的存储（写进执行记录与审计里）。
//
// 本地存储 → 存储根目录；对象存储 → s3://<bucket>。
//
// ⚠️ 读不出配置时返回空串：真正的失败原因由回收本身报出来，
//
//	这里只是给记录留一个可读的标识。
func (e *Executor) Target() (string, error) {
	cfg, err := distribution.LoadConfig(e.ConfigPath)
	if err != nil {
		return "", err
	}
	switch {
	case cfg.Storage.Filesystem != nil:
		return cfg.Storage.Filesystem.Rootdirectory, nil
	case cfg.Storage.S3 != nil:
		return "s3://" + cfg.Storage.S3.Bucket, nil
	default:
		return "", fmt.Errorf("distribution 配置里没有可识别的 storage 段（filesystem / s3）")
	}
}

// runGC 跑上游的 garbage-collect 子命令。
//
// 上游把统计写在 stdout、把"删了哪个 blob"写进 stderr（logrus 默认输出），
// 两边都要收：stdout 是要解析的统计，stderr 是排障线索。
func (e *Executor) runGC(ctx context.Context, opts CollectOptions) (*gcOutput, error) {
	args := []string{"garbage-collect", e.ConfigPath}
	if opts.DryRun {
		args = append(args, "--dry-run")
	}
	if opts.DeleteUntagged {
		args = append(args, "--delete-untagged")
	}

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, e.Bin, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	e.logUpstream(stderr.String())

	if runErr != nil {
		return nil, fmt.Errorf("distribution garbage-collect 失败: %w（上游输出: %s）",
			runErr, outputTail(stdout.String()+"\n"+stderr.String()))
	}
	out, err := parseGCOutput(stdout.String())
	if err != nil {
		return nil, fmt.Errorf("回收已执行，但无法解析上游统计: %w（上游输出: %s）",
			err, outputTail(stdout.String()))
	}
	return &out, nil
}

// logUpstream 把上游 stderr 转进 lau6-cr 的日志。
//
// 上游逐条打印"删了哪个 blob"——那是最直接的排障线索，不能丢。
func (e *Executor) logUpstream(text string) {
	if e.Logger == nil {
		return
	}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		e.Logger.Info("distribution gc: " + line)
	}
}

// gcSummaryRe 匹配上游 GC 的统计行。
//
// 上游原文（registry/storage/garbagecollect.go）：
//
//	"%d blobs marked, %d blobs and %d manifests eligible for deletion"
var gcSummaryRe = regexp.MustCompile(`^(\d+) blobs marked, (\d+) blobs and (\d+) manifests eligible for deletion$`)

// gcOutput 是上游 GC 输出的解析结果。
type gcOutput struct {
	// Repositories 是扫到的仓库数。
	Repositories int
	// BlobsMarked 是被引用的 blob 数。
	BlobsMarked int
	// DeletedBlobs 是待删/已删的 blob 摘要。
	DeletedBlobs []string
	// LayerLinks 是待删/已删的层链接数。
	LayerLinks int
	// Untagged 是待删/已删的无 tag manifest 数。
	Untagged int
}

// parseGCOutput 解析上游 GC 的 stdout。
//
// ⚠️ 统计行**必须**解析出来：解析不了就报错，
//
//	不能当成"回收了 0 个 blob"——
//	那是把"统计不出来"说成"没有垃圾"，属于假成功。
func parseGCOutput(out string) (gcOutput, error) {
	var st gcOutput
	summarySeen := false
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if m := gcSummaryRe.FindStringSubmatch(line); m != nil {
			marked, err := strconv.Atoi(m[1])
			if err != nil {
				return st, fmt.Errorf("统计行里的 blob 数解析失败（%q）: %w", m[1], err)
			}
			st.BlobsMarked = marked
			summarySeen = true
			continue
		}
		if rest, ok := strings.CutPrefix(line, "blob eligible for deletion: "); ok {
			st.DeletedBlobs = append(st.DeletedBlobs, strings.TrimSpace(rest))
			continue
		}
		if _, ok := strings.CutPrefix(line, "manifest eligible for deletion: "); ok {
			st.Untagged++
			continue
		}
		if strings.Contains(line, ": layer link eligible for deletion: ") {
			st.LayerLinks++
			continue
		}
		// 上游在标记阶段给每个仓库打印一行仓库名。
		// 仓库名只含 [a-z0-9._/-]（不含空格与等号），
		// 而日志行一定带空格——靠这个把两者分开。
		if !strings.ContainsAny(line, " \t=") {
			st.Repositories++
		}
	}
	if !summarySeen {
		return st, fmt.Errorf("上游输出里没有统计行（格式可能变了）")
	}
	return st, nil
}

// outputTail 取输出末尾若干字符，用于错误信息。
//
// 不截断会很长，但不截断又会把整段日志塞进错误信息里——
// 取尾部是因为**失败原因通常在最后**（上游是最后才报错的）。
func outputTail(text string) string {
	const limit = 500
	trimmed := strings.TrimSpace(text)
	if len(trimmed) <= limit {
		return trimmed
	}
	return "…" + trimmed[len(trimmed)-limit:]
}

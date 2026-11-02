// Package log 负责 lau6-cr 的日志初始化与文件轮转。
//
// 结构参考 lf-db 的 internal/log：配置驱动（level / output / file），
// 写文件时按天或按大小轮转，并按保留天数清理过期文件。
//
// 与 lf-db 的差异：
//
//  1. lau6-cr 容器内以 PID 1 直接运行，stdout/stderr 由 docker 日志驱动轮转，
//     因此文件日志的定位是**长期留存的排障档案**，不是容器日志的替代品。
//     默认 output=both，文件目录落在 /data/logs（随数据卷持久化）。
//  2. 保留期清理按**文件名里的日期**判断，而不是文件 mtime——
//     容器重启后 mtime 会变，但文件名日期不会。
package log

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Options 是日志配置。
type Options struct {
	// Level 是日志级别：debug|info|warn|error。空则按 info。
	Level string
	// Output 是输出目标：console|file|both。空则按 both。
	Output string
	// File 控制文件日志行为。
	File FileOptions
}

// FileOptions 是文件日志配置。
type FileOptions struct {
	// Enabled 是否允许写文件。output 含 file 时必须为 true。
	Enabled bool
	// Dir 是日志目录。
	Dir string
	// Filename 是日志文件名（实际落盘会带日期后缀）。
	Filename string
	// MaxSizeMB 是单个文件大小上限（MB），超出即滚动。
	MaxSizeMB int
	// Rotate 是滚动方式：daily（按天，默认）| size（只按大小）。
	Rotate string
	// RetainDays 是日志保留天数，超期的文件被删除。
	RetainDays int
}

// Logger 是构造好的日志器与其附属资源。
type Logger struct {
	// Logger 是对外暴露的 slog 实例。
	Logger *slog.Logger
	// Closer 关闭底层文件句柄。进程退出前必须调用。
	Closer io.Closer
	// levelVar 是日志级别的可变句柄。
	//
	// slog 的 HandlerOptions.Level 接受它，改级别是**原子**的——
	// 不需要重建 handler，也不需要重建文件写入器。
	// 排障时在设置页调级别能立即生效，就是靠这个。
	levelVar *slog.LevelVar
}

// SetLevel 动态调整日志级别。
//
// 非法级别返回错误且**不改动**当前级别——
// 宁可保持原样，也不要因为一次误操作把日志全关了。
func (l *Logger) SetLevel(raw string) error {
	level, err := parseLevel(raw)
	if err != nil {
		return err
	}
	l.levelVar.Set(level)
	return nil
}

// Level 返回当前日志级别。
func (l *Logger) Level() slog.Level {
	return l.levelVar.Level()
}

// New 按配置构造日志器。
//
// output 取值：
//   - "console"：仅输出到 stdout
//   - "file"：仅写日志文件
//   - "both"：两者都写（默认）
//
// 文件日志打不开是**致命错误**——配置写了 file 却写不进去，
// 静默降级成只打控制台会让人以为日志存下来了。
func New(opts Options) (*Logger, error) {
	level, err := parseLevel(opts.Level)
	if err != nil {
		return nil, err
	}

	output := strings.ToLower(strings.TrimSpace(opts.Output))
	if output == "" {
		output = "both"
	}
	switch output {
	case "console", "file", "both":
	default:
		return nil, fmt.Errorf("logging.output 非法（%q）：支持 console|file|both", opts.Output)
	}

	var writers []io.Writer
	var closer io.Closer = noopCloser{}

	if output == "console" || output == "both" {
		writers = append(writers, os.Stdout)
	}
	if output == "file" || output == "both" {
		if !opts.File.Enabled {
			return nil, fmt.Errorf("logging.output=%q 要求 logging.file.enabled=true", output)
		}
		w, c, err := newRotatingWriter(opts.File)
		if err != nil {
			return nil, err
		}
		writers = append(writers, w)
		closer = c
	}

	levelVar := new(slog.LevelVar)
	levelVar.Set(level)
	handler := slog.NewJSONHandler(&fanoutWriter{writers: writers}, &slog.HandlerOptions{
		Level: levelVar,
	})
	return &Logger{Logger: slog.New(handler), Closer: closer, levelVar: levelVar}, nil
}

// fanoutWriter 把一条日志写进多个目标。
//
// 与 io.MultiWriter 不同：各目标独立写，一个失败（如 stdout 管道断）
// 不影响其余目标（如文件日志），避免"stdout 一断、文件日志整条不写"。
type fanoutWriter struct {
	writers []io.Writer
}

func (w *fanoutWriter) Write(p []byte) (int, error) {
	var firstErr error
	for _, ww := range w.writers {
		if _, err := ww.Write(p); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return len(p), firstErr
}

// parseLevel 解析日志级别。未知级别即配置错误——
// 静默降级成 info 会让人以为开了 debug 其实没有。
func parseLevel(raw string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("logging.level 非法（%q）：支持 debug|info|warn|error", raw)
	}
}

type noopCloser struct{}

func (noopCloser) Close() error { return nil }

// rotatingWriter 是按天/按大小滚动的文件写入器。
type rotatingWriter struct {
	cfg      FileOptions
	baseName string
	mode     string // "daily" 或 "size"
	now      func() time.Time
	mu       sync.Mutex
	file     *os.File
	curPath  string
	curSize  int64
	seq      int
}

// parseRotateMode 解析滚动方式：
//   - "daily"（默认）：按天切文件，单日内超出 max_size_mb 再按 .001/.002 序号滚
//   - "size"：只按大小滚，不按天切
//   - 空串/未知值：回退 daily（不静默报错——保留向后兼容）
func parseRotateMode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "size":
		return "size"
	default:
		return "daily"
	}
}

// newRotatingWriter 创建文件轮转写入器，并启动保留期清理协程。
//
// 目录/文件名/大小上限/保留天数缺失即配置错误——
// 这些值没有安全的默认值，猜一个比报错更糟。
func newRotatingWriter(cfg FileOptions) (io.Writer, io.Closer, error) {
	w, err := newRotatingWriterWithClock(cfg, time.Now)
	if err != nil {
		return nil, nil, err
	}
	go w.retentionLoop()
	return w, w, nil
}

// newRotatingWriterWithClock 与 newRotatingWriter 同语义，但允许注入时钟（仅供测试）。
//
// ⚠️ 它**不启动**保留期清理协程：测试要能确定性地断言 cleanup 的行为，
// 而后台协程会在构造后立刻删掉过期文件，与测试造的样本打架。
func newRotatingWriterWithClock(cfg FileOptions, clock func() time.Time) (*rotatingWriter, error) {
	if cfg.Dir == "" {
		return nil, fmt.Errorf("logging.file.dir 不能为空")
	}
	if cfg.Filename == "" {
		return nil, fmt.Errorf("logging.file.filename 不能为空")
	}
	if cfg.MaxSizeMB <= 0 {
		return nil, fmt.Errorf("logging.file.max_size_mb 缺失或非法（需 >0）")
	}
	if cfg.RetainDays <= 0 {
		return nil, fmt.Errorf("logging.file.retain_days 缺失或非法（需 >0）")
	}

	base := strings.TrimSuffix(cfg.Filename, filepath.Ext(cfg.Filename))
	w := &rotatingWriter{cfg: cfg, baseName: base, mode: parseRotateMode(cfg.Rotate), now: clock}
	if err := w.rotateIfNeeded(clock()); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	now := w.now()
	if err := w.rotateIfNeeded(now); err != nil {
		return 0, err
	}

	maxBytes := int64(w.cfg.MaxSizeMB) * 1024 * 1024
	if w.curSize+int64(len(p)) > maxBytes {
		w.seq++
		if err := w.openFile(now); err != nil {
			return 0, err
		}
	}

	n, err := w.file.Write(p)
	w.curSize += int64(n)
	return n, err
}

func (w *rotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file != nil {
		return w.file.Close()
	}
	return nil
}

// rotateIfNeeded 检查当前文件是否还该写（跨天了 / 序号变了）。
func (w *rotatingWriter) rotateIfNeeded(now time.Time) error {
	expected := w.filePath(now, w.seq)
	if w.file != nil && w.curPath == expected {
		return nil
	}
	w.seq = 0
	return w.openFile(now)
}

func (w *rotatingWriter) openFile(now time.Time) error {
	if err := os.MkdirAll(w.cfg.Dir, 0o755); err != nil {
		return fmt.Errorf("创建日志目录失败: %w", err)
	}
	if w.file != nil {
		// 关旧文件失败不拦轮转：新文件才是接下来要写的地方。
		// 但必须留痕——否则旧文件的句柄就这么漏了，日志里查不到。
		if err := w.file.Close(); err != nil {
			slog.Warn("日志轮转：关闭旧文件失败", "path", w.curPath, "err", err)
		}
	}
	path := w.filePath(now, w.seq)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("打开日志文件失败: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	w.file = f
	w.curPath = path
	w.curSize = info.Size()
	return nil
}

// filePath 生成目标文件路径：
//   - daily：<base>.<YYYY-MM-DD>.log（大小触发时追加 .NNN）
//   - size ：<base>.log 起步，每次触发递增 .NNN（不嵌日期）
func (w *rotatingWriter) filePath(now time.Time, seq int) string {
	var name string
	switch w.mode {
	case "size":
		if seq == 0 {
			name = w.baseName
		} else {
			name = fmt.Sprintf("%s.%03d", w.baseName, seq)
		}
	default: // daily
		name = fmt.Sprintf("%s.%s", w.baseName, now.Format("2006-01-02"))
		if seq > 0 {
			name = fmt.Sprintf("%s.%03d", name, seq)
		}
	}
	return filepath.Join(w.cfg.Dir, name+".log")
}

// retentionLoop 定期清理过期日志。
func (w *rotatingWriter) retentionLoop() {
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	w.cleanup()
	for range ticker.C {
		w.cleanup()
	}
}

// cleanup 删除超过保留期的日志文件。
//
// 判据是**文件名里的日期**（daily 模式），不是文件 mtime：
// 容器重建 / 备份恢复都会改 mtime，用 mtime 会把昨天的日志误判成过期。
// size 模式没有日期，退回 mtime 判据。
func (w *rotatingWriter) cleanup() {
	entries, err := os.ReadDir(w.cfg.Dir)
	if err != nil {
		slog.Error("日志保留期清理失败：读取日志目录失败", "dir", w.cfg.Dir, "err", err)
		return
	}
	cutoff := w.now().AddDate(0, 0, -w.cfg.RetainDays)
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), w.baseName+".") {
			continue
		}
		expired := false
		if w.mode == "daily" {
			expired = w.expiredByName(e.Name(), cutoff)
		} else {
			info, err := e.Info()
			if err != nil {
				slog.Warn("日志保留期清理：读取文件信息失败", "name", e.Name(), "err", err)
				continue
			}
			expired = info.ModTime().Before(cutoff)
		}
		if !expired {
			continue
		}
		path := filepath.Join(w.cfg.Dir, e.Name())
		if err := os.Remove(path); err != nil {
			slog.Warn("日志保留期清理：删除过期日志失败", "path", path, "err", err)
		}
	}
}

// expiredByName 从文件名里解析日期，判断是否早于 cutoff。
//
// 文件名形如 <base>.2026-10-27.log 或 <base>.2026-10-27.001.log。
// 解析不出日期的一律判为**未过期**——宁可多留一个文件，也别误删当前在写的。
func (w *rotatingWriter) expiredByName(name string, cutoff time.Time) bool {
	rest := strings.TrimPrefix(name, w.baseName+".")
	rest = strings.TrimSuffix(rest, ".log")
	if i := strings.LastIndex(rest, "."); i >= 0 {
		// 去掉大小滚动产生的 .NNN 序号段。
		rest = rest[:i]
	}
	d, err := time.ParseInLocation("2006-01-02", rest, cutoff.Location())
	if err != nil {
		return false
	}
	// 文件名只有日期，保留到当天结束。
	return d.AddDate(0, 0, 1).Before(cutoff)
}

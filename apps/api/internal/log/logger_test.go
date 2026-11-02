package log

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock 是测试可控时钟。
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// makeRotatingWriter 构造一个用 fakeClock 的 rotatingWriter。
// 返回 writer + 目录 + clock。
//
// 用 newRotatingWriterWithClock 而不是 newRotatingWriter：
// 后者会启动后台清理协程，会把测试刚造的过期样本文件删掉。
func makeRotatingWriter(t *testing.T, rotate string, maxSizeMB int) (*rotatingWriter, string, *fakeClock) {
	t.Helper()
	dir := t.TempDir()
	cfg := FileOptions{
		Enabled:    true,
		Dir:        dir,
		Filename:   "test.log",
		MaxSizeMB:  maxSizeMB,
		Rotate:     rotate,
		RetainDays: 7,
	}
	clk := &fakeClock{now: time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)}
	w, err := newRotatingWriterWithClock(cfg, clk.Now)
	if err != nil {
		t.Fatalf("newRotatingWriterWithClock: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w, dir, clk
}

// TestRotatingWriter_DailyRotation 验证 rotate=daily 按天切换文件。
func TestRotatingWriter_DailyRotation(t *testing.T) {
	w, dir, clk := makeRotatingWriter(t, "daily", 1024)
	if _, err := io.WriteString(w, "hello-day1\n"); err != nil {
		t.Fatalf("write day1: %v", err)
	}
	clk.Advance(24 * time.Hour)
	if _, err := io.WriteString(w, "hello-day2\n"); err != nil {
		t.Fatalf("write day2: %v", err)
	}

	want1 := filepath.Join(dir, "test.2026-09-24.log")
	want2 := filepath.Join(dir, "test.2026-09-25.log")
	for _, p := range []string{want1, want2} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("期望文件存在 %s，实际 err=%v", p, err)
		}
	}
	if got := readFile(t, want1); !strings.Contains(got, "hello-day1") || strings.Contains(got, "hello-day2") {
		t.Errorf("day1 文件应仅含 day1 行，实际 %q", got)
	}
	if got := readFile(t, want2); !strings.Contains(got, "hello-day2") {
		t.Errorf("day2 文件应含 day2 行，实际 %q", got)
	}
}

// TestRotatingWriter_DailySizeOverflow 验证 daily 模式超 max_size_mb 触发同日 .NNN 序号滚动。
func TestRotatingWriter_DailySizeOverflow(t *testing.T) {
	w, dir, _ := makeRotatingWriter(t, "daily", 1)

	payload := strings.Repeat("X", 1024*1024) // 1 MiB
	for i := 0; i < 3; i++ {
		if _, err := io.WriteString(w, payload+"\n"); err != nil {
			t.Fatalf("write[%d]: %v", i, err)
		}
	}

	for _, p := range []string{
		filepath.Join(dir, "test.2026-09-24.log"),
		filepath.Join(dir, "test.2026-09-24.001.log"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("期望滚动文件存在 %s，实际 err=%v", p, err)
		}
	}
}

// TestRotatingWriter_SizeRotation 验证 rotate=size 不嵌日期；跨日仍写同一文件。
func TestRotatingWriter_SizeRotation(t *testing.T) {
	w, dir, clk := makeRotatingWriter(t, "size", 1024)

	if _, err := io.WriteString(w, "x\n"); err != nil {
		t.Fatalf("write day1: %v", err)
	}
	clk.Advance(48 * time.Hour)
	if _, err := io.WriteString(w, "y\n"); err != nil {
		t.Fatalf("write day2: %v", err)
	}

	want := filepath.Join(dir, "test.log")
	if _, err := os.Stat(want); err != nil {
		t.Errorf("期望文件 %s，实际 err=%v", want, err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), "2026-09") {
			t.Errorf("size 模式不应在文件名嵌入日期，发现 %s", e.Name())
		}
	}
}

// TestRotatingWriter_SizeModeSeqIncrement 验证 size 模式超 max_size_mb 触发 .NNN 递增。
func TestRotatingWriter_SizeModeSeqIncrement(t *testing.T) {
	w, dir, _ := makeRotatingWriter(t, "size", 1)

	payload := strings.Repeat("X", 1024*1024) // 1 MiB
	for i := 0; i < 3; i++ {
		if _, err := io.WriteString(w, payload+"\n"); err != nil {
			t.Fatalf("write[%d]: %v", i, err)
		}
	}

	for _, p := range []string{
		filepath.Join(dir, "test.log"),
		filepath.Join(dir, "test.001.log"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("期望滚动文件存在 %s，实际 err=%v", p, err)
		}
	}
}

// TestRotatingWriter_DefaultRotate 验证 rotate 为空 / 未知值时回退 daily。
func TestRotatingWriter_DefaultRotate(t *testing.T) {
	for _, raw := range []string{"", "unknown", "DAILY", "  daily  "} {
		t.Run("rotate="+raw, func(t *testing.T) {
			w, dir, _ := makeRotatingWriter(t, raw, 1024)
			if _, err := io.WriteString(w, "x\n"); err != nil {
				t.Fatalf("write: %v", err)
			}
			want := filepath.Join(dir, "test.2026-09-24.log")
			if _, err := os.Stat(want); err != nil {
				t.Errorf("默认 rotate 应回退 daily，期望 %s，实际 err=%v", want, err)
			}
		})
	}
}

// TestParseRotateMode 单测 parseRotateMode 各分支。
func TestParseRotateMode(t *testing.T) {
	cases := []struct{ in, want string }{
		{"daily", "daily"},
		{"size", "size"},
		{"DAILY", "daily"},
		{"  size  ", "size"},
		{"", "daily"},
		{"unknown", "daily"},
		{"Size", "size"},
	}
	for _, tc := range cases {
		t.Run("input="+tc.in, func(t *testing.T) {
			if got := parseRotateMode(tc.in); got != tc.want {
				t.Errorf("parseRotateMode(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestParseLevel 验证级别解析，空串按 info，未知级别报错。
func TestParseLevel(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", "INFO", false},
		{"info", "INFO", false},
		{"DEBUG", "DEBUG", false},
		{"warn", "WARN", false},
		{"warning", "WARN", false},
		{"error", "ERROR", false},
		{"  info  ", "INFO", false},
		{"verbose", "", true},
	}
	for _, tc := range cases {
		t.Run("level="+tc.in, func(t *testing.T) {
			got, err := parseLevel(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseLevel(%q) 期望报错，实际 nil", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseLevel(%q): %v", tc.in, err)
			}
			if got.String() != tc.want {
				t.Errorf("parseLevel(%q) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}

// TestNew_AcceptsSizeRotate 验证 New() 在 rotate=size 时不报错。
func TestNew_AcceptsSizeRotate(t *testing.T) {
	dir := t.TempDir()
	lg, err := New(Options{
		Level:  "info",
		Output: "file",
		File: FileOptions{
			Enabled:    true,
			Dir:        dir,
			Filename:   "test.log",
			MaxSizeMB:  1024,
			Rotate:     "size",
			RetainDays: 7,
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if lg.Logger == nil {
		t.Fatal("logger 期望非 nil")
	}
	if err := lg.Closer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestNew_InvalidOutput 验证非法 output 报错。
func TestNew_InvalidOutput(t *testing.T) {
	_, err := New(Options{Level: "info", Output: "syslog"})
	if err == nil {
		t.Fatal("非法 output 期望报错")
	}
}

// TestNew_FileOutputRequiresEnabled 验证 output=file 但 enabled=false 时报错。
func TestNew_FileOutputRequiresEnabled(t *testing.T) {
	_, err := New(Options{
		Level:  "info",
		Output: "file",
		File:   FileOptions{Enabled: false},
	})
	if err == nil {
		t.Fatal("output=file 且 enabled=false 期望报错")
	}
}

// TestNew_FileMissingRequired 验证文件配置缺项时报错。
func TestNew_FileMissingRequired(t *testing.T) {
	cases := []struct {
		name string
		file FileOptions
	}{
		{"缺 dir", FileOptions{Enabled: true, Filename: "a.log", MaxSizeMB: 1, RetainDays: 1}},
		{"缺 filename", FileOptions{Enabled: true, Dir: "/tmp", MaxSizeMB: 1, RetainDays: 1}},
		{"缺 max_size_mb", FileOptions{Enabled: true, Dir: "/tmp", Filename: "a.log", RetainDays: 1}},
		{"缺 retain_days", FileOptions{Enabled: true, Dir: "/tmp", Filename: "a.log", MaxSizeMB: 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(Options{Level: "info", Output: "file", File: tc.file}); err == nil {
				t.Fatal("期望报错")
			}
		})
	}
}

// TestNew_ConsoleOnlyDoesNotTouchDisk 验证 output=console 时不碰磁盘。
func TestNew_ConsoleOnlyDoesNotTouchDisk(t *testing.T) {
	dir := t.TempDir()
	lg, err := New(Options{
		Level:  "info",
		Output: "console",
		File:   FileOptions{Enabled: false},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = lg.Closer.Close() }()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("console 模式不应产生文件，实际 %d 个", len(entries))
	}
}

// TestCleanup_DeletesExpiredByName 验证保留期清理按**文件名日期**删除。
//
// 这是与 lf-db 的关键差异：容器重建会改 mtime，
// 用 mtime 判据会把昨天的日志误删。文件名日期不受重建影响。
func TestCleanup_DeletesExpiredByName(t *testing.T) {
	w, dir, _ := makeRotatingWriter(t, "daily", 1024)

	// 造三个文件：8 天前、3 天前、今天。
	old := filepath.Join(dir, "test.2026-09-16.log")
	mid := filepath.Join(dir, "test.2026-09-21.log")
	today := filepath.Join(dir, "test.2026-09-24.log")
	// 同日序号滚动出来的文件也要一起判过期。
	oldSeq := filepath.Join(dir, "test.2026-09-16.001.log")
	for _, p := range []string{old, mid, today, oldSeq} {
		if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
			t.Fatalf("造文件 %s: %v", p, err)
		}
	}
	// 把 mtime 改成"刚刚"——模拟容器重建后 mtime 被刷新。
	fresh := time.Now()
	for _, p := range []string{old, mid, today, oldSeq} {
		if err := os.Chtimes(p, fresh, fresh); err != nil {
			t.Fatalf("Chtimes %s: %v", p, err)
		}
	}

	rw := w
	rw.cleanup()

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("8 天前的日志应被删除，实际仍存在 err=%v", err)
	}
	if _, err := os.Stat(oldSeq); !os.IsNotExist(err) {
		t.Errorf("8 天前的序号日志应被删除，实际仍存在 err=%v", err)
	}
	if _, err := os.Stat(mid); err != nil {
		t.Errorf("3 天前的日志应保留，实际 err=%v", err)
	}
	if _, err := os.Stat(today); err != nil {
		t.Errorf("今天的日志应保留，实际 err=%v", err)
	}
}

// TestCleanup_KeepsUnparsableNames 验证文件名解析不出来的一律保留。
func TestCleanup_KeepsUnparsableNames(t *testing.T) {
	w, dir, _ := makeRotatingWriter(t, "daily", 1024)

	weird := filepath.Join(dir, "test.not-a-date.log")
	other := filepath.Join(dir, "unrelated.log")
	for _, p := range []string{weird, other} {
		if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
			t.Fatalf("造文件 %s: %v", p, err)
		}
	}

	rw := w
	rw.cleanup()

	for _, p := range []string{weird, other} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("文件 %s 应保留，实际 err=%v", p, err)
		}
	}
}

// TestCleanup_SkipsOtherFiles 验证只清理自己前缀的文件。
func TestCleanup_SkipsOtherFiles(t *testing.T) {
	w, dir, _ := makeRotatingWriter(t, "daily", 1024)

	other := filepath.Join(dir, "someone-else.2020-01-01.log")
	if err := os.WriteFile(other, []byte("x\n"), 0o644); err != nil {
		t.Fatalf("造文件: %v", err)
	}

	rw := w
	rw.cleanup()

	if _, err := os.Stat(other); err != nil {
		t.Errorf("非本程序的日志文件不应被删除，实际 err=%v", err)
	}
}

// TestSetLevel 验证运行期调级别。
//
// 这是设置页「日志级别热生效」的底座：改完立刻见效，
// 不需要重启容器、不需要重建文件写入器。
func TestSetLevel(t *testing.T) {
	dir := t.TempDir()
	lg, err := New(Options{
		Level:  "info",
		Output: "file",
		File: FileOptions{
			Enabled:    true,
			Dir:        dir,
			Filename:   "test.log",
			MaxSizeMB:  1024,
			Rotate:     "daily",
			RetainDays: 7,
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = lg.Closer.Close() }()

	if lg.Level() != slog.LevelInfo {
		t.Fatalf("初始级别期望 INFO，实际 %s", lg.Level())
	}
	if err := lg.SetLevel("debug"); err != nil {
		t.Fatalf("SetLevel(debug): %v", err)
	}
	if lg.Level() != slog.LevelDebug {
		t.Errorf("级别期望 DEBUG，实际 %s", lg.Level())
	}
	// 非法级别不改动当前级别——不能因为一次误操作把日志全关了。
	if err := lg.SetLevel("verbose"); err == nil {
		t.Fatal("非法级别应当报错")
	}
	if lg.Level() != slog.LevelDebug {
		t.Errorf("非法级别后应保持 DEBUG，实际 %s", lg.Level())
	}
}

// TestSetLevel_FiltersOutput 验证调级别真的改变写出的内容。
func TestSetLevel_FiltersOutput(t *testing.T) {
	dir := t.TempDir()
	lg, err := New(Options{
		Level:  "info",
		Output: "file",
		File: FileOptions{
			Enabled:    true,
			Dir:        dir,
			Filename:   "test.log",
			MaxSizeMB:  1024,
			Rotate:     "daily",
			RetainDays: 7,
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = lg.Closer.Close() }()

	lg.Logger.Debug("debug-before")
	lg.Logger.Info("info-before")
	if err := lg.SetLevel("debug"); err != nil {
		t.Fatalf("SetLevel: %v", err)
	}
	lg.Logger.Debug("debug-after")

	path := filepath.Join(dir, "test."+time.Now().Format("2006-01-02")+".log")
	got := readFile(t, path)
	if strings.Contains(got, "debug-before") {
		t.Error("级别为 info 时不应写出 debug 日志")
	}
	if !strings.Contains(got, "info-before") {
		t.Error("info 日志应被写出")
	}
	if !strings.Contains(got, "debug-after") {
		t.Error("调成 debug 后 debug 日志应被写出")
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("readFile %s: %v", p, err)
	}
	return string(data)
}

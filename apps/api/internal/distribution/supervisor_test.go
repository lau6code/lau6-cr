package distribution

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// TestMain 让测试二进制能冒充 distribution 子进程。
//
// supervisor 用 `exec.Command(bin, "serve", config)` 拉子进程，
// 所以这里把测试二进制自己当那个 bin：带上环境变量时它就起一个
// 只答 /v2/ 的最小 HTTP 服务。
func TestMain(m *testing.M) {
	if addr := os.Getenv(fakeDistAddrEnv); addr != "" {
		runFakeDistribution(addr)
		return
	}
	os.Exit(m.Run())
}

const (
	fakeDistAddrEnv  = "LAU6_CR_FAKE_DIST_ADDR"
	fakeDistExitEnv  = "LAU6_CR_FAKE_DIST_EXIT_MS"
	fakeDistLinesEnv = "LAU6_CR_FAKE_DIST_LINES"
)

// runFakeDistribution 起一个只答 /v2/ 的假 registry。
//
// 设了 fakeDistLinesEnv 就在起服务前往 stdout/stderr 各写若干行——
// 用来验证"子进程的输出有没有被完整转进日志"。
// 设了 fakeDistExitEnv 就在那之后自己退出——用来模拟"distribution 自己崩了"。
func runFakeDistribution(addr string) {
	if raw := os.Getenv(fakeDistLinesEnv); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			for i := 0; i < n; i++ {
				fmt.Fprintf(os.Stdout, "stdout-line-%d\n", i)
				fmt.Fprintf(os.Stderr, "stderr-line-%d\n", i)
			}
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := &http.Server{Addr: addr, Handler: mux}
	if ms := os.Getenv(fakeDistExitEnv); ms != "" {
		if n, err := strconv.Atoi(ms); err == nil {
			go func() {
				time.Sleep(time.Duration(n) * time.Millisecond)
				_ = srv.Close()
			}()
		}
	}
	_ = srv.ListenAndServe()
	if os.Getenv(fakeDistExitEnv) != "" {
		// 模拟"崩了"：非零退出码，上层能拿到退出错误。
		os.Exit(1)
	}
}

// logCollector 收集日志记录，供断言用。
//
// delay 是每条记录的模拟耗时：日志写得慢，才会让"读管道"和
// "子进程退出"撞上——这正是尾部日志被吃掉的那个窗口。
type logCollector struct {
	delay time.Duration

	mu      sync.Mutex
	records []logRecord
}

type logRecord struct {
	level slog.Level
	msg   string
	attrs map[string]any
}

func (c *logCollector) Enabled(context.Context, slog.Level) bool { return true }

func (c *logCollector) Handle(_ context.Context, r slog.Record) error {
	attrs := map[string]any{}
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.Any()
		return true
	})
	c.mu.Lock()
	c.records = append(c.records, logRecord{level: r.Level, msg: r.Message, attrs: attrs})
	c.mu.Unlock()
	if c.delay > 0 {
		time.Sleep(c.delay)
	}
	return nil
}

func (c *logCollector) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *logCollector) WithGroup(string) slog.Handler      { return c }

// messages 返回已收集的日志文案。
func (c *logCollector) messages() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.records))
	for _, r := range c.records {
		out = append(out, r.msg)
	}
	return out
}

// componentOf 返回某条日志的 component 属性。
func (c *logCollector) componentOf(msg string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.records {
		if r.msg == msg {
			v, ok := r.attrs["component"]
			return v, ok
		}
	}
	return nil, false
}

// freeAddr 拿一个空闲端口。
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("申请空闲端口: %v", err)
	}
	defer l.Close()
	return l.Addr().String()
}

// newTestSupervisor 造一个指向假 distribution 的 supervisor。
//
// logger 传 nil 时用 slog.Default()。
func newTestSupervisor(t *testing.T, addr string, logger *slog.Logger) *Supervisor {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "distribution.yml")
	if err := os.WriteFile(cfg, []byte("version: 0.1\n"), 0o600); err != nil {
		t.Fatalf("写配置: %v", err)
	}
	return NewSupervisor(SupervisorOptions{
		Bin:          os.Args[0], // 测试二进制自己
		ConfigPath:   cfg,
		ListenAddr:   addr,
		Logger:       logger,
		ReadyTimeout: 10 * time.Second,
		StopTimeout:  5 * time.Second,
	})
}

// TestSupervisorRestartKeepsDone 覆盖"计划内的重启不算 distribution 挂了"。
//
// ⚠️ 这条要是破了，后果是"重载一次配置 = 整个实例自杀"：
//
//	Restart 会先停旧进程，旧进程退出时若把 Done 关了，
//	上层（cmd/lau6-cr）就以为 distribution 崩了，把实例一起带走。
//	配置重载（setup 提交、设置页保存）每次都会踩到。
func TestSupervisorRestartKeepsDone(t *testing.T) {
	addr := freeAddr(t)
	t.Setenv(fakeDistAddrEnv, addr)

	sup := newTestSupervisor(t, addr, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := sup.Start(ctx); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	t.Cleanup(func() { _ = sup.Stop(context.Background()) })

	if err := sup.Restart(ctx); err != nil {
		t.Fatalf("重启失败: %v", err)
	}

	select {
	case <-sup.Done():
		t.Fatal("计划内重启不该关闭 Done()——上层会误判成 distribution 崩了")
	default:
	}

	// 重启后新进程确实在服务。
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + addr + "/v2/")
	if err != nil {
		t.Fatalf("重启后探活失败: %v", err)
	}
	_ = resp.Body.Close()
}

// TestSupervisorUnexpectedExitClosesDone 覆盖"distribution 自己崩了要通知上层"。
//
// 不通知 = 页面一切正常、push/pull 全失败。
func TestSupervisorUnexpectedExitClosesDone(t *testing.T) {
	addr := freeAddr(t)
	t.Setenv(fakeDistAddrEnv, addr)
	t.Setenv(fakeDistExitEnv, "300")

	sup := newTestSupervisor(t, addr, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := sup.Start(ctx); err != nil {
		t.Fatalf("启动失败: %v", err)
	}

	select {
	case <-sup.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("distribution 自己退出后 Done() 应当关闭")
	}
	if sup.ExitError() == nil {
		t.Error("ExitError 应当给出子进程的退出错误")
	}
}

// TestSupervisorForwardsAllChildOutput 覆盖"子进程的输出必须一条不丢地转进日志"。
//
// ⚠️ 子进程退出时最容易丢日志：
//
//	exec.Cmd.Wait 在子进程退出后会关闭管道的读端，
//	而转发 goroutine 还在读——读失败的那一段（往往是最后几行，
//	也就是"崩了的原因"）就没了，而且没有任何痕迹。
//
//	这里的日志器故意写得慢（每条 2ms），把那个窗口撑开。
func TestSupervisorForwardsAllChildOutput(t *testing.T) {
	const lines = 100

	addr := freeAddr(t)
	t.Setenv(fakeDistAddrEnv, addr)
	t.Setenv(fakeDistLinesEnv, strconv.Itoa(lines))
	t.Setenv(fakeDistExitEnv, "300")

	collector := &logCollector{delay: 5 * time.Millisecond}
	sup := newTestSupervisor(t, addr, slog.New(collector))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := sup.Start(ctx); err != nil {
		t.Fatalf("启动失败: %v", err)
	}

	select {
	case <-sup.Done():
	case <-time.After(20 * time.Second):
		t.Fatal("假 distribution 应当已经退出")
	}

	got := collector.messages()
	for i := 0; i < lines; i++ {
		want := fmt.Sprintf("distribution: stdout-line-%d", i)
		if !contains(got, want) {
			t.Errorf("stdout 的第 %d 行没被转进日志（缺 %q）", i, want)
		}
		want = fmt.Sprintf("distribution: stderr-line-%d", i)
		if !contains(got, want) {
			t.Errorf("stderr 的第 %d 行没被转进日志（缺 %q）", i, want)
		}
	}

	// 日志前缀与 component 字段是排障指引的依赖（见 05-build-deploy.md）。
	comp, ok := collector.componentOf("distribution: stdout-line-0")
	if !ok {
		t.Fatal("转发的日志应当带 component 属性")
	}
	if comp != "distribution" {
		t.Errorf("component 属性 = %v，期望 distribution", comp)
	}
}

// contains 判断切片里有没有这个元素。
func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

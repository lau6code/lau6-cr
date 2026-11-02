package distribution

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// SupervisorOptions 是内嵌 distribution 进程的配置。
type SupervisorOptions struct {
	// Bin 是 distribution 可执行文件的路径。
	Bin string
	// ConfigPath 是渲染出来的 distribution 配置文件路径。
	ConfigPath string
	// ListenAddr 是 distribution 的监听地址（host:port，不带 scheme）。
	ListenAddr string
	// Logger 是日志器——子进程的输出转到这里。
	Logger *slog.Logger
	// ReadyTimeout 是等待子进程就绪的最长时间（默认 30s）。
	ReadyTimeout time.Duration
	// StopTimeout 是等待子进程优雅退出的最长时间（默认 10s）。
	StopTimeout time.Duration
}

// Supervisor 管理内嵌 distribution 子进程的生命周期。
//
// 它做三件事：
//
//  1. 拉起 distribution（serve <config>）；
//  2. 等它真正能服务（轮询 /v2/），而不是"进程起来了就算好"；
//  3. 在 lau6-cr 退出时把它停掉；它自己挂了就通知上层。
//
// ⚠️ 子进程退出**不会**被吞掉：Done() 会关闭，
//
//	上层（cmd/lau6-cr）据此让整个实例退出——
//	distribution 挂了还继续对外服务 = "看起来正常、实际推拉全失败"。
//
// ⚠️ 但**计划内的重启不算"挂了"**：Restart() 会先停旧进程再拉新进程，
//
//	旧进程退出时 Done() 不能关——否则上层会以为 distribution 崩了，
//	把整个实例带走（配置重载一次 = 实例自杀）。
//	所以 Done() 只在**非计划**退出（自己崩了）时关闭。
type Supervisor struct {
	opts SupervisorOptions

	mu       sync.Mutex
	cmd      *exec.Cmd
	exitErr  error
	stopping bool
	// dead 标记 Done() 是否已经关闭过（避免重复 close  panic）。
	dead bool

	// done 是**整个 supervisor 生命周期**的信号：
	// 只在 distribution 非计划退出时关闭一次。
	done chan struct{}
	// procDone 是**单个进程**的信号：进程退出就关闭（不管是不是计划内的）。
	// waitReady / Stop 等的是它。
	procDone chan struct{}
}

// NewSupervisor 构造一个 Supervisor（不启动进程）。
func NewSupervisor(opts SupervisorOptions) *Supervisor {
	if opts.ReadyTimeout <= 0 {
		opts.ReadyTimeout = 30 * time.Second
	}
	if opts.StopTimeout <= 0 {
		opts.StopTimeout = 10 * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Supervisor{opts: opts, done: make(chan struct{})}
}

// Start 拉起 distribution 并等它就绪。
//
// 就绪判据是 HTTP 层能应答（/v2/ 有响应就算起来了；
// 没带令牌时它回 401，那是正常的鉴权响应，说明服务在跑）。
func (s *Supervisor) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.cmd != nil {
		s.mu.Unlock()
		return fmt.Errorf("distribution 已经在运行")
	}
	s.mu.Unlock()

	if err := s.checkBinary(); err != nil {
		return err
	}

	cmd := exec.Command(s.opts.Bin, "serve", s.opts.ConfigPath)
	// 独立进程组：停止时能连它自己 fork 出来的子进程一起收掉，
	// 也不至于被父进程组之外的信号误伤。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = childEnv()
	// ⚠️ 用 Writer 接输出，不走 StdoutPipe：
	//
	//	Wait 会在子进程退出后关闭管道的读端（os/exec 的 closeDescriptors），
	//	而转发 goroutine 还在读——读失败的那一段（往往就是最后几行，
	//	也就是"崩了的原因"）就凭空消失了，而且没有任何痕迹。
	//	交给 exec 自己的拷贝 goroutine 搬运，Wait 会等它读完再关管道。
	stdoutFwd := newLogForwarder(s.opts.Logger, "distribution")
	stderrFwd := newLogForwarder(s.opts.Logger, "distribution")
	cmd.Stdout = stdoutFwd
	cmd.Stderr = stderrFwd
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 distribution (%s): %w", s.opts.Bin, err)
	}

	s.mu.Lock()
	s.cmd = cmd
	s.exitErr = nil
	s.stopping = false
	procDone := make(chan struct{})
	s.procDone = procDone
	s.mu.Unlock()

	go func() {
		waitErr := cmd.Wait()
		// Wait 返回 = exec 的拷贝 goroutine 已经把子进程的输出搬完，
		// 这时才轮到补最后一行（子进程可能没写换行就退出了）。
		stdoutFwd.flush()
		stderrFwd.flush()

		// cmd.Wait() 的错误有两个来源：子进程的退出码，
		// 以及 exec 拷贝 goroutine 的读错误（子进程正常退出时优先报后者）。
		// 两者性质完全不同：前者是"distribution 挂了"，
		// 后者是"日志没搬完"——混在一起报，排障时会被带偏。
		exitErr := waitErr
		if cmd.ProcessState != nil && cmd.ProcessState.Success() {
			exitErr = nil
			if waitErr != nil {
				s.opts.Logger.Error("读取 distribution 输出失败，distribution 的日志可能不完整",
					"err", waitErr)
			}
		}

		s.mu.Lock()
		s.exitErr = exitErr
		s.cmd = nil
		// 计划内的停止（重启 / 实例退出）不算"挂了"。
		planned := s.stopping
		s.mu.Unlock()
		close(procDone)
		if !planned {
			s.markDead()
		}
	}()

	if err := s.waitReady(ctx, procDone); err != nil {
		// 起不来就必须把子进程收掉——留一个半死不活的进程占着端口，
		// 下次重启就报 "address already in use"，排查起来莫名其妙。
		if stopErr := s.Stop(context.Background()); stopErr != nil {
			s.opts.Logger.Error("停止失败的 distribution 进程时出错", "err", stopErr)
		}
		// 起不来 = distribution 没有在跑。上层必须知道：
		// 继续对外服务 = 页面一切正常、push/pull 全失败。
		s.markDead()
		return err
	}
	s.opts.Logger.Info("内嵌 distribution 已就绪",
		"addr", s.opts.ListenAddr, "bin", s.opts.Bin, "config", s.opts.ConfigPath)
	return nil
}

// markDead 标记 supervisor 已经完了（distribution 不再提供服务）。
//
// 只关一次：重复 close 会 panic。
func (s *Supervisor) markDead() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dead {
		return
	}
	s.dead = true
	close(s.done)
}

// childEnv 构造子进程的环境变量。
//
// 默认继承父进程环境；再补一条 OTEL_TRACES_EXPORTER=none：
// distribution 3.x **无条件**初始化 OpenTelemetry，默认用 OTLP 往
// localhost:4318 发 trace —— 没人监听时它每批都报一次
// "connection refused"，把日志刷满。
//
// 已经显式设过就不覆盖：运维要接自己的 collector 时以他们的为准。
func childEnv() []string {
	env := os.Environ()
	for _, e := range env {
		if strings.HasPrefix(e, "OTEL_TRACES_EXPORTER=") {
			return env
		}
	}
	return append(env, "OTEL_TRACES_EXPORTER=none")
}

// checkBinary 检查可执行文件存在且可执行。
//
// 缺文件是最常见的部署事故（镜像没更新、路径写错、卷把二进制盖住了）。
// 必须在启动前说清楚，不能等 exec 报一句 "no such file or directory"。
func (s *Supervisor) checkBinary() error {
	if s.opts.Bin == "" {
		return fmt.Errorf("distribution 可执行文件路径为空")
	}
	fi, err := os.Stat(s.opts.Bin)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("找不到 distribution 可执行文件 %s"+
				"（内嵌模式需要它；若 distribution 是独立部署的，请设 CR_REGISTRY_MODE=external）", s.opts.Bin)
		}
		return fmt.Errorf("检查 distribution 可执行文件 %s: %w", s.opts.Bin, err)
	}
	if fi.IsDir() || fi.Mode()&0o111 == 0 {
		return fmt.Errorf("distribution 可执行文件 %s 不可执行", s.opts.Bin)
	}
	return nil
}

// waitReady 轮询 /v2/，等 distribution 真的能应答。
//
// ⚠️ 子进程中途退出时立刻返回错误——
//
//	"进程起来了但马上崩了"绝不能被当成"还在启动中"。
func (s *Supervisor) waitReady(ctx context.Context, done <-chan struct{}) error {
	client := &http.Client{Timeout: 3 * time.Second}
	url := "http://" + s.opts.ListenAddr + "/v2/"
	deadline := time.Now().Add(s.opts.ReadyTimeout)
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("等待 distribution 就绪被取消: %w", ctx.Err())
		case <-done:
			return fmt.Errorf("distribution 启动后立即退出: %w", s.ExitError())
		default:
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return fmt.Errorf("构造 distribution 探活请求: %w", err)
		}
		resp, err := client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode < 500 {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("等待 distribution 就绪超时（%s 无响应）", url)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("等待 distribution 就绪被取消: %w", ctx.Err())
		case <-done:
			return fmt.Errorf("distribution 启动后立即退出: %w", s.ExitError())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// Restart 停掉旧进程并重新拉起（配置变了时用）。
//
// ⚠️ distribution **不支持热加载配置**（3.x 没有 SIGHUP 重载），
//
//	所以配置变更只能重启子进程。
func (s *Supervisor) Restart(ctx context.Context) error {
	if err := s.Stop(ctx); err != nil {
		return err
	}
	// Start 会重新开一个 done 通道（旧的那个已经关闭）。
	return s.Start(ctx)
}

// Stop 停止子进程（先 SIGTERM，超时再 SIGKILL）。
//
// 停止失败必须报错——调用方要知道自己没能干净地收掉子进程。
func (s *Supervisor) Stop(ctx context.Context) error {
	s.mu.Lock()
	cmd := s.cmd
	procDone := s.procDone
	if cmd == nil || cmd.Process == nil {
		s.mu.Unlock()
		return nil
	}
	s.stopping = true
	s.mu.Unlock()

	pgid := cmd.Process.Pid
	// 负 pid = 整个进程组。
	if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil {
		// 进程可能已经自己退了：这不是错误，但要记下来。
		s.opts.Logger.Warn("向 distribution 发送 SIGTERM 失败", "err", err)
	}
	select {
	case <-procDone:
		return nil
	case <-ctx.Done():
		s.opts.Logger.Warn("distribution 未在超时内退出，强制结束",
			"timeout", s.opts.StopTimeout)
		if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil {
			return fmt.Errorf("强制结束 distribution 进程: %w", err)
		}
		return nil
	case <-time.After(s.opts.StopTimeout):
		s.opts.Logger.Warn("distribution 未在超时内退出，强制结束",
			"timeout", s.opts.StopTimeout)
		if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil {
			return fmt.Errorf("强制结束 distribution 进程: %w", err)
		}
		return nil
	}
}

// Done 在 distribution **非计划**退出时关闭。
//
// 上层用它发现"distribution 挂了"——不能装作没事继续跑。
//
// ⚠️ 计划内的重启（Restart）**不关**它：配置重载一次不等于 distribution 崩了，
//
//	关了上层就会把整个实例带走。
func (s *Supervisor) Done() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.done
}

// ExitError 返回子进程的退出错误（还没退出时返回 nil）。
func (s *Supervisor) ExitError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exitErr
}

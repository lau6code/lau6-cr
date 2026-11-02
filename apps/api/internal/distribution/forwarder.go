package distribution

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
)

// maxForwardLineBytes 是单行转发的上限。
//
// 上游一行写得过长不是错误，但也不能无限缓冲——
// 一次 100MB 的单行会把内存吃穿。所以超限的行保留前
// maxForwardLineBytes 字节照常转进日志，其余部分丢弃，
// 并且**必须留痕**（reportDropped）：悄悄丢掉一整行，
// 和当初"悄悄丢掉整条转发"是同一类事故。
const maxForwardLineBytes = 1 << 20 // 1MB

// logForwarder 把子进程的输出按行转进 lau6-cr 的日志。
//
// 不转的话，容器的日志采集器只会看到 lau6-cr 自己的输出，
// distribution 的报错就永远看不见了。
//
// ⚠️ 它是 io.Writer 而不是"读管道 + bufio.Scanner"，
//
//	因为 Wait 会在子进程退出后关闭管道的读端，
//	Scanner 读到那儿会撞上 "file already closed"，
//	尾部日志（往往正是崩溃原因）就此消失。
//	由 exec 的拷贝 goroutine 搬运时，Wait 会等它读完，
//	搬运失败也会从 Wait 的返回值里出来——不会静默。
type logForwarder struct {
	mu  sync.Mutex
	buf []byte
	tag string
	// dropped 是当前这一行已丢弃的字节数（>0 表示正在丢弃超长行）。
	dropped int

	logger *slog.Logger
}

// newLogForwarder 造一个把子进程输出转进日志的写入器。
func newLogForwarder(logger *slog.Logger, tag string) *logForwarder {
	return &logForwarder{logger: logger, tag: tag}
}

// Write 接收子进程输出的一批字节，按行切分后转进日志。
//
// 永远返回 (len(p), nil)：返回错误会让 exec 干掉子进程，
// 而"日志切分失败"不该要了 distribution 的命。
func (f *logForwarder) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := len(p)
	for len(p) > 0 {
		if f.dropped > 0 {
			// 上一行超了上限，正在丢弃它的剩余部分：先找到行尾。
			idx := bytes.IndexByte(p, '\n')
			if idx < 0 {
				f.dropped += len(p)
				break
			}
			f.dropped += idx
			p = p[idx+1:]
			f.reportDropped()
			f.emitLine()
			continue
		}
		idx := bytes.IndexByte(p, '\n')
		if idx < 0 {
			f.append(p)
			break
		}
		f.append(p[:idx])
		p = p[idx+1:]
		if f.dropped > 0 {
			f.reportDropped()
		}
		f.emitLine()
	}
	return n, nil
}

// append 把一段内容接进当前行；超上限的部分丢弃并记账。
func (f *logForwarder) append(b []byte) {
	room := maxForwardLineBytes - len(f.buf)
	if room <= 0 {
		f.dropped += len(b)
		return
	}
	if len(b) > room {
		f.buf = append(f.buf, b[:room]...)
		f.dropped += len(b) - room
		return
	}
	f.buf = append(f.buf, b...)
}

// emitLine 把当前攒的一行转进日志并清空缓冲。
func (f *logForwarder) emitLine() {
	if len(f.buf) == 0 {
		return
	}
	line := strings.TrimRight(string(f.buf), "\r")
	f.buf = f.buf[:0]
	if line == "" {
		return
	}
	f.logger.Info("distribution: "+line, "component", f.tag)
}

// reportDropped 记一条"这一行太长被截断"的日志。
func (f *logForwarder) reportDropped() {
	f.logger.Error("distribution 输出的单行超过上限，已截断",
		"dropped_bytes", f.dropped, "limit_bytes", maxForwardLineBytes, "component", f.tag)
	f.dropped = 0
}

// flush 把没以换行结尾的残留行补进日志。
//
// 子进程可能没写换行就退出了（panic 栈的最后一行、被截断的日志文件……），
// 那段内容不能因为"没有行尾"就丢掉。
func (f *logForwarder) flush() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dropped > 0 {
		f.reportDropped()
	}
	f.emitLine()
}

package distribution

import (
	"log/slog"
	"strings"
	"testing"
)

// TestLogForwarderSplitsLines 覆盖"按行切分 + 前缀与 component 字段"。
//
// 前缀 "distribution: " 是排障指引的依赖（见 05-build-deploy.md），
// 改了它，`docker logs lau6-cr | grep distribution` 就抓不到东西。
func TestLogForwarderSplitsLines(t *testing.T) {
	collector := &logCollector{}
	fwd := newLogForwarder(slog.New(collector), "distribution")

	// 故意在行中间切断，模拟管道里读到的碎片。
	for _, chunk := range []string{"first line\nsec", "ond line\n\nthird", " line\n"} {
		n, err := fwd.Write([]byte(chunk))
		if err != nil {
			t.Fatalf("Write 返回错误: %v", err)
		}
		if n != len(chunk) {
			t.Fatalf("Write 消费了 %d 字节，期望 %d", n, len(chunk))
		}
	}
	fwd.flush()

	got := collector.messages()
	want := []string{
		"distribution: first line",
		"distribution: second line",
		"distribution: third line",
	}
	for _, w := range want {
		if !contains(got, w) {
			t.Errorf("缺少日志 %q，实际有 %v", w, got)
		}
	}
	// 空行不该进日志。
	if contains(got, "distribution: ") {
		t.Errorf("空行不该被转进日志，实际有 %v", got)
	}
}

// TestLogForwarderTruncatesOverlongLine 覆盖"单行超上限"。
//
// ⚠️ 超长行不能把整条转发打死：
//
//	上游一行写太长是它自己的毛病，不该让我们从此看不见后面的日志。
//	但截断必须留痕——悄悄丢掉一整行等于制造新的"日志凭空消失"。
func TestLogForwarderTruncatesOverlongLine(t *testing.T) {
	collector := &logCollector{}
	fwd := newLogForwarder(slog.New(collector), "distribution")

	// 分两次写，跨过上限——确认跨多次 Write 的超长行也能正确记账。
	half := maxForwardLineBytes / 2
	if _, err := fwd.Write([]byte(strings.Repeat("x", half))); err != nil {
		t.Fatalf("Write 返回错误: %v", err)
	}
	tail := strings.Repeat("x", half+100) + "\nafter-long-line\n"
	if _, err := fwd.Write([]byte(tail)); err != nil {
		t.Fatalf("Write 返回错误: %v", err)
	}
	fwd.flush()

	got := collector.messages()
	if !contains(got, "distribution: after-long-line") {
		t.Errorf("超长行之后的日志仍要转发，实际有 %v", got)
	}
	var reported bool
	for _, msg := range got {
		if strings.Contains(msg, "单行超过上限") {
			reported = true
		}
	}
	if !reported {
		t.Errorf("截断必须留痕，实际日志 %v", got)
	}
}

// TestLogForwarderFlushKeepsTrailingLine 覆盖"子进程没写换行就退出"。
//
// 那种情况下最后一段内容没有行尾，但它是真实内容，不能丢。
func TestLogForwarderFlushKeepsTrailingLine(t *testing.T) {
	collector := &logCollector{}
	fwd := newLogForwarder(slog.New(collector), "distribution")

	if _, err := fwd.Write([]byte("no trailing newline")); err != nil {
		t.Fatalf("Write 返回错误: %v", err)
	}
	if contains(collector.messages(), "distribution: no trailing newline") {
		t.Error("还没 flush 就不该转进日志")
	}
	fwd.flush()

	if !contains(collector.messages(), "distribution: no trailing newline") {
		t.Errorf("flush 应当补上末尾的残留行，实际有 %v", collector.messages())
	}
}

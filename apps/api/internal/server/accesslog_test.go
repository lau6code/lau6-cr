package server

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestLogger 造一个写进内存 buffer 的日志器。
func newTestLogger() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

// logRecords 把日志 buffer 解析成结构化记录。
func logRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	out := []map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("日志不是合法 JSON: %v（行=%s）", err, line)
		}
		out = append(out, rec)
	}
	return out
}

// TestAccessLogRecordsOutcome 锁定访问日志的三档级别。
//
// 没有访问日志，"客户端报 unauthorized 但后端一片安静"就没法查：
// 4xx 必须落在 warn、5xx 必须落在 error，否则默认 info 级别下看不见。
func TestAccessLogRecordsOutcome(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		wantMsg   string
		wantLevel string
	}{
		{"成功", http.StatusOK, "请求完成", "INFO"},
		{"被拒", http.StatusUnauthorized, "请求被拒绝", "WARN"},
		{"服务端错误", http.StatusInternalServerError, "请求处理失败", "ERROR"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logger, buf := newTestLogger()
			app := &App{Logger: logger}

			h := app.accessLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
			}))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v2/team-a/app/manifests/v1", nil))

			recs := logRecords(t, buf)
			if len(recs) != 1 {
				t.Fatalf("日志条数 = %d，期望 1 条", len(recs))
			}
			got := recs[0]
			if got["msg"] != tc.wantMsg {
				t.Fatalf("msg = %v，期望 %q", got["msg"], tc.wantMsg)
			}
			if got["level"] != tc.wantLevel {
				t.Fatalf("level = %v，期望 %q", got["level"], tc.wantLevel)
			}
			if got["status"] != float64(tc.status) {
				t.Fatalf("status = %v，期望 %d", got["status"], tc.status)
			}
			if got["path"] != "/v2/team-a/app/manifests/v1" {
				t.Fatalf("path = %v", got["path"])
			}
			if _, ok := got["request_id"]; !ok {
				t.Fatalf("日志缺少 request_id：%v", got)
			}
		})
	}
}

// TestAccessLogSkipsProbePaths 保证探活不打日志。
//
// 探活每 30s 一次，记下来会把真正的日志淹掉。
func TestAccessLogSkipsProbePaths(t *testing.T) {
	logger, buf := newTestLogger()
	app := &App{Logger: logger}

	h := app.accessLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	for _, p := range []string{"/healthz", "/readyz"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
	}
	if buf.Len() != 0 {
		t.Fatalf("探活路径不该记访问日志，实际内容：%s", buf.String())
	}
}

// TestAccessLogKeepsFlusher 保证包装后的 ResponseWriter 仍然能 Flush。
//
// blob 是**流式**转发的：包装器丢了 Flusher，几 GiB 的上传会被缓冲住。
func TestAccessLogKeepsFlusher(t *testing.T) {
	logger, _ := newTestLogger()
	app := &App{Logger: logger}

	flushed := false
	h := app.accessLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			t.Fatalf("包装后的 ResponseWriter 不再实现 http.Flusher")
		}
		f.Flush()
		flushed = true
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v2/x/blobs/uploads/", nil))
	if !flushed {
		t.Fatalf("Flush 没有生效")
	}
}

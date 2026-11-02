package config

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// TestWarnDeprecatedEnv_NamesSetVar 验证：环境里还留着的已废弃变量会被点名。
//
// 不点名 = 老 compose 里的 CR_LOG_LEVEL=debug 还在，人却以为日志级别还是 debug。
func TestWarnDeprecatedEnv_NamesSetVar(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	t.Setenv("CR_LOG_LEVEL", "debug")
	WarnDeprecatedEnv(logger)

	out := buf.String()
	if !strings.Contains(out, "CR_LOG_LEVEL") {
		t.Fatalf("应当点名 CR_LOG_LEVEL，实际输出: %s", out)
	}
	if !strings.Contains(out, "系统设置") {
		t.Errorf("应当说明去处（系统设置），实际输出: %s", out)
	}
}

// TestWarnDeprecatedEnv_IgnoresEmpty 验证：空值不告警。
//
// 编排文件里的空占位（CR_LOG_LEVEL: "${CR_LOG_LEVEL:-}"）不是残留，
// 每次启动都报一遍只是噪音。口径与 applyEnv 一致（空串 == 未设置）。
func TestWarnDeprecatedEnv_IgnoresEmpty(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	t.Setenv("CR_LOG_LEVEL", "")
	WarnDeprecatedEnv(logger)

	if out := buf.String(); out != "" {
		t.Errorf("空值不该告警，实际输出: %s", out)
	}
}

// TestWarnDeprecatedEnv_NilLogger 验证：logger 为 nil 时不 panic。
func TestWarnDeprecatedEnv_NilLogger(t *testing.T) {
	WarnDeprecatedEnv(nil)
}

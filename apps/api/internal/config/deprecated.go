package config

import (
	"log/slog"
	"os"
	"strings"
)

// DeprecatedEnvVar 描述一个已废弃的环境变量。
type DeprecatedEnvVar struct {
	// Var 是环境变量名。
	Var string
	// Replacement 是它该去哪里设置——告警必须说清楚去处，
	// 只说"已废弃"的话，运维不知道该改哪里。
	Replacement string
}

// DeprecatedEnvVars 是已废弃、但老编排文件里可能还留着的环境变量。
//
// ⚠️ 这里列的都是**曾经**能用的：删掉之后它们不再被读取。
//
//	留着不提示 = 老 compose 里的 CR_LOG_LEVEL=debug 还在，
//	人却以为日志级别还是 debug（AGENTS.md 规则 1：失效必须让调用方看见）。
var DeprecatedEnvVars = []DeprecatedEnvVar{
	{Var: "CR_ADMIN_USERNAME", Replacement: "在 setup 界面输入管理员用户名"},
	{Var: "CR_ADMIN_PASSWORD", Replacement: "在 setup 界面输入管理员口令"},
	{Var: "CR_LOG_LEVEL", Replacement: "在系统设置 → 日志里改 logging.level"},
	{Var: "CR_LOG_OUTPUT", Replacement: "在系统设置 → 日志里改 logging.output"},
	{Var: "CR_LOG_FILE_ENABLED", Replacement: "在系统设置 → 日志里改 logging.file.enabled"},
	{Var: "CR_LOG_DIR", Replacement: "在系统设置 → 日志里改 logging.file.dir"},
	{Var: "CR_LOG_FILENAME", Replacement: "在系统设置 → 日志里改 logging.file.filename"},
	{Var: "CR_LOG_MAX_SIZE_MB", Replacement: "在系统设置 → 日志里改 logging.file.max_size_mb"},
	{Var: "CR_LOG_ROTATE", Replacement: "在系统设置 → 日志里改 logging.file.rotate"},
	{Var: "CR_LOG_RETAIN_DAYS", Replacement: "在系统设置 → 日志里改 logging.file.retain_days"},
}

// WarnDeprecatedEnv 逐条点名环境里仍然存在的已废弃变量。
//
// 只在变量**存在且非空**时报：编排文件里的空占位不算残留。
// 口径与 applyEnv 一致（那边也是"空串 == 未设置"，config.go:739-742）——
// 两处判定规则不能各写一份。
//
// ⚠️ **不阻断启动**：这些变量已经不参与生效，
// 为它们拒绝启动只会让老实例起不来。
func WarnDeprecatedEnv(logger *slog.Logger) {
	if logger == nil {
		return
	}
	for _, d := range DeprecatedEnvVars {
		v, ok := os.LookupEnv(d.Var)
		if !ok || strings.TrimSpace(v) == "" {
			continue
		}
		logger.Warn("环境变量已废弃，不再被读取",
			"var", d.Var, "replacement", d.Replacement)
	}
}

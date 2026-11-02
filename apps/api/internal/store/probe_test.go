package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestSQLDriverName 锁定「配置里的驱动名 → database/sql 注册的驱动名」映射。
//
// ⚠️ pgx 注册的名字是 "pgx"，不是 "postgres"。
// 少了这层映射，配了 postgres 的实例启动就会报
// "sql: unknown driver \"postgres\""。
func TestSQLDriverName(t *testing.T) {
	cases := map[string]string{
		"sqlite":   "sqlite",
		"postgres": "pgx",
		"mysql":    "mysql",
	}
	for driver, want := range cases {
		if got := sqlDriverName(driver); got != want {
			t.Errorf("sqlDriverName(%q) = %q, 期望 %q", driver, got, want)
		}
	}
}

// TestOpenPostgresDriverIsRegistered 锁定 Open 认识 postgres。
//
// 这里不指望连上——只锁定"驱动名能解析"，不能再退化成 unknown driver。
func TestOpenPostgresDriverIsRegistered(t *testing.T) {
	_, err := Open("postgres", "postgres://127.0.0.1:1/lau6_cr?sslmode=disable&connect_timeout=1", 1, 1)
	if err == nil {
		t.Fatal("连不上 127.0.0.1:1 本该报错")
	}
	if strings.Contains(err.Error(), "unknown driver") {
		t.Fatalf("驱动名没映射过去：%v", err)
	}
}

// TestProbeUnsupportedDriver 覆盖不支持的驱动 → 报错。
func TestProbeUnsupportedDriver(t *testing.T) {
	if _, err := Probe(context.Background(), "oracle", "whatever", ""); err == nil {
		t.Fatal("不支持的驱动应报错")
	}
}

// TestProbeSQLiteOK 覆盖 SQLite 探测能连上并建出文件。
func TestProbeSQLiteOK(t *testing.T) {
	path := filepath.Join(t.TempDir(), "probe.db")
	schema, err := Probe(context.Background(), "sqlite", "file:"+path, "")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if schema != "" {
		t.Fatalf("sqlite 的 schema = %q，期望空串", schema)
	}
}

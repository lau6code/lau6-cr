package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestDatabaseTargetPostgresRoundTrip 覆盖 postgres 的"拼 → 解 → 再拼"往返。
//
// 往返必须稳定：向导预填出来的值再提交一次，DSN 不能变。
// 否则用户点两次"测试连接"看到两个不同的连接串，没法判断哪个是对的。
func TestDatabaseTargetPostgresRoundTrip(t *testing.T) {
	orig := DatabaseTarget{
		Driver:   "postgres",
		Host:     "db.internal",
		Port:     "5432",
		Username: "lau6",
		Password: "p@ss:w/rd",
		Name:     "lau6cr",
		Schema:   "app",
		SSLMode:  "disable",
	}
	if err := orig.Normalize(); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	dsn, err := orig.DSN()
	if err != nil {
		t.Fatalf("DSN: %v", err)
	}
	if !strings.HasPrefix(dsn, "postgres://") {
		t.Fatalf("dsn = %q, 期望 postgres:// 开头", dsn)
	}
	// 口令里的 @ 与 / 必须被转义，否则 pgx 解析 URL 时会截断。
	if !strings.Contains(dsn, "p%40ss%3Aw%2Frd") {
		t.Fatalf("dsn = %q, 口令没有被正确转义", dsn)
	}

	back, err := ParseDatabaseTarget("postgres", dsn, "")
	if err != nil {
		t.Fatalf("ParseDatabaseTarget: %v", err)
	}
	if back.Host != orig.Host || back.Port != orig.Port ||
		back.Username != orig.Username || back.Password != orig.Password ||
		back.Name != orig.Name || back.Schema != orig.Schema {
		t.Fatalf("往返不一致:\n原 = %+v\n解 = %+v", orig, back)
	}
}

// TestDatabaseTargetPostgresDefaultPort 覆盖缺端口时补默认端口。
func TestDatabaseTargetPostgresDefaultPort(t *testing.T) {
	target := DatabaseTarget{
		Driver:   "postgres",
		Host:     "db.internal",
		Username: "lau6",
		Name:     "lau6cr",
	}
	if err := target.Normalize(); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if target.Port != DefaultPostgresPort {
		t.Fatalf("port = %q, 期望 %q", target.Port, DefaultPostgresPort)
	}
}

// TestDatabaseTargetMySQLRoundTrip 覆盖 mysql 的往返。
//
// ⚠️ mysql 的口令**不能**转义——go-sql-driver 不解转义序列，
// 转义反而会把口令改坏。这里锁住这个行为。
func TestDatabaseTargetMySQLRoundTrip(t *testing.T) {
	orig := DatabaseTarget{
		Driver:   "mysql",
		Host:     "db.internal",
		Port:     "3306",
		Username: "lau6",
		Password: "p@ss/w:rd",
		Name:     "lau6cr",
	}
	if err := orig.Normalize(); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	dsn, err := orig.DSN()
	if err != nil {
		t.Fatalf("DSN: %v", err)
	}
	if !strings.Contains(dsn, "@tcp(db.internal:3306)/") {
		t.Fatalf("dsn = %q, 期望 tcp 地址形式", dsn)
	}
	if strings.Contains(dsn, "%40") || strings.Contains(dsn, "%2F") {
		t.Fatalf("dsn = %q, mysql 口令不该被转义", dsn)
	}

	back, err := ParseDatabaseTarget("mysql", dsn, "")
	if err != nil {
		t.Fatalf("ParseDatabaseTarget: %v", err)
	}
	if back.Host != orig.Host || back.Port != orig.Port ||
		back.Username != orig.Username || back.Password != orig.Password ||
		back.Name != orig.Name {
		t.Fatalf("往返不一致:\n原 = %+v\n解 = %+v", orig, back)
	}
}

// TestDatabaseTargetNormalizeRejectsIncomplete 覆盖必填项缺失 → 报错。
func TestDatabaseTargetNormalizeRejectsIncomplete(t *testing.T) {
	cases := []struct {
		name   string
		target DatabaseTarget
	}{
		{"没有主机", DatabaseTarget{Driver: "postgres", Username: "u", Name: "n"}},
		{"没有用户名", DatabaseTarget{Driver: "postgres", Host: "h", Name: "n"}},
		{"没有库名", DatabaseTarget{Driver: "postgres", Host: "h", Username: "u"}},
		{"sqlite 没有路径", DatabaseTarget{Driver: "sqlite"}},
		{"驱动不认识", DatabaseTarget{Driver: "oracle", Host: "h"}},
	}
	for _, c := range cases {
		tgt := c.target
		if err := tgt.Normalize(); err == nil {
			t.Errorf("%s: 期望报错，实际 nil", c.name)
		}
	}
}

// TestSwitchToSwitchesConnection 覆盖运行期换库。
//
// 换完库之后：
//   - 新库能查到数据；
//   - 新库已经跑过迁移（表在）。
func TestSwitchToSwitchesConnection(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	first := filepath.Join(dir, "first.db")
	second := filepath.Join(dir, "second.db")

	db, err := Open("sqlite", "file:"+first, 1, 1)
	if err != nil {
		t.Fatalf("打开第一个库: %v", err)
	}
	defer db.Close()

	if err := db.SwitchTo(ctx, "sqlite", "file:"+second); err != nil {
		t.Fatalf("SwitchTo: %v", err)
	}

	// 新库必须是可用的、且已经建好表。
	var n int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n); err != nil {
		t.Fatalf("新库查询失败（迁移没跑？）: %v", err)
	}
	if n != 0 {
		t.Fatalf("新库 users 数量 = %d, 期望 0", n)
	}
	if db.Dialect() != DialectSQLite {
		t.Fatalf("dialect = %q, 期望 sqlite", db.Dialect())
	}
}

// TestSwitchToKeepsOldConnectionOnFailure 覆盖换库失败时旧连接原封不动。
//
// 换库失败绝不能把实例丢在"半切换"的状态上。
func TestSwitchToKeepsOldConnectionOnFailure(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	first := filepath.Join(dir, "first.db")

	db, err := Open("sqlite", "file:"+first, 1, 1)
	if err != nil {
		t.Fatalf("打开第一个库: %v", err)
	}
	defer db.Close()

	// 不支持的驱动 → 换库失败。
	if err := db.SwitchTo(ctx, "oracle", "whatever"); err == nil {
		t.Fatal("换到不存在的驱动应报错")
	}

	// 旧连接必须还能用。
	var n int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n); err != nil {
		t.Fatalf("换库失败后旧连接不可用: %v", err)
	}
}

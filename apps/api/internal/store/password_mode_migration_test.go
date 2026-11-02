package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// TestPasswordModeMigrationInsertsOptional 锁定 0008 迁移的效果：
// 全新实例（system_settings 里还没有 password.mode 行）跑完迁移后，
// 运行期生效的口令传输模式是 optional —— 即前端会发**密文**。
//
// ⚠️ 没有这条迁移时，内置默认值虽然已经是 optional，
// 但老实例的 application.yaml 里存着陈旧的 plaintext
// （那是当年写错的默认值被导出器写进文件的），
// 而 YAML 的优先级高于内置默认值 ⇒ 老实例仍会明文过网。
func TestPasswordModeMigrationInsertsOptional(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	db, err := Open("sqlite", dsn, 1, 1)
	if err != nil {
		t.Fatalf("打开测试库: %v", err)
	}
	defer func() { _ = db.Close() }()

	var raw string
	if err := db.QueryRow(context.Background(),
		`SELECT value FROM system_settings WHERE key = ?`, "password.mode").Scan(&raw); err != nil {
		t.Fatalf("读取 password.mode: %v", err)
	}
	var mode string
	if err := json.Unmarshal([]byte(raw), &mode); err != nil {
		t.Fatalf("解析 password.mode 的值 %q: %v", raw, err)
	}
	if mode != "optional" {
		t.Fatalf("password.mode = %q, 期望 %q", mode, "optional")
	}
}

// TestPasswordModeMigrationKeepsExplicitChoice 锁定 0008 迁移**不覆盖运维的显式选择**。
//
// 判据：设置页保存一项设置时必定写一行 DB，
// 所以库里已有 password.mode 行 ⟺ 运维显式选过模式，迁移必须尊重它。
func TestPasswordModeMigrationKeepsExplicitChoice(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	db, err := Open("sqlite", dsn, 1, 1)
	if err != nil {
		t.Fatalf("打开测试库: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	// 模拟运维显式选了 plaintext。
	if err := db.UpsertSetting(ctx, "password.mode", "plaintext", "operator"); err != nil {
		t.Fatalf("写显式选择: %v", err)
	}

	// 重跑 0008 的语句（迁移必须幂等，且不能覆盖显式选择）。
	b, err := migrationFS.ReadFile("migrations/sqlite/0008_password_mode.sql")
	if err != nil {
		t.Fatalf("读迁移: %v", err)
	}
	for _, stmt := range splitStatements(string(b)) {
		if _, err := db.Exec(ctx, stmt); err != nil {
			t.Fatalf("重跑迁移失败: %v\n语句: %s", err, stmt)
		}
	}

	var raw string
	if err := db.QueryRow(ctx, `SELECT value FROM system_settings WHERE key = ?`, "password.mode").Scan(&raw); err != nil {
		t.Fatalf("读取 password.mode: %v", err)
	}
	var mode string
	if err := json.Unmarshal([]byte(raw), &mode); err != nil {
		t.Fatalf("解析 password.mode 的值 %q: %v", raw, err)
	}
	if mode != "plaintext" {
		t.Fatalf("password.mode = %q, 期望 %q（迁移不该覆盖运维的显式选择）", mode, "plaintext")
	}
}

// TestPasswordModeMigrationEquivalentAcrossDialects 锁定三方言的迁移语义等价。
//
// ⚠️ postgres / mysql 本机跑不了，只能静态比对：
//
//	三份必须写入同一个值、同一套"没有才插入"的语义。
//	少一方言 = 那个方言上的实例仍会明文过网。
func TestPasswordModeMigrationEquivalentAcrossDialects(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres", "mysql"} {
		t.Run(dialect, func(t *testing.T) {
			b, err := migrationFS.ReadFile("migrations/" + dialect + "/0008_password_mode.sql")
			if err != nil {
				t.Fatalf("读迁移: %v", err)
			}
			s := string(b)
			if !strings.Contains(s, "'password.mode'") {
				t.Fatalf("%s 方言的迁移没写 password.mode", dialect)
			}
			if !strings.Contains(s, `'"optional"'`) {
				t.Fatalf("%s 方言的迁移写的不是 \"optional\"（value 存的是 JSON 编码后的字符串）", dialect)
			}
			// "没有才插入"：三种方言各用各的写法，但语义必须一致。
			if !strings.Contains(s, "OR IGNORE") &&
				!strings.Contains(s, "ON CONFLICT") &&
				!strings.Contains(s, "INSERT IGNORE") {
				t.Fatalf("%s 方言的迁移没有「没有才插入」的语义（会覆盖运维的显式选择）", dialect)
			}
		})
	}
}

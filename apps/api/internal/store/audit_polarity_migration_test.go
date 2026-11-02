package store

import (
	"context"
	"strings"
	"testing"
)

// TestAuditPolarityMigrationFlipsLegacyRows 锁定 0009 迁移把存量行的极性翻正。
//
// 旧写侧把布尔取反了：成功存 0、失败存 1。
// 迁移必须把存量行翻回来——否则审计页继续把"登录失败"显示成成功。
//
// ⚠️ 用**旧写侧的约定**造数据（成功存 0、失败存 1），
//
//	而不是用现在的 WriteAudit（它已经修正了，存的是正确值）。
func TestAuditPolarityMigrationFlipsLegacyRows(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	// 旧写侧的约定：成功 → 0，失败 → 1。
	if _, err := db.Exec(ctx, `INSERT INTO audit_logs
		(ts, actor, action, target, scope, client_ip, success, detail)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		Now(), "admin", "legacy.ok", "u1", "", "127.0.0.1", 0, ""); err != nil {
		t.Fatalf("造旧写侧的审计行: %v", err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO audit_logs
		(ts, actor, action, target, scope, client_ip, success, detail)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		Now(), "admin", "legacy.fail", "u2", "", "127.0.0.1", 1, ""); err != nil {
		t.Fatalf("造旧写侧的审计行: %v", err)
	}

	// 跑 0009 的语句（等价于升级时启动阶段执行的那次）。
	b, err := migrationFS.ReadFile("migrations/sqlite/0009_audit_success_polarity.sql")
	if err != nil {
		t.Fatalf("读迁移: %v", err)
	}
	for _, stmt := range splitStatements(string(b)) {
		if _, err := db.Exec(ctx, stmt); err != nil {
			t.Fatalf("执行迁移失败: %v\n语句: %s", err, stmt)
		}
	}

	logs, err := db.ListAudit(ctx, "", "", 10, 0)
	if err != nil {
		t.Fatalf("读取审计: %v", err)
	}
	got := map[string]bool{}
	for _, l := range logs {
		got[l.Action] = l.Success
	}
	if !got["legacy.ok"] {
		t.Errorf("成功的历史行迁移后仍是 success=false，极性没翻过来")
	}
	if got["legacy.fail"] {
		t.Errorf("失败的历史行迁移后是 success=true，极性没翻过来")
	}
}

// TestAuditPolarityMigrationEquivalentAcrossDialects 锁定三方言的 0009 是同一句。
//
// 少一个方言 = 那个引擎上的实例审计页继续显示反的标签，
// 而这种差异只有真连上那个数据库才看得见。
func TestAuditPolarityMigrationEquivalentAcrossDialects(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres", "mysql"} {
		t.Run(dialect, func(t *testing.T) {
			b, err := migrationFS.ReadFile("migrations/" + dialect + "/0009_audit_success_polarity.sql")
			if err != nil {
				t.Fatalf("读迁移: %v", err)
			}
			stmts := splitStatements(string(b))
			if len(stmts) != 1 {
				t.Fatalf("%s 方言的迁移有 %d 条语句，期望 1 条（翻正）", dialect, len(stmts))
			}
			if !strings.Contains(stmts[0], "UPDATE audit_logs SET success = 1 - success") {
				t.Errorf("%s 方言的迁移语句 = %q，期望翻正 success 列", dialect, stmts[0])
			}
		})
	}
}

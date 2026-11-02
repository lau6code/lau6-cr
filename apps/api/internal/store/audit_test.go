package store

import (
	"context"
	"testing"
)

// TestAuditSuccessPolarity 锁定审计 success 列的极性：**1 = 成功**。
//
// ⚠️ 为什么要有这条：建表默认值是 DEFAULT 1、读侧 ListAudit 直接 Scan 进 bool。
//
//	写侧与读侧的极性一旦相反 → 审计页把"登录失败"显示成成功、把成功显示成失败。
//	这种错不会报错，只会让人对审计记录做出相反的判断。
func TestAuditSuccessPolarity(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	if err := db.WriteAudit(ctx, "admin", "test.action.ok", "target", "", "127.0.0.1", true, nil); err != nil {
		t.Fatalf("写成功审计: %v", err)
	}
	if err := db.WriteAudit(ctx, "admin", "test.action.fail", "target", "", "127.0.0.1", false, nil); err != nil {
		t.Fatalf("写失败审计: %v", err)
	}

	logs, err := db.ListAudit(ctx, "", "", 10, 0)
	if err != nil {
		t.Fatalf("读取审计: %v", err)
	}
	got := map[string]bool{}
	for _, l := range logs {
		got[l.Action] = l.Success
	}
	if !got["test.action.ok"] {
		t.Errorf("成功的事件读回来是 success=false，极性反了")
	}
	if got["test.action.fail"] {
		t.Errorf("失败的事件读回来是 success=true，极性反了")
	}
}

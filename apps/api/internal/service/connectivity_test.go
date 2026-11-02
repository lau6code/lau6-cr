package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// probeToken 是测试用的引导令牌（长度合法）。
const probeToken = "0123456789abcdef0123456789abcdef"

// TestTestDatabaseSQLiteOK 覆盖 SQLite 连通性测试：
// 指向一个可写的临时目录 → 测试通过，并报告"文件已创建"。
func TestTestDatabaseSQLiteOK(t *testing.T) {
	ctx := context.Background()
	s := newTestSetupService(t)

	path := filepath.Join(t.TempDir(), "probe.db")
	res, err := s.TestDatabase(ctx, DatabaseTestInput{
		BootstrapToken: probeToken,
		Database: DatabaseInput{
			Driver:     "sqlite",
			SQLitePath: path,
		},
	})
	if err != nil {
		t.Fatalf("SQLite 测试: %v", err)
	}
	if !strings.Contains(res.Detail, "连接成功") {
		t.Fatalf("detail = %q, 期望提到连接成功", res.Detail)
	}
	if !strings.Contains(res.Detail, "已创建") {
		t.Fatalf("detail = %q, 期望提到文件已创建（新建库文件是有副作用的，必须说）", res.Detail)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("数据库文件没建出来: %v", err)
	}
}

// TestTestDatabaseRejectsBadDriver 覆盖不支持的驱动 → 参数错误。
func TestTestDatabaseRejectsBadDriver(t *testing.T) {
	ctx := context.Background()
	s := newTestSetupService(t)

	_, err := s.TestDatabase(ctx, DatabaseTestInput{
		BootstrapToken: probeToken,
		Database: DatabaseInput{
			Driver: "oracle",
		},
	})
	if err == nil {
		t.Fatal("不支持的驱动应被拒绝")
	}
	if !strings.Contains(err.Error(), ErrInvalidProbeParam.Error()) {
		t.Fatalf("err = %v, 期望是参数错误", err)
	}
}

// TestTestDatabaseRequiresHost 覆盖 postgres 缺主机 → 参数错误。
func TestTestDatabaseRequiresHost(t *testing.T) {
	ctx := context.Background()
	s := newTestSetupService(t)

	_, err := s.TestDatabase(ctx, DatabaseTestInput{
		BootstrapToken: probeToken,
		Database: DatabaseInput{
			Driver:   "postgres",
			Username: "lau6",
			Name:     "lau6cr",
		},
	})
	if err == nil {
		t.Fatal("缺主机应被拒绝")
	}
	if !strings.Contains(err.Error(), ErrInvalidProbeParam.Error()) {
		t.Fatalf("err = %v, 期望是参数错误", err)
	}
}

// TestTestDatabaseFailsWhenUnreachable 覆盖连不上时**必须报错**，
// 不能返回"看起来成功"的结果（禁止兜底）。
func TestTestDatabaseFailsWhenUnreachable(t *testing.T) {
	ctx := context.Background()
	s := newTestSetupService(t)

	// 127.0.0.1:1 上不会有服务在监听（连接被拒）。
	_, err := s.TestDatabase(ctx, DatabaseTestInput{
		BootstrapToken: probeToken,
		Database: DatabaseInput{
			Driver:   "postgres",
			Host:     "127.0.0.1",
			Port:     "1",
			Username: "lau6",
			Password: "pw",
			Name:     "lau6cr",
		},
	})
	if err == nil {
		t.Fatal("连不上时必须返回错误，不能假装成功")
	}
	if strings.Contains(err.Error(), ErrInvalidProbeParam.Error()) {
		t.Fatalf("err = %v, 期望是连接失败而不是参数错误", err)
	}
}

// TestTestDatabaseRejectsAfterInitialized 覆盖实例初始化后关闭探测口。
//
// 这个端点能连任意目标，长期留着就是探测面——初始化完成即关闭。
func TestTestDatabaseRejectsAfterInitialized(t *testing.T) {
	ctx := context.Background()
	s := newTestSetupService(t)

	if _, err := s.Submit(ctx, SetupInput{
		BootstrapToken: probeToken,
		PublicURL:      "http://localhost:5173",
		AdminUsername:  "admin",
		AdminPassword:  "VerifyPass123!",
	}); err != nil {
		t.Fatalf("首次 Submit: %v", err)
	}

	_, err := s.TestDatabase(ctx, DatabaseTestInput{
		BootstrapToken: probeToken,
		Database: DatabaseInput{
			Driver:     "sqlite",
			SQLitePath: filepath.Join(t.TempDir(), "after.db"),
		},
	})
	if err != ErrSetupAlreadyDone {
		t.Fatalf("err = %v, 期望 ErrSetupAlreadyDone", err)
	}
}

// TestTestDatabaseRejectsBadToken 覆盖令牌不对时拒绝探测。
func TestTestDatabaseRejectsBadToken(t *testing.T) {
	ctx := context.Background()
	s := newTestSetupService(t)

	// 首次初始化：库里没有摘要，只校验格式。
	if _, err := s.TestDatabase(ctx, DatabaseTestInput{
		BootstrapToken: "short",
		Database: DatabaseInput{
			Driver:     "sqlite",
			SQLitePath: filepath.Join(t.TempDir(), "x.db"),
		},
	}); err == nil {
		t.Fatal("弱令牌应被拒绝")
	}

	// setup reset 之后：库里留有摘要，必须匹配。
	if _, err := s.Submit(ctx, SetupInput{
		BootstrapToken: probeToken,
		PublicURL:      "http://localhost:5173",
		AdminUsername:  "admin",
		AdminPassword:  "VerifyPass123!",
	}); err != nil {
		t.Fatalf("首次 Submit: %v", err)
	}
	if _, err := s.db.Exec(ctx, `DELETE FROM system_settings WHERE key = 'system.initialized'`); err != nil {
		t.Fatalf("清初始化标记: %v", err)
	}

	_, err := s.TestDatabase(ctx, DatabaseTestInput{
		BootstrapToken: "ffffffffffffffffffffffffffffffff",
		Database: DatabaseInput{
			Driver:     "sqlite",
			SQLitePath: filepath.Join(t.TempDir(), "y.db"),
		},
	})
	if err != ErrBadBootstrapToken {
		t.Fatalf("err = %v, 期望 ErrBadBootstrapToken", err)
	}
}

// TestTestLDAPRejectsBadURL 覆盖 URL 非法 → 参数错误。
func TestTestLDAPRejectsBadURL(t *testing.T) {
	ctx := context.Background()
	s := newTestSetupService(t)

	_, err := s.TestLDAP(ctx, LDAPTestInput{
		BootstrapToken: probeToken,
		URL:            "http://ldap:389",
	})
	if err == nil {
		t.Fatal("非 ldap:// 开头应被拒绝")
	}
	if !strings.Contains(err.Error(), ErrInvalidProbeParam.Error()) {
		t.Fatalf("err = %v, 期望是参数错误", err)
	}
}

// TestTestLDAPFailsWhenUnreachable 覆盖 LDAP 连不上时必须报错。
func TestTestLDAPFailsWhenUnreachable(t *testing.T) {
	ctx := context.Background()
	s := newTestSetupService(t)

	// 127.0.0.1:1 上没有 LDAP 服务 → 连接被拒。
	_, err := s.TestLDAP(ctx, LDAPTestInput{
		BootstrapToken: probeToken,
		URL:            "ldap://127.0.0.1:1",
		UserSegment:    "uid={0},ou=people",
	})
	if err == nil {
		t.Fatal("连不上时必须返回错误，不能假装成功")
	}
}

// TestTestLDAPRejectsUsernameWithoutSegment 覆盖"填了账号但没填用户过滤器"。
//
// 这时拼不出 DN → 参数错误，而不是拿空模板去 bind。
func TestTestLDAPRejectsUsernameWithoutSegment(t *testing.T) {
	ctx := context.Background()
	s := newTestSetupService(t)

	_, err := s.TestLDAP(ctx, LDAPTestInput{
		BootstrapToken: probeToken,
		URL:            "ldap://127.0.0.1:1",
		UserSegment:    "uid=admin",
		Username:       "alice",
	})
	if err == nil {
		t.Fatal("用户过滤器缺 {0} 时应被拒绝")
	}
	if !strings.Contains(err.Error(), ErrInvalidProbeParam.Error()) {
		t.Fatalf("err = %v, 期望是参数错误", err)
	}
}

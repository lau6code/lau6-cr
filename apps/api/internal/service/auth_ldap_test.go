package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lau6code/lau6-cr/apps/api/internal/cache"
	"github.com/lau6code/lau6-cr/apps/api/internal/ldap"
)

// TestLDAPUnavailableIsNotCredentialFailure 覆盖"目录不可达 ≠ 口令错"。
//
// ⚠️ 断言的重点是**不计入登录失败锁定**：
//
//	failLimit=1，只要记了一次失败账号就会被锁；
//	目录抖动一次就把所有 LDAP 账号锁死，是不可接受的。
func TestLDAPUnavailableIsNotCredentialFailure(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	if _, err := db.CreateUser(ctx, "alice", "x", "ldap", "", false); err != nil {
		t.Fatalf("建用户: %v", err)
	}

	c := cache.NewLocal(100)
	defer c.Close()
	guard := NewLoginGuard(
		func() cache.Cache { return c },
		func() int { return 1 },
		func() time.Duration { return 5 * time.Minute },
		nil,
	)

	auth := NewAuthService(db, AuthOptions{
		Guard: guard,
		LDAP: func() ldap.Authenticator {
			return &ldap.Fake{Err: ldap.ErrUnavailable}
		},
		LDAPEnabled: func() bool { return true },
	})

	ctx = WithClientIP(ctx, "1.2.3.4")
	_, err := auth.VerifyCredentials(ctx, "alice", "whatever")
	if !errors.Is(err, ErrAuthUnavailable) {
		t.Fatalf("目录不可达应当报 ErrAuthUnavailable，实际: %v", err)
	}
	if errors.Is(err, ErrInvalidCredentials) {
		t.Fatal("目录不可达不能被当成凭据无效——那会让运维去查'谁在撞库'")
	}
	// 关键断言：不可达不计入失败计数。
	if err := guard.Check(ctx, "alice", "1.2.3.4"); err != nil {
		t.Fatalf("目录不可达不该把账号锁住: %v", err)
	}
}

// TestLDAPInvalidCredentialsCountsAsFailure 覆盖"口令错仍然算失败"。
//
// 这是 F4 修复的另一半：不能把"什么都算不可达"当成解决方案——
// 那样撞库就再也不会触发锁定了。
func TestLDAPInvalidCredentialsCountsAsFailure(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	if _, err := db.CreateUser(ctx, "alice", "x", "ldap", "", false); err != nil {
		t.Fatalf("建用户: %v", err)
	}

	c := cache.NewLocal(100)
	defer c.Close()
	guard := NewLoginGuard(
		func() cache.Cache { return c },
		func() int { return 1 },
		func() time.Duration { return 5 * time.Minute },
		nil,
	)

	auth := NewAuthService(db, AuthOptions{
		Guard: guard,
		LDAP: func() ldap.Authenticator {
			return &ldap.Fake{Users: map[string]string{"alice": "correct"}}
		},
		LDAPEnabled: func() bool { return true },
	})

	ctx = WithClientIP(ctx, "1.2.3.4")
	_, err := auth.VerifyCredentials(ctx, "alice", "wrong")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("口令错误应当报 ErrInvalidCredentials，实际: %v", err)
	}
	if err := guard.Check(ctx, "alice", "1.2.3.4"); err == nil {
		t.Fatal("口令错误必须计入失败锁定，否则撞库就拦不住了")
	}
}

// TestLDAPSettingsAreHot 覆盖"LDAP 开关跟着设置走"。
//
// ldap.* 是 T0 热生效设置（14-settings.md §3.4：「切换后立即影响登录」），
// 客户端必须在运行期跟着设置快照走，不能在启动时冻住。
func TestLDAPSettingsAreHot(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	if _, err := db.CreateUser(ctx, "alice", "x", "ldap", "", false); err != nil {
		t.Fatalf("建用户: %v", err)
	}

	// enabled 变量模拟设置项：运行期可以改。
	enabled := true
	fake := &ldap.Fake{Users: map[string]string{"alice": "correct"}}

	auth := NewAuthService(db, AuthOptions{
		LDAP: func() ldap.Authenticator {
			if !enabled {
				return nil
			}
			return fake
		},
		LDAPEnabled: func() bool { return enabled },
		AllowCreate: func() bool { return true },
	})

	if _, err := auth.VerifyCredentials(ctx, "alice", "correct"); err != nil {
		t.Fatalf("LDAP 开启时应当登录成功: %v", err)
	}

	// 运行期关掉 LDAP → 立刻不再走 LDAP。
	enabled = false
	if _, err := auth.VerifyCredentials(ctx, "alice", "correct"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("LDAP 关闭后应当拒绝登录，实际: %v", err)
	}

	// 再打开 → 立刻恢复。
	enabled = true
	if _, err := auth.VerifyCredentials(ctx, "alice", "correct"); err != nil {
		t.Fatalf("LDAP 重新开启后应当登录成功: %v", err)
	}
}

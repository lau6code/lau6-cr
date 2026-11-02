package service

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lau6code/lau6-cr/apps/api/internal/crypto"
	"github.com/lau6code/lau6-cr/apps/api/internal/store"
)

// newTestPATService 造一个个人令牌服务。
func newTestPATService(t *testing.T) (*PersonalTokenService, *store.DB) {
	t.Helper()
	db := newTestDB(t)
	return NewPersonalTokenService(db, PersonalTokenOptions{}), db
}

// TestPersonalTokenRawValueShownOnce 覆盖"原始令牌只返回一次"。
//
// 这是 PAT 的核心安全性质：库里只有散列，原始值取不回来。
func TestPersonalTokenRawValueShownOnce(t *testing.T) {
	ctx := context.Background()
	svc, db := newTestPATService(t)
	user, err := db.CreateUser(ctx, "alice", mustHash(t, "VerifyPass123!"), "local", "", false)
	if err != nil {
		t.Fatalf("创建用户: %v", err)
	}

	raw, tok, err := svc.Create(ctx, user.ID, "ci 推送", ExpiryPolicy{})
	if err != nil {
		t.Fatalf("生成令牌: %v", err)
	}
	if !strings.HasPrefix(raw, crypto.PATPrefix) {
		t.Fatalf("令牌前缀 = %q，期望 %q", raw, crypto.PATPrefix)
	}
	if tok.TokenPrefix == "" || tok.TokenPrefix == raw {
		t.Fatalf("展示前缀不该等于令牌本体: %q", tok.TokenPrefix)
	}

	list, err := svc.List(ctx, user.ID)
	if err != nil {
		t.Fatalf("列令牌: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("列出了 %d 枚令牌，期望 1 枚", len(list))
	}
	if list[0].TokenHash == "" {
		t.Fatal("库里应当有散列")
	}
	if list[0].TokenHash == raw {
		t.Fatal("库里存的竟然是令牌本体——数据库泄露就等于凭据泄露")
	}
	if list[0].TokenHash != crypto.HashPersonalToken(raw) {
		t.Fatal("散列与令牌不匹配")
	}
}

// TestPersonalTokenVerify 覆盖"校验通过 → 拿到属主"。
func TestPersonalTokenVerify(t *testing.T) {
	ctx := context.Background()
	svc, db := newTestPATService(t)
	user, err := db.CreateUser(ctx, "alice", mustHash(t, "VerifyPass123!"), "local", "", false)
	if err != nil {
		t.Fatalf("创建用户: %v", err)
	}
	raw, _, err := svc.Create(ctx, user.ID, "ci", ExpiryPolicy{})
	if err != nil {
		t.Fatalf("生成令牌: %v", err)
	}

	owner, tok, err := svc.Verify(ctx, raw)
	if err != nil {
		t.Fatalf("校验失败: %v", err)
	}
	if owner.Username != "alice" {
		t.Fatalf("属主 = %q，期望 alice", owner.Username)
	}
	if tok.TokenHash == raw {
		t.Fatal("令牌记录里不该有原始值")
	}

	// 最近使用时间要被记上（运维靠它判断哪枚该清理）。
	list, err := svc.List(ctx, user.ID)
	if err != nil {
		t.Fatalf("列令牌: %v", err)
	}
	if list[0].LastUsedAt == nil {
		t.Fatal("最近使用时间没记上")
	}
}

// TestPersonalTokenRejectInvalid 覆盖"无效令牌 → 明确的失败"。
func TestPersonalTokenRejectInvalid(t *testing.T) {
	ctx := context.Background()
	svc, db := newTestPATService(t)
	user, err := db.CreateUser(ctx, "alice", mustHash(t, "VerifyPass123!"), "local", "", false)
	if err != nil {
		t.Fatalf("创建用户: %v", err)
	}
	raw, _, err := svc.Create(ctx, user.ID, "ci", ExpiryPolicy{})
	if err != nil {
		t.Fatalf("生成令牌: %v", err)
	}

	for _, bad := range []string{
		"",
		"lau6_u_不是一枚真令牌",
		raw + "x", // 多一个字符
		"not-a-pat-at-all",
	} {
		if _, _, err := svc.Verify(ctx, bad); err == nil {
			t.Fatalf("无效令牌 %q 竟然校验通过", bad)
		} else if !errors.Is(err, ErrInvalidPersonalToken) {
			t.Fatalf("错误类型 = %T，期望 ErrInvalidPersonalToken", err)
		}
	}
}

// TestPersonalTokenRevoke 覆盖"吊销后立刻失效"。
func TestPersonalTokenRevoke(t *testing.T) {
	ctx := context.Background()
	svc, db := newTestPATService(t)
	user, err := db.CreateUser(ctx, "alice", mustHash(t, "VerifyPass123!"), "local", "", false)
	if err != nil {
		t.Fatalf("创建用户: %v", err)
	}
	raw, tok, err := svc.Create(ctx, user.ID, "ci", ExpiryPolicy{})
	if err != nil {
		t.Fatalf("生成令牌: %v", err)
	}
	if err := svc.Revoke(ctx, tok.ID, user.ID); err != nil {
		t.Fatalf("吊销: %v", err)
	}
	if _, _, err := svc.Verify(ctx, raw); err == nil {
		t.Fatal("吊销后令牌仍然可用")
	}
	// 别人的令牌不该被我的会话吊销掉。
	other, err := db.CreateUser(ctx, "bob", mustHash(t, "VerifyPass123!"), "local", "", false)
	if err != nil {
		t.Fatalf("创建用户: %v", err)
	}
	raw2, tok2, err := svc.Create(ctx, other.ID, "ci", ExpiryPolicy{})
	if err != nil {
		t.Fatalf("生成令牌: %v", err)
	}
	if err := svc.Revoke(ctx, tok2.ID, user.ID); err == nil {
		t.Fatal("alice 竟然吊销掉了 bob 的令牌")
	}
	if _, _, err := svc.Verify(ctx, raw2); err != nil {
		t.Fatalf("bob 的令牌不该受影响: %v", err)
	}
}

// TestPersonalTokenRevokeAll 覆盖"吊销某人的全部令牌"。
func TestPersonalTokenRevokeAll(t *testing.T) {
	ctx := context.Background()
	svc, db := newTestPATService(t)
	user, err := db.CreateUser(ctx, "alice", mustHash(t, "VerifyPass123!"), "local", "", false)
	if err != nil {
		t.Fatalf("创建用户: %v", err)
	}
	for _, name := range []string{"ci-1", "ci-2"} {
		if _, _, err := svc.Create(ctx, user.ID, name, ExpiryPolicy{}); err != nil {
			t.Fatalf("生成令牌: %v", err)
		}
	}
	n, err := svc.RevokeAll(ctx, user.ID)
	if err != nil {
		t.Fatalf("吊销全部: %v", err)
	}
	if n != 2 {
		t.Fatalf("吊销了 %d 枚，期望 2 枚", n)
	}
	list, err := svc.List(ctx, user.ID)
	if err != nil {
		t.Fatalf("列令牌: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("还剩 %d 枚令牌", len(list))
	}
}

// TestRealmAcceptsPersonalToken 覆盖"PAT 能当口令换令牌"。
//
// 这是"用户用 token 来推拉"的 docker 路径：
// docker login -u <用户名> -p <PAT> → realm 认 PAT → 换一枚用户级 JWT。
func TestRealmAcceptsPersonalToken(t *testing.T) {
	ctx := context.Background()
	svc, db := newTestTokenService(t)
	seedProjectOnly(t, db, "team-a")
	user, err := db.UserByUsername(ctx, "admin")
	if err != nil {
		t.Fatalf("查用户: %v", err)
	}
	pats := NewPersonalTokenService(db, PersonalTokenOptions{})
	raw, _, err := pats.Create(ctx, user.ID, "ci", ExpiryPolicy{})
	if err != nil {
		t.Fatalf("生成令牌: %v", err)
	}

	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("admin:"+raw))
	res, err := svc.Issue(ctx, basic, []string{"repository:team-a/app:pull"})
	if err != nil {
		t.Fatalf("PAT 换令牌失败: %v", err)
	}
	if res.Subject != "admin" {
		t.Fatalf("subject = %q，期望 admin", res.Subject)
	}
	if len(res.Granted) == 0 {
		t.Fatal("PAT 换的令牌没有 access")
	}
}

// TestRealmRejectsBadPersonalToken 覆盖"无效 PAT → 401"。
func TestRealmRejectsBadPersonalToken(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestTokenService(t)

	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("admin:lau6_u_不是真的"))
	if _, err := svc.Issue(ctx, basic, []string{"repository:team-a/app:pull"}); err == nil {
		t.Fatal("无效 PAT 竟然换到了令牌")
	}
}

// TestPersonalTokenNotUsableForUILogin 覆盖"PAT 不能当 UI 登录口令"。
//
// PAT 是**registry 凭据**，不是账户口令。
// 让它能登录 Web UI 等于给长期凭据开了一条完全不同的入口——
// 口令轮换、封禁、LDAP 策略都管不到它。
func TestPersonalTokenNotUsableForUILogin(t *testing.T) {
	ctx := context.Background()
	svc, db := newTestTokenService(t)
	user, err := db.UserByUsername(ctx, "admin")
	if err != nil {
		t.Fatalf("查用户: %v", err)
	}
	pats := NewPersonalTokenService(db, PersonalTokenOptions{})
	raw, _, err := pats.Create(ctx, user.ID, "ci", ExpiryPolicy{})
	if err != nil {
		t.Fatalf("生成令牌: %v", err)
	}
	if _, err := svc.auth.VerifyCredentials(ctx, "admin", raw); err == nil {
		t.Fatal("PAT 竟然能当 UI 登录口令——它只能是 registry 凭据")
	}
}

// TestPersonalTokenNeverExpires 覆盖"永久有效"。
//
// 永久是用户**显式选**的：ExpiresAt 必须是 nil（落库 NULL），
// 而不是"没填就永久"——那会让用户以为拿到的是 30 天。
func TestPersonalTokenNeverExpires(t *testing.T) {
	ctx := context.Background()
	svc, db := newTestPATService(t)
	user, err := db.CreateUser(ctx, "alice", mustHash(t, "VerifyPass123!"), "local", "", false)
	if err != nil {
		t.Fatalf("创建用户: %v", err)
	}

	raw, tok, err := svc.Create(ctx, user.ID, "ci", ExpiryPolicy{NeverExpires: true})
	if err != nil {
		t.Fatalf("生成令牌: %v", err)
	}
	if tok.ExpiresAt != nil {
		t.Fatalf("永久令牌的失效时间 = %d，期望 nil", *tok.ExpiresAt)
	}
	// 永久令牌照常可用。
	if _, _, err := svc.Verify(ctx, raw); err != nil {
		t.Fatalf("永久令牌校验失败: %v", err)
	}
	// 列表回读也必须是 nil。
	list, err := svc.List(ctx, user.ID)
	if err != nil {
		t.Fatalf("列令牌: %v", err)
	}
	if list[0].ExpiresAt != nil {
		t.Fatalf("列表里的失效时间 = %d，期望 nil（永久）", *list[0].ExpiresAt)
	}
}

// TestPersonalTokenCustomExpiry 覆盖"指定失效时间"。
func TestPersonalTokenCustomExpiry(t *testing.T) {
	ctx := context.Background()
	svc, db := newTestPATService(t)
	user, err := db.CreateUser(ctx, "alice", mustHash(t, "VerifyPass123!"), "local", "", false)
	if err != nil {
		t.Fatalf("创建用户: %v", err)
	}

	want := store.Now() + int64(72*time.Hour/time.Second)
	exp := want
	raw, tok, err := svc.Create(ctx, user.ID, "ci", ExpiryPolicy{ExpiresAt: &exp})
	if err != nil {
		t.Fatalf("生成令牌: %v", err)
	}
	if tok.ExpiresAt == nil || *tok.ExpiresAt != want {
		t.Fatalf("失效时间 = %v，期望 %d", tok.ExpiresAt, want)
	}
	if _, _, err := svc.Verify(ctx, raw); err != nil {
		t.Fatalf("校验失败: %v", err)
	}
}

// TestPersonalTokenDefaultExpiry 覆盖"不给策略 → 走系统默认 TTL"。
func TestPersonalTokenDefaultExpiry(t *testing.T) {
	ctx := context.Background()
	svc, db := newTestPATService(t)
	user, err := db.CreateUser(ctx, "alice", mustHash(t, "VerifyPass123!"), "local", "", false)
	if err != nil {
		t.Fatalf("创建用户: %v", err)
	}

	before := store.Now()
	_, tok, err := svc.Create(ctx, user.ID, "ci", ExpiryPolicy{})
	if err != nil {
		t.Fatalf("生成令牌: %v", err)
	}
	if tok.ExpiresAt == nil {
		t.Fatal("默认不该是永久")
	}
	delta := *tok.ExpiresAt - before
	if delta < int64(DefaultPersonalTokenTTL/time.Second)-5 || delta > int64(DefaultPersonalTokenTTL/time.Second)+5 {
		t.Fatalf("默认失效时间距今 %d 秒，不在 %v 附近", delta, DefaultPersonalTokenTTL)
	}
}

// TestPersonalTokenRejectsContradictoryExpiry 覆盖"永久 + 指定时间 → 报错"。
//
// 矛盾请求必须暴露给调用方，不能悄悄挑一个执行。
func TestPersonalTokenRejectsContradictoryExpiry(t *testing.T) {
	ctx := context.Background()
	svc, db := newTestPATService(t)
	user, err := db.CreateUser(ctx, "alice", mustHash(t, "VerifyPass123!"), "local", "", false)
	if err != nil {
		t.Fatalf("创建用户: %v", err)
	}

	exp := store.Now() + 3600
	if _, _, err := svc.Create(ctx, user.ID, "ci",
		ExpiryPolicy{NeverExpires: true, ExpiresAt: &exp}); err == nil {
		t.Fatal("矛盾请求竟然创建成功了")
	}
}

// TestPersonalTokenRejectsPastExpiry 覆盖"失效时间在过去 → 报错"。
func TestPersonalTokenRejectsPastExpiry(t *testing.T) {
	ctx := context.Background()
	svc, db := newTestPATService(t)
	user, err := db.CreateUser(ctx, "alice", mustHash(t, "VerifyPass123!"), "local", "", false)
	if err != nil {
		t.Fatalf("创建用户: %v", err)
	}

	exp := store.Now() - 60
	if _, _, err := svc.Create(ctx, user.ID, "ci", ExpiryPolicy{ExpiresAt: &exp}); err == nil {
		t.Fatal("过期的失效时间竟然创建成功了")
	}
}

// TestPersonalTokenSetExpiry 覆盖"事后改失效时间"。
//
// 关键性质：改的只是失效时间，**散列不动**——
// CI 里已经配好的凭据不用重新配置，令牌不会因为改了时间而失效。
func TestPersonalTokenSetExpiry(t *testing.T) {
	ctx := context.Background()
	svc, db := newTestPATService(t)
	user, err := db.CreateUser(ctx, "alice", mustHash(t, "VerifyPass123!"), "local", "", false)
	if err != nil {
		t.Fatalf("创建用户: %v", err)
	}

	raw, tok, err := svc.Create(ctx, user.ID, "ci", ExpiryPolicy{NeverExpires: true})
	if err != nil {
		t.Fatalf("生成令牌: %v", err)
	}
	hashBefore := tok.TokenHash

	// 永久 → 定时。
	want := store.Now() + int64(48*time.Hour/time.Second)
	exp := want
	if err := svc.SetExpiry(ctx, tok.ID, user.ID, ExpiryPolicy{ExpiresAt: &exp}); err != nil {
		t.Fatalf("改失效时间: %v", err)
	}
	list, err := svc.List(ctx, user.ID)
	if err != nil {
		t.Fatalf("列令牌: %v", err)
	}
	if list[0].ExpiresAt == nil || *list[0].ExpiresAt != want {
		t.Fatalf("失效时间 = %v，期望 %d", list[0].ExpiresAt, want)
	}
	if list[0].TokenHash != hashBefore {
		t.Fatal("改失效时间不该动散列——令牌本体必须保持不变")
	}
	if _, _, err := svc.Verify(ctx, raw); err != nil {
		t.Fatalf("改失效时间后令牌不该失效: %v", err)
	}

	// 定时 → 永久。
	if err := svc.SetExpiry(ctx, tok.ID, user.ID, ExpiryPolicy{NeverExpires: true}); err != nil {
		t.Fatalf("改成永久: %v", err)
	}
	list, err = svc.List(ctx, user.ID)
	if err != nil {
		t.Fatalf("列令牌: %v", err)
	}
	if list[0].ExpiresAt != nil {
		t.Fatalf("失效时间 = %v，期望 nil（永久）", *list[0].ExpiresAt)
	}
}

// TestPersonalTokenSetExpiryRejectsOthers 覆盖"改不了别人的令牌"。
func TestPersonalTokenSetExpiryRejectsOthers(t *testing.T) {
	ctx := context.Background()
	svc, db := newTestPATService(t)
	alice, err := db.CreateUser(ctx, "alice", mustHash(t, "VerifyPass123!"), "local", "", false)
	if err != nil {
		t.Fatalf("创建用户: %v", err)
	}
	bob, err := db.CreateUser(ctx, "bob", mustHash(t, "VerifyPass123!"), "local", "", false)
	if err != nil {
		t.Fatalf("创建用户: %v", err)
	}
	_, bobTok, err := svc.Create(ctx, bob.ID, "bob 的令牌", ExpiryPolicy{})
	if err != nil {
		t.Fatalf("生成令牌: %v", err)
	}

	if err := svc.SetExpiry(ctx, bobTok.ID, alice.ID, ExpiryPolicy{NeverExpires: true}); err == nil {
		t.Fatal("alice 竟然改得了 bob 的令牌")
	} else if !errors.Is(err, ErrInvalidPersonalToken) {
		t.Fatalf("错误类型 = %T，期望 ErrInvalidPersonalToken", err)
	}
}

// TestPersonalTokenExpiredRejected 覆盖"过期的令牌一律拒绝"。
//
// 这里直接改库把失效时间拨到过去（service 层本来不让创建过期令牌），
// 模拟"令牌到期"这条路径。
func TestPersonalTokenExpiredRejected(t *testing.T) {
	ctx := context.Background()
	svc, db := newTestPATService(t)
	user, err := db.CreateUser(ctx, "alice", mustHash(t, "VerifyPass123!"), "local", "", false)
	if err != nil {
		t.Fatalf("创建用户: %v", err)
	}
	raw, tok, err := svc.Create(ctx, user.ID, "ci", ExpiryPolicy{})
	if err != nil {
		t.Fatalf("生成令牌: %v", err)
	}
	past := store.Now() - 60
	if err := db.SetPersonalTokenExpiry(ctx, tok.ID, user.ID, &past); err != nil {
		t.Fatalf("改库里的失效时间: %v", err)
	}
	if _, _, err := svc.Verify(ctx, raw); err == nil {
		t.Fatal("过期的令牌竟然还能用")
	} else if !errors.Is(err, ErrInvalidPersonalToken) {
		t.Fatalf("错误类型 = %T，期望 ErrInvalidPersonalToken", err)
	}
}

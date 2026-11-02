package ldap

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/go-ldap/ldap/v3"
)

// TestClassifyBindError 锁定"凭据无效"与"目录不可达"的区分。
//
// ⚠️ 这条区分是登录锁定策略的前提：
//
//	两者都算"登录失败"的话，目录侧一次网络抖动就会把所有 LDAP 用户
//	按"猜错口令"计数，累计到 access.login_fail_limit 后连账号带来源 IP 一起锁死。
func TestClassifyBindError(t *testing.T) {
	// 凭据无效：LDAP result code 49（invalidCredentials）。
	credErr := classifyBindError(ldap.NewError(ldap.LDAPResultInvalidCredentials, errors.New("invalid credentials")))
	if !errors.Is(credErr, ErrInvalidCredentials) {
		t.Errorf("result code 49 应当判为凭据无效，实际: %v", credErr)
	}
	if errors.Is(credErr, ErrUnavailable) {
		t.Error("凭据无效不该同时判为目录不可达")
	}

	// DN 不存在（32 noSuchObject）也是"这条凭据不对"。
	noSuch := classifyBindError(ldap.NewError(ldap.LDAPResultNoSuchObject, errors.New("no such object")))
	if !errors.Is(noSuch, ErrInvalidCredentials) {
		t.Errorf("result code 32 应当判为凭据无效，实际: %v", noSuch)
	}

	// 网络错误：没有 result code，一律算"目录不可达"。
	netErr := classifyBindError(&net.OpError{Op: "dial", Err: errors.New("connection refused")})
	if !errors.Is(netErr, ErrUnavailable) {
		t.Errorf("网络错误应当判为目录不可达，实际: %v", netErr)
	}
	if errors.Is(netErr, ErrInvalidCredentials) {
		t.Error("网络错误不该判为凭据无效")
	}

	// 服务器忙（51 busy）同样不是用户的问题。
	busy := classifyBindError(ldap.NewError(ldap.LDAPResultBusy, errors.New("busy")))
	if !errors.Is(busy, ErrUnavailable) {
		t.Errorf("result code 51 应当判为目录不可达，实际: %v", busy)
	}
}

// TestFakeBindErrorKinds 锁定测试替身的错误分类。
//
// 测试替身不能比真实实现更宽松：Fake 也要能表达"目录不可达"。
func TestFakeBindErrorKinds(t *testing.T) {
	ctx := context.Background()

	// 口令不对 → 凭据无效。
	fake := &Fake{Users: map[string]string{"alice": "correct"}}
	err := fake.Bind(ctx, "alice", "wrong")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("口令错误应当判为凭据无效，实际: %v", err)
	}
	if err := fake.Bind(ctx, "alice", "correct"); err != nil {
		t.Errorf("正确口令应当通过，实际: %v", err)
	}

	// 目录不可达 → 原样透出。
	fake.Err = ErrUnavailable
	err = fake.Bind(ctx, "alice", "correct")
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("目录不可达应当透出 ErrUnavailable，实际: %v", err)
	}
}

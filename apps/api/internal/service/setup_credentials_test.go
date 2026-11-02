package service

import (
	"context"
	"strings"
	"testing"
)

// TestValidateAdminCredentials 覆盖管理员凭据的校验规则。
//
// 这条规则只有一份（CLI 的终端输入与 Web 向导共用），
// 两处各写一份必然漂移——有一处放宽了，另一处照旧。
func TestValidateAdminCredentials(t *testing.T) {
	cases := []struct {
		name     string
		username string
		password string
		wantErr  bool
	}{
		{"正常", "admin", "VerifyPass123!", false},
		{"用户名为空", "", "VerifyPass123!", true},
		{"口令为空", "admin", "", true},
		{"口令 7 位", "admin", "1234567", true},
		{"口令 8 位", "admin", "12345678", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateAdminCredentials(tc.username, tc.password)
			if tc.wantErr && err == nil {
				t.Fatal("应当被拒绝，但没有报错")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("应当通过，却报错: %v", err)
			}
		})
	}
}

// TestValidateAdminCredentials_Messages 锁定两条错误文案。
//
// 运维脚本与既有测试按文案判断原因，改字会打断它们。
func TestValidateAdminCredentials_Messages(t *testing.T) {
	err := ValidateAdminCredentials("", "")
	if err == nil || !strings.Contains(err.Error(), "管理员用户名与密码不能为空") {
		t.Fatalf("期望『管理员用户名与密码不能为空』，实际: %v", err)
	}
	err = ValidateAdminCredentials("admin", "123")
	if err == nil || !strings.Contains(err.Error(), "管理员密码至少 8 位") {
		t.Fatalf("期望『管理员密码至少 8 位』，实际: %v", err)
	}
}

// TestSubmitRequiresCredentialsInInput 覆盖"不再有服务层兜底"。
//
// 兜底（请求里没传就退回 SetupDeps 里的值）意味着凭据有两个来源，
// 调用方说不清自己到底用了哪个——删掉它，错误必须暴露出来。
func TestSubmitRequiresCredentialsInInput(t *testing.T) {
	s := newTestSetupService(t)
	ctx := context.Background()

	_, err := s.Submit(ctx, SetupInput{
		BootstrapToken: strings.Repeat("a", 32),
		PublicURL:      "http://localhost:5001",
		// AdminUsername / AdminPassword 故意留空
	})
	if err == nil {
		t.Fatal("没有凭据应当被拒绝")
	}
	if !strings.Contains(err.Error(), "管理员用户名与密码不能为空") {
		t.Fatalf("期望凭据缺失的错误，实际: %v", err)
	}
}

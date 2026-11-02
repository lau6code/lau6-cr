package main

import (
	"os"
	"strings"
	"testing"
)

// TestReadAdminCredentialsPiped 覆盖管道来源：两行（用户名、口令）。
//
// 这是给 CI 用的入口——`printf 'admin\ns3cret-pass\n' | lau6-cr setup`。
func TestReadAdminCredentialsPiped(t *testing.T) {
	in, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatalf("建临时文件: %v", err)
	}
	if _, err := in.WriteString("admin\ns3cret-pass\n"); err != nil {
		t.Fatalf("写输入: %v", err)
	}
	if _, err := in.Seek(0, 0); err != nil {
		t.Fatalf("回卷: %v", err)
	}
	defer in.Close()

	user, pass, err := readAdminCredentials(in, os.Stderr)
	if err != nil {
		t.Fatalf("读取凭据: %v", err)
	}
	if user != "admin" {
		t.Errorf("用户名期望 admin，实际 %q", user)
	}
	if pass != "s3cret-pass" {
		t.Errorf("口令期望 s3cret-pass，实际 %q", pass)
	}
}

// TestReadAdminCredentialsPiped_ShortPassword 覆盖口令过短被拒。
//
// 校验不过必须报错退出，不能放行——建出一个用户自己不知道口令的管理员
// 等于把实例锁死。
func TestReadAdminCredentialsPiped_ShortPassword(t *testing.T) {
	in, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatalf("建临时文件: %v", err)
	}
	if _, err := in.WriteString("admin\nshort\n"); err != nil {
		t.Fatalf("写输入: %v", err)
	}
	if _, err := in.Seek(0, 0); err != nil {
		t.Fatalf("回卷: %v", err)
	}
	defer in.Close()

	if _, _, err := readAdminCredentials(in, os.Stderr); err == nil {
		t.Fatal("5 位口令应当被拒绝")
	}
}

// TestReadAdminCredentialsPiped_EmptyUsername 覆盖用户名为空 → 取默认值。
func TestReadAdminCredentialsPiped_EmptyUsername(t *testing.T) {
	in, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatalf("建临时文件: %v", err)
	}
	if _, err := in.WriteString("\ns3cret-pass\n"); err != nil {
		t.Fatalf("写输入: %v", err)
	}
	if _, err := in.Seek(0, 0); err != nil {
		t.Fatalf("回卷: %v", err)
	}
	defer in.Close()

	user, _, err := readAdminCredentials(in, os.Stderr)
	if err != nil {
		t.Fatalf("读取凭据: %v", err)
	}
	if user != defaultAdminUsername {
		t.Errorf("用户名期望默认值 %q，实际 %q", defaultAdminUsername, user)
	}
}

// TestFinishAdminCredentials_RejectsShort 覆盖归一化后的校验。
func TestFinishAdminCredentials_RejectsShort(t *testing.T) {
	if _, _, err := finishAdminCredentials("admin", "1234567"); err == nil {
		t.Fatal("7 位口令应当被拒绝")
	}
	if _, _, err := finishAdminCredentials("admin", "12345678"); err != nil {
		t.Fatalf("8 位口令应当通过: %v", err)
	}
}

// TestFinishAdminCredentials_KeepsPasswordSpaces 覆盖口令两端的空格不被抹掉。
//
// 口令里的空格是用户有意敲的；悄悄 trim 掉会让用户以为自己设的
// 口令与实际生效的不一样。
func TestFinishAdminCredentials_KeepsPasswordSpaces(t *testing.T) {
	_, pass, err := finishAdminCredentials("admin", " pass word ")
	if err != nil {
		t.Fatalf("应当通过: %v", err)
	}
	if pass != " pass word " {
		t.Errorf("口令两端空格应保留，实际 %q", pass)
	}
	if strings.TrimSpace(pass) == "" {
		t.Error("口令不该被清空")
	}
}

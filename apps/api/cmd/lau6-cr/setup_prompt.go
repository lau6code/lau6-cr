package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/lau6code/lau6-cr/apps/api/internal/service"
)

// defaultAdminUsername 是交互式输入时用户名的默认值。
//
// 与 Web 向导的默认值一致（SetupView 的 admin_username 初值）。
const defaultAdminUsername = "admin"

// readAdminCredentials 读取管理员凭据。
//
// ⚠️ 凭据只从**界面**来：交互式终端（口令不回显）或 stdin 管道。
// 不读环境变量、不接受命令行参数——
// 命令行参数会出现在 ps 输出与 shell 历史里（08-setup.md §10）。
//
// stdin 是 TTY → 交互式：提示用户名（带默认值）、口令不回显、两次口令确认。
// stdin 不是 TTY → 从管道读两行（用户名、口令），不打印提示，供 CI 使用。
func readAdminCredentials(in *os.File, out io.Writer) (string, string, error) {
	if term.IsTerminal(int(in.Fd())) {
		return readAdminCredentialsInteractive(in, out)
	}
	return readAdminCredentialsPiped(in)
}

// readAdminCredentialsInteractive 走交互式提示。
//
// ⚠️ 口令用 term.ReadPassword 读：**不回显**。
// 回显的口令会留在终端滚屏里。
func readAdminCredentialsInteractive(in *os.File, out io.Writer) (string, string, error) {
	// 一个 reader 读到底：终端在 canonical 模式下一次只交出一行，
	// bufio 不会把后面的口令行一起吞掉。
	r := bufio.NewReader(in)

	fmt.Fprintf(out, "系统管理员用户名 [%s]: ", defaultAdminUsername)
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return "", "", fmt.Errorf("读取用户名失败: %w", err)
	}

	password, err := readSecret(in, out, "系统管理员口令（至少 8 位，不回显）: ")
	if err != nil {
		return "", "", err
	}
	confirm, err := readSecret(in, out, "确认口令: ")
	if err != nil {
		return "", "", err
	}
	if password != confirm {
		return "", "", fmt.Errorf("两次输入的口令不一致")
	}
	return finishAdminCredentials(strings.TrimSpace(line), password)
}

// readAdminCredentialsPiped 从管道读两行：用户名、口令。
//
// 不打印提示——管道里没有人在看提示。
func readAdminCredentialsPiped(in *os.File) (string, string, error) {
	r := bufio.NewReader(in)
	username, err := r.ReadString('\n')
	if err != nil && username == "" {
		return "", "", fmt.Errorf("读取用户名失败: %w", err)
	}
	password, err := r.ReadString('\n')
	if err != nil && password == "" {
		return "", "", fmt.Errorf("读取口令失败: %w", err)
	}
	return finishAdminCredentials(
		strings.TrimSpace(username),
		strings.TrimRight(password, "\r\n"),
	)
}

// readSecret 读一行且**不回显**。
func readSecret(in *os.File, out io.Writer, prompt string) (string, error) {
	fmt.Fprint(out, prompt)
	b, err := term.ReadPassword(int(in.Fd()))
	// 补一个换行：回显关着时用户敲的回车不会换行。
	fmt.Fprintln(out)
	if err != nil {
		return "", fmt.Errorf("读取口令失败: %w", err)
	}
	return string(b), nil
}

// finishAdminCredentials 归一化并校验管理员凭据。
//
// 用户名留空 → 取默认值；口令留空 → 报错（**不**给默认口令：
// 静默给一个默认口令等于给实例留一把万能钥匙）。
//
// ⚠️ 口令两端的空格**不 trim**：那是用户有意敲的字符。
func finishAdminCredentials(username, password string) (string, string, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		username = defaultAdminUsername
	}
	if err := service.ValidateAdminCredentials(username, password); err != nil {
		return "", "", err
	}
	return username, password, nil
}

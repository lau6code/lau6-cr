package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/lau6code/lau6-cr/apps/api/internal/service"
)

// runAdmin 处理 admin 子命令。
func runAdmin(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "用法: lau6-cr admin integration-info [--show-secret]")
		os.Exit(2)
	}
	switch args[0] {
	case "integration-info":
		runIntegrationInfo(args[1:])
	case "help", "--help", "-h":
		fmt.Println("用法: lau6-cr admin integration-info [--show-secret]")
	default:
		fmt.Fprintf(os.Stderr, "未知子命令: admin %s\n\n", args[0])
		fmt.Fprintln(os.Stderr, "用法: lau6-cr admin integration-info [--show-secret]")
		os.Exit(2)
	}
}

// runIntegrationInfo 打印远程 distribution 的接入清单。
//
// 这是给运维看的：远程 distribution 上要配 JWKS 来源 + 事件回调地址与密钥，
// 这个命令把这几项一次性列全，免得去翻文档对地址。
func runIntegrationInfo(args []string) {
	showSecret := false
	for _, a := range args {
		switch a {
		case "--show-secret":
			showSecret = true
		default:
			fmt.Fprintf(os.Stderr, "未知参数: %s\n\n", a)
			fmt.Fprintln(os.Stderr, "用法: lau6-cr admin integration-info [--show-secret]")
			os.Exit(2)
		}
	}

	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "加载配置失败: %v\n", err)
		os.Exit(1)
	}
	info, err := service.BuildIntegrationInfo(cfg, showSecret)
	if err != nil {
		fmt.Fprintf(os.Stderr, "生成接入信息失败: %v\n", err)
		os.Exit(1)
	}
	printIntegrationInfo(info, showSecret)
}

// printIntegrationInfo 打印接入清单。
func printIntegrationInfo(info *service.IntegrationInfo, showSecret bool) {
	const line = "────────────────────────────────────────────"
	fmt.Println("远程 distribution 接入信息")
	fmt.Println(line)
	fmt.Printf("%s %s   (registry.upstream_addr)\n", padDisplay("上游地址", 16), info.UpstreamAddr)
	fmt.Printf("%s %s\n", padDisplay("JWKS HTTP 地址", 16), info.JWKSURL)
	fmt.Printf("%s %s   ← 复制到远程 distribution 即可\n", padDisplay("JWKS 文件", 16), info.JWKSPath)
	fmt.Printf("%s %s\n", padDisplay("回调地址", 16), info.CallbackURL)
	if showSecret {
		fmt.Printf("%s %s   (文件: %s)\n", padDisplay("回调密钥", 16), info.WebhookSecret, info.WebhookSecretPath)
	} else {
		fmt.Printf("%s %s\n", padDisplay("回调密钥", 16), info.WebhookSecretPath)
	}
	fmt.Println(line)
	fmt.Println("远程 distribution 的 config.yml 需要：")
	fmt.Println("  auth:")
	fmt.Println("    token:")
	fmt.Println("      jwks: <见上面两种方式>")
	fmt.Println("  notifications:")
	fmt.Println("    endpoints:")
	fmt.Println("      - name: lau6-cr-cache")
	fmt.Printf("        url: %s\n", info.CallbackURL)
	fmt.Println("        headers:")
	fmt.Println("          Authorization: [Bearer <webhook-secret 的值>]")
	fmt.Println("        events: [push, pull, delete, mount]")
	fmt.Println()
	fmt.Println("JWKS 来源两种方式（不确定目标 distribution 是否支持 url 形式时，用文件方式最稳）：")
	fmt.Printf("  1. HTTP   auth.token.jwks.url: %s\n", info.JWKSURL)
	fmt.Printf("  2. 文件   auth.token.jwks: <把 %s 复制到远程 distribution 能读到的位置>\n", info.JWKSPath)
	if showSecret {
		fmt.Println()
		fmt.Println("⚠️  警告：上面已打印回调密钥的明文值——它会进 shell 历史、终端回滚缓冲与日志采集器。")
		fmt.Println("⚠️  确认屏幕内容已清理，或改用不带 --show-secret 的方式手工拷贝密钥文件。")
	}
}

// padDisplay 按**显示宽度**右侧补空格（中文算 2 列）。
//
// fmt 的 %-14s 按**字符数**补齐，中文标签会歪——这里按终端列宽补。
func padDisplay(s string, width int) string {
	w := 0
	for _, r := range s {
		if isWideRune(r) {
			w += 2
		} else {
			w++
		}
	}
	if w >= width {
		return s
	}
	return s + strings.Repeat(" ", width-w)
}

// isWideRune 判断字符是否占 2 个终端列（东亚宽字符）。
func isWideRune(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x115F, // 谚文字母
		r >= 0x2E80 && r <= 0xA4CF, // CJK 部首、扩展 A、假名
		r >= 0xAC00 && r <= 0xD7A3, // 谚文音节
		r >= 0xF900 && r <= 0xFAFF, // CJK 兼容汉字
		r >= 0xFE30 && r <= 0xFE6F, // CJK 兼容形式
		r >= 0xFF00 && r <= 0xFF60, // 全角形式
		r >= 0xFFE0 && r <= 0xFFE6: // 全角符号
		return true
	}
	return false
}

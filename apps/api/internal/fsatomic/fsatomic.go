// Package fsatomic 提供原子的文件替换写入。
//
// 为什么单独一个包：仓库里有三四处"写临时文件 → rename 覆盖目标"的代码
// （签名私钥、用户密钥、application.yaml、distribution 配置），
// 收敛到一个实现，坑只踩一次。
//
// 覆盖必须走 rename，**不能先删目标文件**：
// POSIX 的 rename 本身就能覆盖，先删再 rename 一旦 rename 失败，
// 目标文件（密钥、配置）**永久丢失**，没有补救余地。
package fsatomic

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// Write 原子写文件：先写同目录下的临时文件，再 rename 覆盖目标。
//
// 目标文件在 rename 成功之前一直保持原样——
// 读者永远读不到半截内容，写失败也不会毁掉旧文件。
func Write(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("创建目录 %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("创建临时文件: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // rename 成功后这里是 no-op
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("写临时文件: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("同步临时文件: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭临时文件: %w", err)
	}
	if err := Replace(tmpName, path); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

// Replace 把临时文件 rename 成目标文件。
//
// ⚠️ **POSIX 上绝不先删目标文件**：
//
//	rename(2) 本身就能原子覆盖已存在的文件。
//	先删再改名会在"删完、rename 失败"之间留一个窗口——
//	目标文件（签名私钥、口令加密私钥、配置文件）就此丢失。
//
// Windows 的 rename 不能覆盖已存在的文件，只有那里才退回"先删后改名"。
func Replace(tmp, dst string) error {
	if runtime.GOOS != "windows" {
		if err := os.Rename(tmp, dst); err != nil {
			return fmt.Errorf("替换文件 %s: %w", dst, err)
		}
		return nil
	}
	// Windows：rename 不能覆盖已存在的文件。
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("替换文件 %s: %w", dst, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return fmt.Errorf("替换文件 %s: %w", dst, err)
	}
	return nil
}

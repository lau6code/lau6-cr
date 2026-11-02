package distribution

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// LoadConfig 读回 distribution 的配置文件。
//
// ⚠️ 它读的是**distribution 自己用的那一份**：
//
//   - embedded 形态：lau6-cr 启动时渲染出来的那份；
//   - external 形态：独立部署的 registry 用的那份。
//
// blob 回收必须与在跑的 registry 用同一份配置——
// GC 是按配置里的 storage 段去清理的，
// 配置不是同一份就会清到别的地方去（症状是"回收跑完了，桶里的东西还在"）。
func LoadConfig(path string) (*DistributionConfig, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取 distribution 配置 %s 失败: %w", path, err)
	}
	var c DistributionConfig
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("解析 distribution 配置 %s 失败: %w", path, err)
	}
	return &c, nil
}

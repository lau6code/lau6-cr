// Package config 负责加载 lau6-cr 的配置。
//
// 配置来源与优先级（高 → 低）：
//
//	环境变量 > system_settings 表 > /data/config/application.yaml > 代码内置默认值
//
// 启动时只读 YAML 与环境变量；system_settings 表由 settings 服务在运行时叠加，
// 并在保存后回写 /data/config/application.yaml。
//
// ⚠️ 镜像里**不烘焙任何配置文件**——默认值是代码里的 Default()，
//
//	运行期配置由 setup 写到 /data/config/application.yaml。
package config

import (
	"fmt"
	"gopkg.in/yaml.v3"
	"os"
)

// Load 按优先级合并配置：代码内置默认值 → YAML 文件 → 环境变量。
//
// 参数是候选 YAML 路径，按顺序后者覆盖前者；
// 文件不存在就跳过（不是错误）——运行期配置可能还没生成。
func Load(paths ...string) (*Config, error) {
	cfg := Default()
	for _, p := range paths {
		if p == "" {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("读取配置 %s: %w", p, err)
		}
		if err := yaml.Unmarshal(b, cfg); err != nil {
			return nil, fmt.Errorf("解析配置 %s: %w", p, err)
		}
	}
	if err := cfg.applyEnv(); err != nil {
		return nil, err
	}
	cfg.resolvePaths()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

package main

import (
	"testing"

	"github.com/lau6code/lau6-cr/apps/api/internal/config"
)

// TestDistributionConfigPathUsesConfig 覆盖 distribution 配置路径的取值来源。
//
// 路径必须来自**配置**（registry.distribution_config_path）：
// 渲染配置、拉起内嵌 distribution、跑 blob 回收三处共用同一个值，
// 否则回收会按另一份配置去删对象（症状是"回收跑完了，桶里的东西还在"）。
func TestDistributionConfigPathUsesConfig(t *testing.T) {
	cfg := config.Default()
	cfg.Registry.DistributionConfigPath = "/etc/registry/config.yml"
	if got := distributionConfigPath(cfg); got != "/etc/registry/config.yml" {
		t.Errorf("期望 /etc/registry/config.yml，实际 %q", got)
	}
}

// TestDistributionConfigPathDefault 覆盖默认值来自配置而非硬编码常量。
func TestDistributionConfigPathDefault(t *testing.T) {
	cfg := config.Default()
	if got := distributionConfigPath(cfg); got != "/run/distribution-config.yml" {
		t.Errorf("期望 /run/distribution-config.yml，实际 %q", got)
	}
}

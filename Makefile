.PHONY: help build build-api build-web build-dist embed-web test check-i18n check-crypto check-settings-keys vet fmt run dev up down logs clean setup smoke

API_DIR    := apps/api
WEB_DIR    := apps/web
WEB_DIST   := $(WEB_DIR)/dist
EMBED_DIST := $(API_DIR)/internal/web/dist
DIST_DIR   := apps/distribution-3.1.2
DIST_BIN   := bin/distribution
IMAGE      := lau6-cr:dev

help: ## 显示可用命令
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

# ⚠️ 顺序必须是 前端 → 拷贝 → 编译 Go：
#    go:embed 在**编译期**固化内容，反了就嵌进旧产物。
build: build-web embed-web build-api build-dist ## 构建前端 → 嵌入 → 编译后端 + distribution

build-api: ## 编译后端二进制到 bin/
	cd $(API_DIR) && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o ../../bin/lau6-cr ./cmd/lau6-cr

# 编译内嵌 distribution（仓库内核）。
#
# ⚠️ 依赖已 vendored（apps/distribution-3.1.2/vendor/）→ 构建不联网。
build-dist: ## 编译 distribution（仓库内核）到 bin/distribution
	cd $(DIST_DIR) && CGO_ENABLED=0 go build -mod=vendor -trimpath -ldflags="-s -w" -o ../../$(DIST_BIN) ./cmd/registry

build-web: ## 构建前端到 apps/web/dist
	cd $(WEB_DIR) && pnpm install --frozen-lockfile && pnpm run build

# 把前端产物拷进 Go 的嵌入目录。
#
# 真实产物落在 $(EMBED_DIST) 根下（index.html + assets/），
# 与版本库里的占位文件（placeholder/index.html、.gitkeep）并存——
# 占位文件**不能删**，否则干净 checkout 编译不过（go:embed 要求目录非空）。
embed-web: ## 把前端产物拷进 Go 嵌入目录（apps/api/internal/web/dist）
	find $(EMBED_DIST) -mindepth 1 -maxdepth 1 ! -name placeholder ! -name .gitkeep -exec rm -rf {} +
	cp -R $(WEB_DIST)/. $(EMBED_DIST)/

test: ## 跑后端测试
	cd $(API_DIR) && go test ./...

# 前端语言包一致性（键结构 / 引用完整性 / 占位符）。
# 前端没有单测框架，这个脚本是语言包唯一的自动化守门。
check-i18n: ## 校验三种语言包的一致性
	node scripts/check-i18n.mjs

# 口令传输协议的前后端互操作校验。
# 前端没有单测框架，这个脚本是前端加密实现唯一的自动化守门：
# 它用前端真实实现复现共享向量，Go 侧用同一份向量验解密。
check-crypto: ## 校验前后端口令传输协议一致
	node scripts/check-password-crypto.mjs

# 设置项清单一致性（后端定义 / 前端设置页 / 设计文档三处）。
# 漏一项 = 设置页上少一个入口，而用户看不到任何提示。
check-settings-keys: ## 校验设置项清单三处一致
	node scripts/check-settings-keys.mjs

vet: ## 静态检查
	cd $(API_DIR) && go vet ./...

fmt: ## 格式化 Go 代码
	cd $(API_DIR) && gofmt -w .

# ⚠️ 本地跑默认走**内嵌模式**（与容器里的形态一致）：
#    lau6-cr 自己拉起 bin/distribution，两者共享同一份数据目录，
#    blob 回收能直接用。地址不靠猜——由 registry.distribution_addr 推导。
#
#    要连**独立部署**的 distribution（本地 distribution 常用
#    http://127.0.0.1:5001）：
#      make run CR_REGISTRY_MODE=external CR_UPSTREAM_REGISTRY=http://127.0.0.1:5001
run: build-api build-dist ## 本地跑后端（内嵌 distribution）
	cd $(API_DIR) && CR_REGISTRY_MODE="$${CR_REGISTRY_MODE:-embedded}" \
		CR_DISTRIBUTION_BIN="$(CURDIR)/$(DIST_BIN)" \
		CR_UPSTREAM_REGISTRY="$${CR_UPSTREAM_REGISTRY}" \
		go run ./cmd/lau6-cr serve

dev: ## 本地开发（后端热重载）
	cd $(API_DIR) && CR_REGISTRY_MODE="$${CR_REGISTRY_MODE:-embedded}" \
		CR_DISTRIBUTION_BIN="$(CURDIR)/$(DIST_BIN)" \
		CR_UPSTREAM_REGISTRY="$${CR_UPSTREAM_REGISTRY}" \
		air -c .air.toml 2>/dev/null || (cd $(API_DIR) && CR_REGISTRY_MODE="$${CR_REGISTRY_MODE:-embedded}" \
		CR_DISTRIBUTION_BIN="$(CURDIR)/$(DIST_BIN)" \
		CR_UPSTREAM_REGISTRY="$${CR_UPSTREAM_REGISTRY}" go run ./cmd/lau6-cr serve)

setup: ## 交互式初始化（终端里输入管理员凭据）
	cd $(API_DIR) && go run ./cmd/lau6-cr setup

smoke: ## 端到端冒烟测试（对着已部署的实例跑）
	./scripts/smoke.sh

up: ## 构建并启动容器
	docker compose up -d --build

down: ## 停止容器
	docker compose down

logs: ## 跟踪容器日志
	docker compose logs -f lau6-cr

clean: ## 清理构建产物
	rm -rf bin $(WEB_DIST)
	find $(EMBED_DIST) -mindepth 1 -maxdepth 1 ! -name placeholder ! -name .gitkeep -exec rm -rf {} +

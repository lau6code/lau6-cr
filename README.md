<div align="center">

# lau6-cr

**轻量自托管私有镜像仓** · 一个镜像搞定：API + Web 界面 + 仓库内核 + 流量统计


[![Go](https://img.shields.io/badge/Go-1.26-00ADD8?style=flat-square&logo=go&logoColor=white)](https://go.dev/)
[![Vue](https://img.shields.io/badge/Vue-3.5-42b883?style=flat-square&logo=vuedotjs&logoColor=white)](https://vuejs.org/)
[![Element Plus](https://img.shields.io/badge/Element%20Plus-2.9-409EFF?style=flat-square)](https://element-plus.org/)
[![License](https://img.shields.io/badge/License-Apache--2.0-D22128?style=flat-square)](./LICENSE)
[![Registry](https://img.shields.io/badge/Registry%20API-v2-2496ED?style=flat-square&logo=docker&logoColor=white)](https://distribution.github.io/distribution/spec/api/)

</div>

```bash
docker compose up -d --build
# 打开 http://localhost:5000 → 按提示走 setup 引导
```
---
## 前言（制作此项目的缘由）
- 1、资源有限的情况下， harbor 也算重了一点点。
- 2、zot 和 distribution 都比较简陋，没有足够的仓库管理
- 3、dockery 功能实在是有点不能满足我的需要。够轻量，但是不够灵活。
- 4、包括 harbor 在内的一些项目密码未加密传输，很多企业的扫到密码不加密传输会要求整改。
---

## 📋 目录

- [项目简介](#-项目简介)
- [能力](#-能力)
- [界面一览](#-界面一览)
- [快速开始](#-快速开始)
- [架构](#-架构)
- [打包：单一二进制](#-打包单一二进制)
- [与 distribution 的关系](#-与-distribution-的关系)
- [配置](#-配置)
- [开发](#-开发)
- [设计文档](#-设计文档)
- [致谢](#-致谢)
- [许可](#-许可)

---

## 📖 项目简介

**lau6-cr** 是一个 Docker Registry v2 协议兼容的私有镜像仓，自带 Web 管理界面与项目制权限体系。

它不是 Harbor 的轻量替身，而是另一条路线：**单容器镜像交付，对外只有一个端口，全部流量（含镜像层拉取）都经过 lau6-cr 自己**。这正是它能精确统计流量、能在自己的进程里做权限判定的前提。

| 维度 | 选择                                                                           |
|---|------------------------------------------------------------------------------|
| 交付形态 | 单容器镜像（all-in-one），`lau6-cr` 是 PID 1，自己拉起内嵌的 distribution 子进程                 |
| 对外端口 | **只有一个**：host `:5000` → container `:5000`                                    |
| 仓库内核 | upstream `distribution/distribution:3.1.2`（不自研 blob/manifest 存储），**打进同一个镜像** |
| 镜像存储 | **本地文件系统 / S3 兼容对象存储**（MinIO / OSS / COS / S3 / R2）                          |
| 数据库 | **SQLite（默认）/ PostgreSQL / MySQL**                                           |
| 缓存 | **进程内 / Redis**                                                              |
| 组织模型 | **项目（Project）为第一分区**，项目下分公开仓库 / 私有仓库                                         |
| 认证 | 自签 Ed25519 JWT + JWKS，Docker CLI 与 Web UI **共用一套权限模型**                       |
| 后端 | Go（单静态二进制，无 CGO）                                                             |
| 前端 | Vue 3 + TypeScript + Element Plus + Vite                                     |

**它明确不做的事**（避免误用）：镜像扫描、镜像签名（cosign）、跨仓库复制、项目之上的组织层级、自定义 RBAC 规则编辑器、分布式 / HA。完整边界见 [`docs/design/README.md`](./docs/design/README.md) §4。

---

## ✨ 能力

| | |
|---|---|
| **协议** | Docker Registry HTTP API V2，兼容 `docker push` / `pull` / `login` |
| **组织** | 项目 → 成员（项目管理员 / 开发者 / 只读）→ 仓库（公开 / 私有） |
| **免登录** | 公开项目里的公开仓库可匿名 `docker pull` |
| **鉴权** | 自签 Ed25519 JWT + JWKS；Docker CLI 与 Web UI **共用同一套权限模型** |
| **访问令牌** | 个人令牌（`lau6_u_`）与项目令牌（`lau6_p_`），供 CI / 脚本推拉，不必交出账户口令 |
| **企业目录** | LDAP simple bind，首次登录自动建影子账号 |
| **数据库** | SQLite（默认）/ PostgreSQL / MySQL |
| **镜像存储** | blob 存在 distribution 那侧：本地文件系统 / S3 兼容对象存储，由配置文件或环境变量指定 |
| **缓存** | 进程内 / Redis |
| **统计** | 逐请求流量（拉取字节、推送字节、请求数），小时 + 天双粒度 |
| **保留策略** | 项目级 / 全局两级保留策略，按 tag 规则淘汰旧镜像 |
| **设置** | Web 界面可视化调整，**保存即热生效**，并回写 `application.yaml` |
| **审计** | 操作审计日志，只追加不可改 |

---

## 🖼️ 界面一览

|                                                |                                             |
|------------------------------------------------|---------------------------------------------|
| ![初始化向导](./docs/img/01-setup-01.png)           | ![初始化向导](./docs/img/01-setup-02.png)           |
| ![初始化向导](./docs/img/01-setup-03.png)           | ![初始化向导](./docs/img/01-setup-04.png)           |
| ![初始化向导](./docs/img/01-setup-05.png)           | ![初始化向导](./docs/img/01-setup-06.png)           |
| ![初始化向导](./docs/img/01-setup-07.png)           | ![登录](./docs/img/02-login.png)           | 
| ![镜像目录](./docs/img/03-catalog.png)             | ![仓库标签页](./docs/img/04-repository-tags.png) |
| ![项目列表](./docs/img/05-projects.png)            | ![成员管理](./docs/img/06-project-members.png)     |
|  ![项目访问令牌](./docs/img/07-project-tokens.png) |![项目保留策略](./docs/img/08-project-retention.png) |
|  ![个人访问令牌](./docs/img/09-tokens.png)         | ![用户管理](./docs/img/10-admin-users.png)         |
| ![系统设置](./docs/img/11-admin-settings.png)   |![镜像清理](./docs/img/12-admin-cleanup.png)       | 


---

## 🚀 快速开始

### setup 引导

```bash
docker compose up -d --build
# 打开 http://localhost:5000/setup，点「生成随机令牌」走完引导
```

### 使用

```bash
docker login cr.example.com -u admin
docker push cr.example.com/team-a/myapp:v1
```

先在 Web 界面建项目 `team-a`（项目名是 Docker 名称空间的**第一段**），
再把 `team-a` 加为项目成员，成员即可推拉。

### CI / 脚本推拉

长期凭据用**访问令牌**，不必把账户口令交出去：

```bash
# 个人令牌（lau6_u_）：权限随当前角色实时算，作用域 = 该用户有权限的所有仓库
docker login cr.example.com -u alice --password <个人令牌>

# 项目令牌（lau6_p_）：创建时选定项目内角色，固定不变，仅本项目生效
docker login cr.example.com -u <任意用户名> --password <项目令牌>
```

原始令牌**只返回一次**（库里只存 SHA-256 散列），取不回来。
令牌只对 registry 生效，**不能当 UI 登录口令**。

---

## 🏗️ 架构

```
                    外部端口 :5001 (host → :5000 container)
                             │
                      [ lau6-cr ]  ← 唯一对外入口，TLS 终结
      ┌─────────────┬─────────────┬────────────────┐
     /  静态      /token        /api/*           /v2/*
      │             │             │                 │
 内嵌前端产物   token realm    业务 API      统计代理 + 守卫
                                                │
                                                └──► distribution（子进程，同容器同数据目录）
                                 │
                            数据库 /data/db
                            jwt-private.pem
                            jwt-jwks.json
```

**一个镜像、两个进程**：`lau6-cr`（Go，PID 1）自己把 **distribution**（仓库内核）

### 路由一览

| 路径 | 用途 |
|---|---|
| `/` | 静态前端（SPA） |
| `/token` | Docker CLI token realm |
| `/v2/*` | Registry API —— manifest PUT 走守卫，其余流式代理 |
| `/api/v1/*` | 业务 API（项目、成员、仓库、令牌、统计、清理、审计、用户、设置） |
| `/api/internal/registry-events` | distribution 事件回调（共享密钥鉴权，不是 session） |
| `/setup/*` | 初始化向导；已初始化后由 handler 返 409 |
| `/jwks.json` | JWKS（**只有公钥**，公开端点） |
| `/healthz` `/readyz` | 探针 |

---

## 📦 打包：单一二进制

前端产物用 `embed.FS` **打进 Go 二进制**，交付物是**单一二进制**——
镜像里不再单独放静态文件目录。

构建顺序不能反（`go:embed` 在**编译期**固化内容）：

```
前端构建 (pnpm build)  →  拷进 apps/api/internal/web/dist  →  go build
```

`make build` 就是这三步。

---

## 🔗 与 distribution 的关系

lau6-cr 只做三件事：鉴权、权限判定、把 `/v2/*` 流式转发给 distribution。
blob 与 manifest 的存储全部交给 distribution。

| 形态 | 上游地址 | 说明 |
|---|---|---|
| **embedded**（默认） | 由 `registry.distribution_addr` 推导（回环 `127.0.0.1:5001`） | distribution 与 lau6-cr 同容器同数据目录。lau6-cr 渲染它的配置（`/run/distribution-config.yml`）并拉起子进程 |
| **external** | `CR_UPSTREAM_REGISTRY`（**无默认值**） | distribution 独立部署，用它自己的配置。**没给这个变量 `serve` 启动失败**并指名它 |


---

## ⚙️ 配置

优先级（高 → 低）：

```
环境变量  >  system_settings 表  >  /data/config/application.yaml  >  代码内置默认值
```

- `config.Default()` —— 代码内置的默认值
- `/data/config/application.yaml` —— setup 写入 / 系统设置页回写，**跨升级保留**
- `system_settings` 表 —— 运行时真源，Web 界面改这里，热生效

**环境变量的值写错 = 启动失败，不是"静默用默认值"。**
服务会一次性列全所有写错的变量（哪个变量、什么值、期望什么类型），改一轮就能修好。
被环境变量固定的设置项，在设置页里是**只读**的。
---

## 🛠️ 开发

```bash
make build      # 前端 → 拷进嵌入目录 → 编译后端（单一二进制）
make test       # 后端测试
make vet        # 静态检查
make run        # 本地跑后端
make up         # 容器跑全套
make smoke      # 端到端冒烟测试（对着已部署的实例跑）
```

前端没有单测框架，改动靠三个校验脚本守门：

```bash
make check-i18n          # 三种语言包键结构一致、引用完整、占位符一致
make check-crypto        # 前后端口令传输协议互操作（同一份共享向量）
make check-settings-keys # 设置项清单三处一致（后端定义 / 前端分组 / 设计文档）
```

目录：

```
apps/api/          Go 后端（chi + database/sql，零代码生成）
  cmd/lau6-cr/     入口
  internal/
    config/        配置加载（env + YAML）
    crypto/        Ed25519 密钥库、JWKS、JWT 签发验签
    store/         三方言数据库、迁移、仓储
    service/       权限引擎、token realm、registry 代理、设置、setup
    server/        路由与 handler
    cache/         进程内 / Redis
    ldap/          LDAP bind + RFC 4514 转义
    log/           日志器（stdout + 文件双写、轮转、保留期清理）
    distribution/  distribution 配置渲染 + 子进程管理
    gc/            blob 回收执行器
    objectstore/   S3 连通性探测
    web/           前端产物嵌入副本（embed.FS）
apps/web/          Vue 3 + TS + Element Plus + Vite
docker/            Dockerfile
docs/design/       设计文档（权威规格，索引见 docs/design/README.md）
scripts/smoke.sh   端到端冒烟测试
```

---

## 🙏 致谢

- [Docker Distribution](https://github.com/distribution/distribution) —— 仓库内核，
  blob / manifest 的存储与 GC 都由它承担
- [chi](https://github.com/go-chi/chi) —— HTTP 路由
- [golang-jwt](https://github.com/golang-jwt/jwt) —— JWT 签发与校验
- [go-ldap/ldap](https://github.com/go-ldap/ldap) —— LDAP 认证
- [minio-go](https://github.com/minio/minio-go) —— S3 兼容对象存储
- [go-redis](https://github.com/redis/go-redis) —— Redis 缓存
- [modernc.org/sqlite](https://gitlab.com/cznic/sqlite) —— 纯 Go SQLite 驱动（免 CGO）
- [Vue](https://github.com/vuejs/core) / [Element Plus](https://github.com/element-plus/element-plus) —— 前端
- [@noble/ciphers](https://github.com/paulmillr/noble-ciphers) / [@noble/hashes](https://github.com/paulmillr/noble-hashes) —— 口令传输的应用层加密

---

## 📄 许可

[Apache-2.0](./LICENSE)。

仓库内嵌了 `distribution` 3.1.2 的源码（`apps/distribution-3.1.2/`），
其许可同为 Apache-2.0，原文在该目录的 `LICENSE`。

---
## Star History

<a href="https://www.star-history.com/#lau6code/lau6-cr&Date">
<svg class="w-full h-full" width="600" height="400" preserveAspectRatio="xMidYMid meet" viewBox="0 0 600 400" style="stroke-width: 3; font-family: xkcd; background: white;"><filter id="xkcdify" filterUnits="userSpaceOnUse" x="-5" y="-5" width="100%" height="100%"><feTurbulence type="fractalNoise" baseFrequency="0.05" result="noise"></feTurbulence><feDisplacementMap scale="5" xChannelSelector="R" yChannelSelector="G" in="SourceGraphic" in2="noise"></feDisplacementMap></filter><style>
            @keyframes lobster-swim {
                0%, 100% { transform: translate(0, 0) rotate(0deg); }
                25% { transform: translate(2px, -3px) rotate(-5deg); }
                50% { transform: translate(0, -5px) rotate(0deg); }
                75% { transform: translate(-2px, -3px) rotate(5deg); }
            }
            .moltbot-emoji {
                animation: lobster-swim 1.5s ease-in-out infinite;
                transform-origin: center;
                transform-box: fill-box;
            }
        </style><g transform="translate(70,60)"><g pointer-events="all"><text transform="translate(450,330)" text-anchor="middle" style="font-size: 16px; fill: rgb(102, 102, 102);"><tspan>star-histor</tspan><tspan dx="-0.09em">y.com</tspan></text><image transform="translate(365,315)" height="20" width="20" href="data:image/png;base64,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"></image><g class="xkcd-chart-markers"></g><g class="xaxis" transform="translate(0,290)" fill="none" font-size="10" font-family="sans-serif" text-anchor="middle"><path class="domain" stroke="currentColor" d="M0.5,0.5H500.5" filter="url(#xkcdify)" style="stroke: black;"></path><g class="tick" opacity="1" transform="translate(0.5,0)"><line stroke="currentColor" y2="0"></line><text fill="currentColor" y="6" dy="0.71em" style="font-family: xkcd; font-size: 16px; fill: black;"> </text></g><g class="tick" opacity="1" transform="translate(78.66917413313865,0)"><line stroke="currentColor" y2="0"></line><text fill="currentColor" y="6" dy="0.71em" style="font-family: xkcd; font-size: 16px; fill: black;">0 days</text></g><g class="tick" opacity="1" transform="translate(156.8383482662773,0)"><line stroke="currentColor" y2="0"></line><text fill="currentColor" y="6" dy="0.71em" style="font-family: xkcd; font-size: 16px; fill: black;"> </text></g><g class="tick" opacity="1" transform="translate(235.00752239941593,0)"><line stroke="currentColor" y2="0"></line><text fill="currentColor" y="6" dy="0.71em" style="font-family: xkcd; font-size: 16px; fill: black;">0 days</text></g><g class="tick" opacity="1" transform="translate(313.1766965325546,0)"><line stroke="currentColor" y2="0"></line><text fill="currentColor" y="6" dy="0.71em" style="font-family: xkcd; font-size: 16px; fill: black;"> </text></g><g class="tick" opacity="1" transform="translate(391.3458706656932,0)"><line stroke="currentColor" y2="0"></line><text fill="currentColor" y="6" dy="0.71em" style="font-family: xkcd; font-size: 16px; fill: black;">0 days</text></g><g class="tick" opacity="1" transform="translate(469.51504479883187,0)"><line stroke="currentColor" y2="0"></line><text fill="currentColor" y="6" dy="0.71em" style="font-family: xkcd; font-size: 16px; fill: black;"> </text></g></g><g class="yaxis" fill="none" font-size="10" font-family="sans-serif" text-anchor="end"><path class="domain" stroke="currentColor" d="M-1,290.5H0.5V0.5H-1" filter="url(#xkcdify)" style="stroke: black;"></path><g class="tick" opacity="1" transform="translate(0,145.5)"><line stroke="currentColor" x2="-1"></line><text fill="currentColor" x="-7" dy="0.32em" style="font-family: xkcd; font-size: 16px; fill: black;"> </text></g></g><path class="xkcd-chart-xyline" d="M500,145Z" fill="none" stroke="#dd4528" filter="url(#xkcdify)"></path><g class="xkcd-chart-xycircle-group" filter="url(#xkcdify)" xy-group-index="0"><circle class="chart-tooltip-dot" r="1.75" cx="500" cy="145" pointer-events="all" style="stroke: rgb(221, 69, 40); fill: rgb(221, 69, 40);"></circle></g><g transform="translate(8, 5)"><g><rect fill-opacity="0.85" stroke="black" stroke-width="2" rx="5" ry="5" filter="url(#xkcdify)" width="138.6868438720703" height="32" x="0" y="0" style="fill: white;"></rect></g><g><rect width="8" height="8" rx="2" ry="2" filter="url(#xkcdify)" x="7" y="12" style="fill: rgb(221, 69, 40);"></rect><text x="21" y="20" style="font-size: 15px; fill: black;">lau6code/lau6-cr</text></g></g></g></g><svg x="15" y="10" style="visibility: hidden;"><rect fill-opacity="0.9" stroke="black" stroke-width="2" rx="5" ry="5" filter="url(#xkcdify)" width="25" height="30" x="5" y="5" style="fill: white;"></rect><text x="15" y="25" style="font-size: 15px; font-weight: bold; fill: black;"></text></svg><text x="50%" y="30" text-anchor="middle" style="font-size: 20px; font-weight: bold; fill: black;"><tspan>Star Histor</tspan><tspan dx="-0.09em">y</tspan></text><svg><defs><clipPath id="clip-circle-title"><circle r="11" cx="227" cy="23"></circle></clipPath></defs></svg><image x="216" y="12" height="22" width="22" href="https://avatars.githubusercontent.com/u/301942404?v=4" clip-path="url(#clip-circle-title)"></image><text x="50%" y="390" text-anchor="middle" style="font-size: 17px; fill: black;">Timeline</text><text text-anchor="end" dy=".75em" transform="rotate(-90)" y="24" style="font-size: 17px; fill: black;" x="-154">GitHub Stars</text></svg>
</a>
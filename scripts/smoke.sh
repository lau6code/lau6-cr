#!/usr/bin/env bash
# lau6-cr 端到端冒烟测试。
#
# 跑在**已部署**的实例上（默认 docker compose 起的 http://localhost:5001）。
# 每一步都真断言：任何一步不过就以非 0 退出，绝不"看起来通过了"。
#
# 用法：
#   ./scripts/smoke.sh                       # 只跑不需要凭据的检查
#   ADMIN_USERNAME=admin ADMIN_PASSWORD=xxx ./scripts/smoke.sh   # 加上登录检查
#   SMOKE_REPO=team-a/app SMOKE_PUSH=1 ./scripts/smoke.sh         # 加上推送/拉取回环
#
# 环境变量：
#   BASE_URL          实例根地址（默认 http://localhost:5001）
#   ADMIN_USERNAME    管理员账号（给了就查登录态）
#   ADMIN_PASSWORD    管理员密码
#   SMOKE_REPO        推送用的仓库名（形如 team-a/app），配合 SMOKE_PUSH=1
#   SMOKE_REGISTRY     推送用的 registry 主机（默认取 BASE_URL 的主机）
#   SMOKE_PUSH        置 1 时做真实的 docker push/pull 回环（需要本机有 docker）
#
# ⚠️ 推送回环会往目标仓库里写一个测试镜像（tag: smoke-<随机>），
#    验完不自动删除——删除要走保留策略，测试环境之外的实例别开这个开关。

set -uo pipefail

BASE_URL="${BASE_URL:-http://localhost:5001}"
ADMIN_USERNAME="${ADMIN_USERNAME:-}"
ADMIN_PASSWORD="${ADMIN_PASSWORD:-}"
SMOKE_REPO="${SMOKE_REPO:-}"
SMOKE_PUSH="${SMOKE_PUSH:-}"

PASS=0
FAIL=0

# ── 输出 ──────────────────────────────────────────────────────────
if [ -t 1 ]; then
  C_OK=$'\033[32m'; C_BAD=$'\033[31m'; C_DIM=$'\033[2m'; C_OFF=$'\033[0m'
else
  C_OK=""; C_BAD=""; C_DIM=""; C_OFF=""
fi

ok()   { PASS=$((PASS+1)); printf '  %s✔%s %s\n' "$C_OK" "$C_OFF" "$1"; }
bad()  { FAIL=$((FAIL+1)); printf '  %s✘%s %s\n' "$C_BAD" "$C_OFF" "$1"; }
head_() { printf '\n%s\n' "── $* ──────────────────────────────"; }

# check <名称> <期望状态码> <实际状态码> [详情]
check() {
  local name="$1" want="$2" got="$3" detail="${4:-}"
  if [ "$want" = "$got" ]; then
    ok "$name（$got）"
  else
    bad "$name：期望 $want，实际 $got ${detail}"
  fi
}

need() {
  if ! command -v "$1" >/dev/null 2>&1; then
    bad "缺少依赖命令：$1"
    exit 1
  fi
}

# ── 1. 健康检查 ──────────────────────────────────────────────────
head_ "1. 健康检查"
need curl
health_code=$(curl -sS -o /tmp/smoke-health.$$ -w '%{http_code}' "$BASE_URL/healthz" 2>/dev/null)
check "/healthz 返回 200" 200 "$health_code"
if grep -q '"status":"ok"' /tmp/smoke-health.$$ 2>/dev/null; then
  ok "/healthz 报 status=ok"
else
  bad "/healthz 响应不是 {\"status\":\"ok\"}：$(cat /tmp/smoke-health.$$ 2>/dev/null | head -c 200)"
fi
rm -f /tmp/smoke-health.$$

# ── 2. 初始化状态 ────────────────────────────────────────────────
head_ "2. 初始化状态"
setup_code=$(curl -sS -o /tmp/smoke-setup.$$ -w '%{http_code}' "$BASE_URL/setup/status" 2>/dev/null)
check "/setup/status 返回 200" 200 "$setup_code"
if grep -q '"initialized":true' /tmp/smoke-setup.$$ 2>/dev/null; then
  ok "实例已初始化"
  INITIALIZED=1
elif grep -q '"initialized":false' /tmp/smoke-setup.$$ 2>/dev/null; then
  bad "实例尚未初始化——打开 /setup 走网页引导，或执行 docker compose exec lau6-cr lau6-cr setup"
  INITIALIZED=0
else
  bad "/setup/status 响应异常：$(head -c 200 /tmp/smoke-setup.$$ 2>/dev/null)"
  INITIALIZED=0
fi
rm -f /tmp/smoke-setup.$$

# ── 3. 前端静态资源 ──────────────────────────────────────────────
head_ "3. 前端静态资源"
spa_code=$(curl -sS -o /tmp/smoke-spa.$$ -w '%{http_code}' "$BASE_URL/" 2>/dev/null)
check "首页返回 200" 200 "$spa_code"
if grep -qi '<div id="app">' /tmp/smoke-spa.$$ 2>/dev/null; then
  ok "首页是 SPA 入口（含 <div id=\"app\">）"
else
  bad "首页不是 SPA 入口，可能是占位页或静态资源没嵌进二进制"
fi
rm -f /tmp/smoke-spa.$$

# ── 4. registry 认证协商 ─────────────────────────────────────────
head_ "4. registry 认证协商"
auth_hdr=$(curl -sS -D - -o /dev/null "$BASE_URL/v2/" 2>/dev/null | tr -d '\r')
if echo "$auth_hdr" | grep -qi "^Www-Authenticate: Bearer"; then
  ok "/v2/ 返回 Bearer 认证挑战"
  realm=$(echo "$auth_hdr" | grep -i "^Www-Authenticate: Bearer" | sed -E 's/.*realm="([^"]*)".*/\1/')
  ok "token realm = ${realm:-<未解析>}"
else
  bad "/v2/ 没有返回 WWW-Authenticate: Bearer —— docker 客户端拿不到 token"
fi

# ── 5. 登录（给了凭据才跑）────────────────────────────────────────
if [ -n "$ADMIN_USERNAME" ] && [ -n "$ADMIN_PASSWORD" ]; then
  head_ "5. 登录与会话"
  login_code=$(curl -sS -c /tmp/smoke-cookie.$$ -o /tmp/smoke-login.$$ -w '%{http_code}' \
    -X POST -H 'Content-Type: application/json' \
    -d "{\"username\":\"$ADMIN_USERNAME\",\"password\":\"$ADMIN_PASSWORD\"}" \
    "$BASE_URL/api/v1/auth/login" 2>/dev/null)
  check "POST /api/v1/auth/login 返回 200" 200 "$login_code"
  me_code=$(curl -sS -b /tmp/smoke-cookie.$$ -o /tmp/smoke-me.$$ -w '%{http_code}' \
    "$BASE_URL/api/v1/auth/me" 2>/dev/null)
  check "GET /api/v1/auth/me 返回 200" 200 "$me_code"
  if grep -q '"anonymous":false' /tmp/smoke-me.$$ 2>/dev/null; then
    ok "会话有效（anonymous=false）"
  else
    bad "会话无效：$(head -c 200 /tmp/smoke-me.$$ 2>/dev/null)"
  fi
  rm -f /tmp/smoke-me.$$ /tmp/smoke-login.$$ /tmp/smoke-cookie.$$
else
  head_ "5. 登录与会话（跳过）"
  printf '  %s未提供 ADMIN_USERNAME / ADMIN_PASSWORD，跳过%s\n' "$C_DIM" "$C_OFF"
fi

# ── 6. docker push / pull 回环 ───────────────────────────────────
if [ "$SMOKE_PUSH" = "1" ]; then
  head_ "6. docker push / pull 回环"
  need docker
  if [ -z "$SMOKE_REPO" ]; then
    bad "SMOKE_PUSH=1 需要同时指定 SMOKE_REPO（形如 team-a/app）"
  else
    tag="smoke-$$"
    workdir=$(mktemp -d)
    printf 'lau6-cr smoke test\n' > "$workdir/hello.txt"
    printf 'FROM scratch\nCOPY hello.txt /hello.txt\n' > "$workdir/Dockerfile"
    host="${SMOKE_REGISTRY:-${BASE_URL#http://}}"; host="${host#https://}"
    image="$host/$SMOKE_REPO:$tag"

    # ⚠️ docker 的凭据是**按 registry 主机**存的：
    #    登录 127.0.0.1:5001 不会让 localhost:5001 免登录，
    #    匿名推私有仓库会被 401 打回。所以推送前自己登一次。
    if [ -n "$ADMIN_USERNAME" ] && [ -n "$ADMIN_PASSWORD" ]; then
      if printf '%s' "$ADMIN_PASSWORD" | docker login "$host" -u "$ADMIN_USERNAME" --password-stdin >/dev/null 2>&1; then
        ok "docker login $host"
      else
        bad "docker login $host 失败"
      fi
    fi

    if (cd "$workdir" && docker build -q -t "$image" . >/dev/null 2>&1); then
      ok "构建测试镜像 $image"
      if docker push "$image" >/dev/null 2>&1; then
        ok "push 成功"
        docker rmi -f "$image" >/dev/null 2>&1
        if docker pull "$image" >/dev/null 2>&1; then
          ok "pull 成功"
          # 校验内容真的回来了。
          # ⚠️ scratch 镜像没有 CMD，`docker create` 不给 entrypoint 会报
          #    "no command specified"——显式指定 entrypoint 即可（不执行，只建容器）。
          cid=$(docker create --entrypoint /hello.txt "$image" 2>/dev/null)
          if [ -n "$cid" ]; then
            if docker cp "$cid:/hello.txt" - 2>/dev/null | grep -q 'lau6-cr smoke test'; then
              ok "拉回的镜像内容与推上去的一致"
            else
              bad "拉回的镜像内容不对"
            fi
            docker rm -f "$cid" >/dev/null 2>&1
          else
            bad "docker create 失败"
          fi
        else
          bad "pull 失败"
        fi
        docker rmi -f "$image" >/dev/null 2>&1
      else
        bad "push 失败"
      fi
    else
      bad "构建测试镜像失败"
    fi
    rm -rf "$workdir"
  fi
else
  head_ "6. docker push / pull 回环（跳过）"
  printf '  %s未置 SMOKE_PUSH=1，跳过%s\n' "$C_DIM" "$C_OFF"
fi

# ── 汇总 ──────────────────────────────────────────────────────────
printf '\n──────────────────────────────────────────\n'
printf '通过 %s%d%s，失败 %s%d%s\n' "$C_OK" "$PASS" "$C_OFF" "$C_BAD" "$FAIL" "$C_OFF"
[ "$FAIL" -eq 0 ] || exit 1
exit 0

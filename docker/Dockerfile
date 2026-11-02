# lau6-cr 一体化镜像
# ============ Stage 1: web 前端 ============
FROM node:24-alpine AS web-builder
WORKDIR /w
RUN npm install -g pnpm@10
COPY apps/web/package.json apps/web/pnpm-lock.yaml* ./apps/web/
RUN cd apps/web && pnpm install --frozen-lockfile
COPY apps/web ./apps/web
RUN cd apps/web && pnpm run build

# ============ Stage 2: Go 后端 ============
FROM golang:1.26-alpine AS api-builder
WORKDIR /w
RUN go env -w GO111MODULE=on && go env -w GOPROXY=https://goproxy.cn
COPY apps/api/go.mod apps/api/go.sum ./
RUN go mod download
COPY apps/api ./
COPY --from=web-builder /w/apps/web/dist/ ./internal/web/dist/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/lau6-cr ./cmd/lau6-cr

# ============ Stage 3: distribution（仓库内核）============
FROM golang:1.26-alpine AS dist-builder
WORKDIR /w
RUN go env -w GO111MODULE=on && go env -w GOPROXY=https://goproxy.cn
COPY apps/distribution-3.1.2 ./
RUN CGO_ENABLED=0 go build -mod=vendor -trimpath -ldflags="-s -w" -o /out/distribution ./cmd/registry

# ============ Stage 4: 运行时 ============
FROM ubuntu:26.04
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
       ca-certificates tzdata wget \
    && rm -rf /var/lib/apt/lists/*
COPY --from=api-builder    /out/lau6-cr           /usr/local/bin/lau6-cr
COPY --from=dist-builder  /out/distribution      /usr/local/bin/distribution
ENV CR_REGISTRY_MODE=embedded

EXPOSE 5000
VOLUME /data

HEALTHCHECK --interval=30s --timeout=3s --start-period=30s --retries=3 \
    CMD wget -qO- http://127.0.0.1:5000/healthz >/dev/null 2>&1 || exit 1
ENTRYPOINT ["/usr/local/bin/lau6-cr"]
CMD ["serve"]

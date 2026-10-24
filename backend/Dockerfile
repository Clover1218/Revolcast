# ============================================================
# Revolcast Backend · 多阶段构建
#
# 构建：在项目根目录执行 docker compose build backend
# 关键点：
#   1. Go 版本必须与 backend/go.mod 的 `go` 指令一致，否则构建报
#      "go.mod requires go >= 1.x.x"。改版本只需覆盖 GO_VERSION 构建参数。
#   2. CGO 关闭：后端依赖（pgx / mongo-driver / go-redis）均为纯 Go，
#      可产出静态二进制，运行镜像不需要 glibc。
#   3. config.yaml 打进镜像作为兜底默认值；实际部署以环境变量 REVOLCAST_*
#      为准（viper 优先级：环境变量 > config.yaml > 代码默认值）。
# ============================================================

ARG GO_VERSION=1.26.5
ARG ALPINE_VERSION=3.20

# ---- 构建阶段 ----
FROM golang:${GO_VERSION}-alpine AS builder

ARG GOPROXY=https://goproxy.cn,direct

ENV CGO_ENABLED=0 \
    GOOS=linux \
    GOPROXY=${GOPROXY} \
    GOFLAGS=-mod=mod

WORKDIR /src

# 先拷依赖清单，利用层缓存：只改业务代码时无需重新下载依赖
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# -trimpath 去掉宿主机路径；-s -w 去掉符号表与调试信息
RUN go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

# ---- 运行阶段 ----
FROM alpine:${ALPINE_VERSION}

# ca-certificates：出网 TLS；tzdata：TZ 生效；nc（busybox）用于健康检查
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S revolcast && adduser -S -G revolcast revolcast

WORKDIR /app

COPY --from=builder /out/server /app/server
COPY config.yaml /app/config.yaml

# 日志目录（compose 把宿主机 ./data/logs/backend 挂载到这里）
RUN mkdir -p /app/logs && chown -R revolcast:revolcast /app

ENV TZ=Asia/Shanghai

USER revolcast

EXPOSE 8080

ENTRYPOINT ["/app/server"]

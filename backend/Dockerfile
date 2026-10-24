# ============================================================
# Revolcast Backend · 多阶段构建
#
# 构建：在项目根目录执行 docker compose build backend
#
# 关键点：
#   1. Go 版本必须与 backend/go.mod 的 `go` 指令一致，否则构建报
#      "go.mod requires go >= 1.x.x"。改版本只需覆盖 GO_VERSION 构建参数。
#   2. CGO 关闭：后端依赖（pgx / mongo-driver / go-redis）均为纯 Go，
#      可产出静态二进制，运行镜像不需要 glibc。
#   3. config.yaml 打进镜像作为兜底默认值；实际部署以环境变量 REVOLCAST_*
#      为准（viper 优先级：环境变量 > config.yaml > 代码默认值）。
#   4. 国内网络：apk 走 APK_MIRROR（默认阿里云），Go 依赖走 GOPROXY。
#      两者都可在 .env 覆盖；把 APK_MIRROR 设为空串则回退官方源。
# ============================================================

ARG GO_VERSION=1.26.5
ARG ALPINE_VERSION=3.20
ARG APK_MIRROR=mirrors.aliyun.com

# ---- 构建阶段 ----
# 本阶段不执行 apk add，依赖全部经 GOPROXY 拉取，因此不需要换 apk 源。
FROM golang:${GO_VERSION}-alpine AS builder

ARG GOPROXY=https://goproxy.cn,direct

# 不覆盖 GOSUMDB：goproxy.cn 支持 /sumdb 代理，校验数据也走代理，
# 无需直连 sum.golang.org（显式指定反而会变成直连，国内更容易卡）。
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

# ARG 在 FROM 之前声明只作用于 FROM 行，想在 RUN 里用必须重新声明
ARG APK_MIRROR

# 换 apk 源（国内访问 dl-cdn.alpinelinux.org 常年超时，这是最常见的卡死点）。
# 只替换域名，保留 /alpine/v3.xx/main 路径，换任何国内镜像都通用：
#   mirrors.aliyun.com / mirrors.tuna.tsinghua.edu.cn / mirrors.ustc.edu.cn
# APK_MIRROR 传空则跳过，走官方源。
RUN if [ -n "${APK_MIRROR}" ]; then \
        sed -i "s|dl-cdn.alpinelinux.org|${APK_MIRROR}|g" /etc/apk/repositories; \
    fi \
    && echo "--- apk repositories ---" && cat /etc/apk/repositories \
    && apk add --no-cache ca-certificates tzdata \
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

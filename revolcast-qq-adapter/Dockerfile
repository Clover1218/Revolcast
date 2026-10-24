# ============================================================
# Revolcast QQ Adapter · 多阶段构建
#
# 构建：在项目根目录执行 docker compose build qq-adapter
#
# 关键点：
#   1. Go 版本必须与 revolcast-qq-adapter/go.mod 的 `go` 指令一致（1.26.5）。
#   2. 已实测 CGO_ENABLED=0 可编译通过：go.mod 里的 mattn/go-sqlite3 是
#      ZeroBot-Plugin 的间接依赖，当前代码未 import，因此不需要 gcc。
#      若后续真正用到 sqlite（需要 CGO），需把构建阶段换成 golang:*-alpine
#      并 apk add build-base，同时去掉 CGO_ENABLED=0。
#   3. 本服务是「主动出站」形态：连 Backend WS + 连 NapCat OneBot WS，
#      自身不监听任何端口，因此不需要 EXPOSE / ports。
#   4. 国内网络：apk 走 APK_MIRROR（默认阿里云），Go 依赖走 GOPROXY。
# ============================================================

ARG GO_VERSION=1.26.5
ARG ALPINE_VERSION=3.20
ARG APK_MIRROR=mirrors.aliyun.com

# ---- 构建阶段 ----
# 本阶段不执行 apk add，依赖全部经 GOPROXY 拉取，因此不需要换 apk 源。
FROM golang:${GO_VERSION}-alpine AS builder

ARG GOPROXY=https://goproxy.cn,direct

# 不覆盖 GOSUMDB：goproxy.cn 支持 /sumdb 代理，校验数据也走代理。
ENV CGO_ENABLED=0 \
    GOOS=linux \
    GOPROXY=${GOPROXY} \
    GOFLAGS=-mod=mod

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN go build -trimpath -ldflags="-s -w" -o /out/adapter .

# ---- 运行阶段 ----
FROM alpine:${ALPINE_VERSION}

# ARG 在 FROM 之前声明只作用于 FROM 行，想在 RUN 里用必须重新声明
ARG APK_MIRROR

# 换 apk 源（国内访问 dl-cdn.alpinelinux.org 常年超时）。
# 只替换域名，保留 /alpine/v3.xx/main 路径，换任何国内镜像都通用：
#   mirrors.aliyun.com / mirrors.tuna.tsinghua.edu.cn / mirrors.ustc.edu.cn
RUN if [ -n "${APK_MIRROR}" ]; then \
        sed -i "s|dl-cdn.alpinelinux.org|${APK_MIRROR}|g" /etc/apk/repositories; \
    fi \
    && echo "--- apk repositories ---" && cat /etc/apk/repositories \
    && apk add --no-cache ca-certificates tzdata \
    && addgroup -S revolcast && adduser -S -G revolcast revolcast

WORKDIR /app

COPY --from=builder /out/adapter /app/adapter
COPY config.yaml /app/config.yaml

RUN mkdir -p /app/logs && chown -R revolcast:revolcast /app

ENV TZ=Asia/Shanghai

USER revolcast

ENTRYPOINT ["/app/adapter"]

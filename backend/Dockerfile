# ---- 构建阶段 ----
FROM golang:1.22-alpine AS builder

WORKDIR /app

# 先拷贝依赖清单，利用 Docker 层缓存加速构建
COPY go.mod go.sum* ./
RUN go mod download

# 拷贝源码并编译（CGO 关闭，生成静态二进制，便于瘦镜像）
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/server ./cmd/server

# ---- 运行阶段 ----
FROM alpine:3.20

WORKDIR /app
RUN apk add --no-cache ca-certificates tzdata

COPY --from=builder /out/server /app/server
COPY config.yaml /app/config.yaml

# 默认端口，可被 docker-compose / 环境变量覆盖
EXPOSE 8080

ENTRYPOINT ["/app/server"]

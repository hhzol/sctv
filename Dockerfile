# 1. 编译阶段
FROM golang:1.21-alpine AS builder
WORKDIR /build

# 在 builder 阶段预先安装证书和时区（后续复制给 scratch 镜像）
RUN apk --no-cache add ca-certificates tzdata

# 优化 Docker 缓存：先复制依赖配置，再下载依赖
COPY go.mod go.sum* ./
RUN go mod download

# 复制源码并编译
COPY main.go .
# -ldflags="-s -w": 去除符号表和调试信息，极大精简 Go 可执行文件体积
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o sctv .

# 2. 运行阶段 (使用绝对空白镜像 scratch)
FROM scratch

# 从 builder 中提取 HTTPS 请求必需的 SSL 证书和时区文件
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo

WORKDIR /app

# 复制编译好的二进制文件和配置文件
COPY --from=builder /build/sctv .
COPY interface.m3u .

EXPOSE 6622
CMD ["./sctv"]
# ---------- 构建阶段 ----------
FROM golang:1.25.10-alpine AS builder

WORKDIR /app

ENV GOPROXY=https://goproxy.cn,direct

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# 静态编译，产出单文件二进制，便于放到最小运行镜像中
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o main ./cmd/main.go

# ---------- 运行阶段 ----------
FROM alpine:latest

WORKDIR /app

# 时区与证书：调用外部大模型接口需要 CA 证书
RUN apk add --no-cache ca-certificates tzdata && \
    ln -sf /usr/share/zoneinfo/Asia/Shanghai /etc/localtime

COPY --from=builder /app/main .
# 后端直接托管前端静态页面（/ 与 /static）
COPY --from=builder /app/frontend ./frontend

ENV SERVER_PORT=8080

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:8080/api/ai/audit/info || exit 1

CMD ["./main"]

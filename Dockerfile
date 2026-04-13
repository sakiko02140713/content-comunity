FROM golang:1.21-alpine

WORKDIR /app

# 设置 Go 代理
ENV GOPROXY=https://goproxy.cn,direct
ENV GOSUMDB=off

COPY . .

CMD go run ./cmd/main.go
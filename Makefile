# =============================================================================
# 并行计算与AI应用 · 面向UGC社区的智能内容审核与发布系统
# 常用命令封装（Windows 下建议在 Git Bash / WSL 中执行）
# =============================================================================

.PHONY: help setup run dev ai test lint build up down logs clean fmt

help: ## 显示所有可用命令
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'

setup: ## 初始化配置文件（.env / ai-service/.env）
	@test -f .env || cp .env.example .env
	@test -f ai-service/.env || cp ai-service/.env.example ai-service/.env
	@echo "配置文件已就绪，请按需填写 DEEPSEEK_API_KEY"

run: ## 本地直接启动后端（读取 .env 中的 MySQL/Redis 地址）
	go run ./cmd/main.go

ai: ## 本地启动 Python AI 并行服务（默认 :8000）
	cd ai-service && python main.py

test: ## 运行 Go 单元测试（并行引擎、规则引擎、判定聚合）
	go test ./... -count=1

lint: ## 静态检查
	go vet ./...

fmt: ## 格式化代码
	gofmt -w ./cmd ./internal ./pkg

build: ## 编译后端二进制到 bin/
	go build -trimpath -ldflags="-s -w" -o bin/content-community ./cmd/main.go

up: ## 一键启动全部服务（MySQL + Redis + AI + 后端）
	docker compose up -d --build
	@echo "前端 http://localhost:8080 · AI 服务 http://localhost:8000/health"

down: ## 停止全部服务
	docker compose down

logs: ## 查看后端日志
	docker compose logs -f app

logs-ai: ## 查看 AI 服务日志
	docker compose logs -f ai-service

clean: ## 清空容器与数据卷（会删除数据库数据）
	docker compose down -v

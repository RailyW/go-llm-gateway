# 前端构建产物直接输出到后端 embed 目录
WEB_DIST := backend/internal/web/dist
BIN      := bin/gateway

.PHONY: help dev-backend dev-web web build run clean mock fmt tidy

help:
	@echo "make dev-backend  # 起后端 (:8080)，前端走 vite 代理"
	@echo "make dev-web      # 起前端 dev server (:5173)"
	@echo "make web          # 构建前端到 $(WEB_DIST)"
	@echo "make build        # 构建前端 + 单二进制 $(BIN)"
	@echo "make run          # 构建并运行"
	@echo "make mock         # 起一个 mock 上游 (:9911) 供联调"

dev-backend:
	cd backend && go run ./cmd/server

dev-web:
	cd web && npm run dev

web:
	cd web && npm install && npm run build

build: web
	mkdir -p bin
	cd backend && go build -o ../$(BIN) ./cmd/server
	@echo "==> $(BIN)"

run: build
	./$(BIN)

mock:
	python3 scripts/mock_upstream.py

fmt:
	cd backend && go fmt ./...

tidy:
	cd backend && go mod tidy

clean:
	rm -rf bin $(WEB_DIST)/assets

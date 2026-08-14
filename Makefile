# 前端构建产物直接输出到后端 embed 目录
WEB_DIST := backend/internal/web/dist
BIN      := bin/gateway

# go test 需要真 PG（测试在里面建/删独立 schema）。可用 .env 或环境变量覆盖。
TEST_DSN ?= postgres://gateway:gateway@127.0.0.1:5432/gateway_test?sslmode=disable

.PHONY: help db-up db-down dev-backend dev-web web build run clean mock fmt tidy test vet

help:
	@echo "make db-up        # 起 PostgreSQL (docker compose)"
	@echo "make db-down      # 停 PostgreSQL"
	@echo "make dev-backend  # 起后端 (:8080)，前端走 vite 代理"
	@echo "make dev-web      # 起前端 dev server (:5173)"
	@echo "make web          # 构建前端到 $(WEB_DIST)"
	@echo "make build        # 构建前端 + 单二进制 $(BIN)"
	@echo "make run          # 构建并运行"
	@echo "make mock         # 起一个 mock 上游 (:9911) 供联调"
	@echo "make test         # go test（需要 PG，见 TEST_DSN）"

db-up:
	docker compose up -d
	@echo "==> PostgreSQL :5432 (库 gateway / gateway_test，账号 gateway/gateway)"

db-down:
	docker compose down

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

test:
	cd backend && GATEWAY_TEST_DSN="$(TEST_DSN)" go test ./...

vet:
	cd backend && go vet ./...

fmt:
	cd backend && go fmt ./...

tidy:
	cd backend && go mod tidy

clean:
	rm -rf bin $(WEB_DIST)/assets

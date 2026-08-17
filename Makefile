# 前端构建产物直接输出到后端 embed 目录
WEB_DIST := backend/internal/web/dist
BIN      := bin/gateway

# go test 需要真 PG（测试在里面建/删独立 schema）。可用 .env 或环境变量覆盖。
TEST_DSN ?= postgres://gateway:gateway@127.0.0.1:5432/gateway_test?sslmode=disable
# 协调层（选主/广播）的测试需要 Redis；不设则相关测试自动跳过
TEST_REDIS ?=
TEST_REDIS_PASSWORD ?=

.PHONY: help cluster cluster-down db-up db-down dev-backend dev-web web build run clean mock fmt tidy test vet

help:
	@echo "make db-up        # 起 PostgreSQL (docker compose)"
	@echo "make db-down      # 停 PostgreSQL"
	@echo "make dev-backend  # 起后端 (:8080)，前端走 vite 代理"
	@echo "make dev-web      # 起前端 dev server (:5173)"
	@echo "make web          # 构建前端到 $(WEB_DIST)"
	@echo "make build        # 构建前端 + 单二进制 $(BIN)"
	@echo "make run          # 构建并运行"
	@echo "make mock         # 起一个 mock 上游 (:9911) 供联调"
	@echo "make test         # go test（需要 PG，见 TEST_DSN；Redis 测试需 TEST_REDIS）"
	@echo "make cluster      # 本地起 1 console + 2 gateway + 1 worker，验证多实例"
	@echo "make cluster-down # 停掉多实例"

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

cluster:
	bash scripts/cluster.sh up

cluster-down:
	bash scripts/cluster.sh down

mock:
	python3 scripts/mock_upstream.py

test:
	cd backend && GATEWAY_TEST_DSN="$(TEST_DSN)" \
		GATEWAY_TEST_REDIS_ADDR="$(TEST_REDIS)" \
		GATEWAY_TEST_REDIS_PASSWORD="$(TEST_REDIS_PASSWORD)" go test ./...

vet:
	cd backend && go vet ./...

fmt:
	cd backend && go fmt ./...

tidy:
	cd backend && go mod tidy

clean:
	rm -rf bin $(WEB_DIST)/assets

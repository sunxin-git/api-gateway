# api-gateway Makefile
#
# 默认 target：help
# 使用 `make <target>` 调用，Windows 用户请用 git-bash / WSL2 / msys2
#
# 所有变量可在调用时覆盖，例如：
#   make migrate-up PG_DSN="postgres://user:pwd@host:5432/db?sslmode=disable"

# ---------- 变量 ----------
# 本地默认 DSN，与 docker-compose 暴露端口对齐（55432 避开本地 PG）；CI / 生产请覆盖
PG_DSN ?= postgres://gateway:gateway_dev@localhost:55432/gateway?sslmode=disable

# 工具命令
#
# 工具链分两类：
#   1. sqlc — go.mod 锁定（tools.go），用 `go run` 调用，保证全员同版本
#   2. migrate / golangci-lint — 不走 tools.go（依赖体积爆炸，详见 tools.go 注释），
#      由 `make install-tools` 安装到 ./bin/（本地）或由 CI 用官方 action 安装
SQLC          := go run github.com/sqlc-dev/sqlc/cmd/sqlc

# 优先用 ./bin/<name>（本地 install-tools 装的），其次 PATH（CI 装到 PATH）
MIGRATE       := $(shell command -v ./bin/migrate 2>/dev/null || command -v migrate 2>/dev/null || echo migrate)
GOLANGCI_LINT := $(shell command -v ./bin/golangci-lint 2>/dev/null || command -v golangci-lint 2>/dev/null || echo golangci-lint)

# 工具版本锁定（与 CI 用同一版本，避免本地与 CI 漂移）
# golangci-lint v2.12.2 起用 Go 1.26 构建，能加载 go 1.25/1.26 项目的 config（v1.x build 用 go 1.23，不支持）
MIGRATE_VERSION       := v4.18.3
GOLANGCI_LINT_VERSION := v2.12.2

# 构建输出
BIN_DIR := bin

# ---------- 元 target ----------
.DEFAULT_GOAL := help
.PHONY: help build run test lint sqlc sqlc-compile sqlc-diff \
        migrate-up migrate-down migrate-create migrate-version \
        dev-up dev-down dev-clean dev-logs reimpl-guard tidy clean \
        install-tools install-migrate install-lint

## help: 列出所有 target
help:
	@echo "api-gateway 开发命令"
	@echo ""
	@echo "构建与运行："
	@echo "  build              编译 gateway + admin-cli 到 bin/"
	@echo "  run                运行 gateway（go run .）"
	@echo "  test               跑全部单测（-race -count=1）"
	@echo "  lint               跑 golangci-lint"
	@echo "  tidy               go mod tidy"
	@echo ""
	@echo "工具安装（首次或升级）："
	@echo "  install-tools      安装 migrate + golangci-lint 到 ./bin/"
	@echo "  install-migrate    单独安装 golang-migrate CLI ($(MIGRATE_VERSION))"
	@echo "  install-lint       单独安装 golangci-lint ($(GOLANGCI_LINT_VERSION))"
	@echo ""
	@echo "数据库工具链："
	@echo "  sqlc               sqlc generate"
	@echo "  sqlc-compile       sqlc compile（仅校验 yaml/查询语法，不生成代码）"
	@echo "  sqlc-diff          generate 后断言 git diff 为空（CI 用）"
	@echo "  migrate-up         应用所有未执行的 migrations"
	@echo "  migrate-down       回滚一步 migration（互动确认）"
	@echo "  migrate-version    打印当前 migration 版本号"
	@echo "  migrate-create name=<descr>  生成下一序号的 migration 文件"
	@echo ""
	@echo "本地开发环境："
	@echo "  dev-up             docker compose up -d"
	@echo "  dev-down           docker compose down"
	@echo "  dev-clean          docker compose down -v（连数据卷一起删，破坏性）"
	@echo "  dev-logs           docker compose logs -f"
	@echo ""
	@echo "合规检查："
	@echo "  reimpl-guard       grep 检测 new-api 路径残留（ADR-0001 兜底）"

## build: 编译 gateway + admin-cli 到 bin/
build:
	@mkdir -p $(BIN_DIR)
	go build -o $(BIN_DIR)/gateway .
	go build -o $(BIN_DIR)/admin-cli ./cmd/admin-cli
	@echo "构建完成：$(BIN_DIR)/gateway $(BIN_DIR)/admin-cli"

## run: 启动网关进程（前台）
run:
	go run .

## test: 跑所有单元测试（-p=1 顺序跑各包，避免并行时多包 pgxpool 加总超 PG max_connections=100）
test:
	go test ./... -race -count=1 -p=1

## lint: golangci-lint 全量检查
lint:
	$(GOLANGCI_LINT) run

## tidy: go mod tidy
tidy:
	go mod tidy

## sqlc: 生成 sqlc query 代码
sqlc:
	$(SQLC) generate -f sql/sqlc.yaml

## sqlc-compile: 仅校验 sqlc 配置与查询语法（不生成代码）
sqlc-compile:
	$(SQLC) compile -f sql/sqlc.yaml

## sqlc-diff: 跑 generate 后断言 internal/db/ 无未提交变更（CI 用）
sqlc-diff: sqlc
	@if [ -n "$$(git status --porcelain internal/db 2>/dev/null)" ]; then \
		echo "ERROR: sqlc generate 后 internal/db/ 有未提交变更；请运行 make sqlc 并提交"; \
		git status --short internal/db; \
		exit 1; \
	fi
	@echo "sqlc-diff OK：internal/db/ 与 source 一致"

## migrate-up: 应用所有未执行的 migrations
migrate-up:
	$(MIGRATE) -path migrations -database "$(PG_DSN)" up

## migrate-down: 回滚一步 migration（互动确认）
migrate-down:
	$(MIGRATE) -path migrations -database "$(PG_DSN)" down 1

## migrate-version: 打印当前 migration 版本号
migrate-version:
	$(MIGRATE) -path migrations -database "$(PG_DSN)" version

## migrate-create: 生成下一序号的 migration 文件（参数：name=<descr>）
migrate-create:
	@if [ -z "$(name)" ]; then echo "用法：make migrate-create name=<descr>"; exit 1; fi
	$(MIGRATE) create -ext sql -dir migrations -seq $(name)

## dev-up: 启动本地 PG + Redis
dev-up:
	docker compose up -d
	@docker compose ps

## dev-down: 停止本地 PG + Redis（保留数据卷）
dev-down:
	docker compose down

## dev-clean: 停止并删除数据卷（破坏性，将丢失本地 DB 数据）
dev-clean:
	docker compose down -v

## dev-logs: 跟随容器日志
dev-logs:
	docker compose logs -f

## reimpl-guard: ADR-0001 兜底，检测代码中是否出现 new-api 路径残留
reimpl-guard:
	@if grep -rE "QuantumNous/new-api|songquanpeng/one-api" --include="*.go" --exclude-dir=third-party --exclude-dir=node_modules . ; then \
		echo "ERROR: 检测到 new-api 残留 import / 字符串，违反 ADR-0001"; \
		exit 1; \
	fi
	@echo "reimpl-guard OK：未检测到 new-api 残留"

## clean: 删除 bin/ 输出
clean:
	rm -rf $(BIN_DIR)

## install-tools: 安装 migrate + golangci-lint 到 ./bin/
install-tools: install-migrate install-lint
	@echo "工具安装完成。请把 $$(pwd)/bin 加入 PATH，或直接用 make target（已配置 ./bin/ 优先）"

## install-migrate: 安装 golang-migrate CLI
install-migrate:
	@mkdir -p $(BIN_DIR)
	@echo "安装 golang-migrate $(MIGRATE_VERSION) -> $(BIN_DIR)/migrate"
	@OS=$$(uname -s | tr '[:upper:]' '[:lower:]'); \
	 ARCH=$$(uname -m); \
	 case "$$ARCH" in x86_64) ARCH=amd64;; aarch64|arm64) ARCH=arm64;; esac; \
	 case "$$OS" in mingw*|msys*|cygwin*) OS=windows; EXT=".exe"; ARCHIVE_EXT="zip";; *) EXT=""; ARCHIVE_EXT="tar.gz";; esac; \
	 URL="https://github.com/golang-migrate/migrate/releases/download/$(MIGRATE_VERSION)/migrate.$$OS-$$ARCH.$$ARCHIVE_EXT"; \
	 echo "下载: $$URL"; \
	 TMP=$$(mktemp -d); \
	 cd $$TMP && curl -sSL -o archive.$$ARCHIVE_EXT "$$URL" && \
	 if [ "$$ARCHIVE_EXT" = "zip" ]; then unzip -q archive.zip; else tar xzf archive.tar.gz; fi && \
	 mv migrate$$EXT "$(CURDIR)/$(BIN_DIR)/migrate$$EXT"; \
	 rm -rf $$TMP
	@$(BIN_DIR)/migrate -version

## install-lint: 安装 golangci-lint
install-lint:
	@mkdir -p $(BIN_DIR)
	@echo "安装 golangci-lint $(GOLANGCI_LINT_VERSION) -> $(BIN_DIR)/golangci-lint"
	@curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/$(GOLANGCI_LINT_VERSION)/install.sh \
		| sh -s -- -b $(BIN_DIR) $(GOLANGCI_LINT_VERSION)
	@$(BIN_DIR)/golangci-lint --version

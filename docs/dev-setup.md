# 本地开发环境上手指南

> 目标：让新接入开发者在 **5 分钟内** 启动一个空的 api-gateway 网关。

---

## 1. 前置条件

| 工具 | 版本 | 说明 |
|---|---|---|
| Go | ≥ 1.25 | `go version` 验证 |
| Docker | Desktop 4.x+ 或 Engine 24+ | 用于本地 PG + Redis |
| make | GNU make 3.81+ | macOS / Linux 自带；Windows 用 [git-bash](https://git-scm.com/download/win) / WSL2 / msys2 |
| git | 2.40+ | — |

**Windows 用户**：建议用 WSL2 或 git-bash 跑 Makefile target；PowerShell 不直接支持 `make` 与部分 unix 习惯（如 `mkdir -p`）。

---

## 2. 首次启动（5 分钟流程）

```bash
# 1. 复制环境变量样板，按需修改（KEK 等敏感项请生成随机值；勿用样板原值）
cp .env.example .env.local

# 2. 安装 migrate + golangci-lint 到 ./bin/（首次必需；之后跳过）
make install-tools

# 3. 启动本地 PG + Redis
make dev-up

# 4. 应用 schema migrations（创建 9 张 P0 表）
make migrate-up

# 5. 启动网关（前台）
make run
```

> **工具链说明**：sqlc 通过 `go.mod` 锁定（`tools.go` 模式），用 `make sqlc` 直接调用。
> `migrate` 与 `golangci-lint` 由于 transitive 依赖体积巨大（snowflake/spanner/clickhouse/cloud-sdk 等），不走 `tools.go`，改由 `make install-tools` 从 GitHub Release 下载预编译二进制到 `./bin/`。CI 用对应官方 action。

完成后浏览器访问：

- <http://localhost:8080/healthz> → `{"status":"ok",...}`
- <http://localhost:8080/readyz>  → `200 OK`
- <http://localhost:8080/metrics> → Prometheus 文本格式

> **端口说明**：docker-compose 主机暴露端口是 **55432**（PG）/ **56379**（Redis），避免与本地 PG/Redis 服务冲突。容器内仍是默认 5432/6379。`Makefile` 中默认 `PG_DSN` 已对齐。如需改回 5432，修改 `docker-compose.yml` + `Makefile` 中 PG_DSN。

---

## 3. 常用命令速查

| 目的 | 命令 |
|---|---|
| 列出所有 make target | `make` 或 `make help` |
| 编译产物到 `bin/` | `make build` |
| 跑全部测试 | `make test` |
| 跑 lint | `make lint` |
| 生成 sqlc 代码 | `make sqlc` |
| 跑 migration 上一步 | `make migrate-up` |
| 跑 migration 回滚一步 | `make migrate-down` |
| 查看当前 migration 版本 | `make migrate-version` |
| 新增 migration 文件 | `make migrate-create name=add_xxx_index` |
| 停止本地容器（保留数据） | `make dev-down` |
| 销毁本地数据卷（**破坏性**） | `make dev-clean` |
| 跟踪容器日志 | `make dev-logs` |
| 跑 reimplement 守门 | `make reimpl-guard` |

---

## 4. 排查清单

### 4.1 `make dev-up` 失败：端口 5432/6379 被占用

```bash
# Linux/macOS
lsof -i :5432
lsof -i :6379
# Windows
netstat -ano | findstr "5432"
netstat -ano | findstr "6379"
```

解决：要么停掉占用端口的本地 PG/Redis 进程，要么修改 `docker-compose.yml` 端口映射。

### 4.2 `make migrate-up` 失败：`connection refused`

PG 容器尚未就绪。等 10 秒后重试，或：

```bash
docker compose ps              # 看 postgres 列是否 healthy
docker compose logs postgres   # 查启动日志
```

### 4.3 `make run` 失败：缺环境变量

进程因 `.env.local` 缺关键字段而 fail-fast 退出（设计如此）。检查：

- `PG_DSN` 是否设置
- `GATEWAY_KEK_V1`（**必须 32 字节 base64**，可用 `openssl rand -base64 32` 生成）
- `ADMIN_TOKEN_SIGNING_KEY`（可用 `openssl rand -hex 32` 生成）

### 4.4 `make sqlc` 失败：query 语法错误

检查 `sql/queries/*.sql` 文件；sqlc 在 PG 模式下要求 query 通过 PG 解析器，因此本地必须有 PG 容器在跑（`make dev-up`）才能 verify。

### 4.5 Windows 下 make 报「无法识别的命令」

请用 git-bash 或 WSL2 运行，不要在 cmd / PowerShell 中直接调用 make。

---

## 5. 文件树速览

参见根目录 `README.md` 或本计划文档：[`docs/plans/2026-05-26-001-feat-phase-1-skeleton-and-migrations-plan.md`](plans/2026-05-26-001-feat-phase-1-skeleton-and-migrations-plan.md)。

关键目录：

- `internal/<subsystem>/`：领域子系统，Phase 2+ 落地
- `sql/queries/`：sqlc query 源
- `migrations/`：golang-migrate 风格 `NNNN_*.up.sql` / `.down.sql`
- `cmd/admin-cli/`：运维 CLI
- `docs/`：所有人类可读文档
- `docs/db/`：每版 schema 快照

---

## 6. 进一步阅读

- 项目宪法：[`CLAUDE.md`](../CLAUDE.md)
- 主设计文档：[`docs/multimedia-gateway-design.md`](multimedia-gateway-design.md)
- 5 份 ADR：[`docs/adr/`](adr/)
- 术语表：[`CONTEXT.md`](../CONTEXT.md)
- 当前 schema 快照：[`docs/db/schema-v0001.md`](db/schema-v0001.md)（U7 完成后存在）

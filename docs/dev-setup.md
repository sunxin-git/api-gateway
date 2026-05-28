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

## 5. 运维场景

### 5.1 Migration 部署流程（生产环境关键）

生产环境跑 migration 前**必须**先暂停 reconciler，避免 ALTER TABLE 的 ACCESS EXCLUSIVE 锁与 reconciler 全表 SELECT 死锁。

```bash
# 方式 A（Linux / macOS）：SIGUSR1 暂停 reconciler，不停整个 gateway
kill -SIGUSR1 <gateway-pid>

# 等 5s 让正在跑的 reconciler 轮结束，然后跑 migration
sleep 5
make migrate-up

# 完成后再次 SIGUSR1 恢复 reconciler
kill -SIGUSR1 <gateway-pid>
```

```bash
# 方式 B（跨平台 / Windows 必用）：直接停 gateway 进程
systemctl stop api-gateway   # 或 docker stop / pkill 视部署方式
make migrate-up
systemctl start api-gateway
```

**关于 0002 down migration**：

- 0002 down.sql 首行 DO 块自动检查 `business_account_ledger` 是否有数据，**有数据则 RAISE 异常**拒绝执行
- 生产环境 rollback **永远走代码热修**，不要回退 schema（DROP COLUMN 会丢审计字段）

### 5.2 Rebuild stuck 恢复 runbook（账户陷 frozen+rebuild_in_progress 时）

收到告警 `gateway_ledger_rebuild_stuck_total{account_id=X} > 0` 时：

1. 查 slog 找到 rebuild 失败的根因（多为高并发 CAS 重试耗尽）
2. SIGUSR1 暂停 reconciler，避免它继续干扰
3. 临时 Go 脚本调用 `LedgerService.RebuildBalance(ctx, actor={system:ops}, accountID)`
   - P0 阶段无 admin-cli 子命令；未来 D-min HTTP / P1 admin-cli 会加暴露
4. 验证 `GetBalance` 返回正确投影 + 账户 unfrozen
5. SIGUSR1 恢复 reconciler
6. 在 `docs/solutions/` 写入复盘（一次性事故 + 根因 + 永久修复方案）

### 5.3 Drift 检测切换流程（P0 dry-run → P1 freeze）

- P0 部署默认 `LEDGER_DRIFT_ACTION=log`（dry-run）
- 生产跑 1-2 周观察 `gateway_ledger_drift_total{action=dry_run}` 与 `gateway_ledger_drift_false_positive_total`
- 14 天零 drift（false positives < 1% 也可接受）→ 切 `LEDGER_DRIFT_ACTION=freeze` 重启
- 切换前后都有 page 告警（不同 severity）防真 drift 被吞噬

---

## 6. Relay 业务调用 quickstart（验证上游 provider 闭环）

> 目标：本地起网关 → 用 business key 调 `POST /v1/chat/completions` → 真实命中上游 provider → 验证按 usage 扣费闭环。
> 完整对外契约见 [`docs/api/business-api.md`](api/business-api.md)。

### 6.1 配置 `.env.local` relay 块

在 §2 已能启动空网关的基础上，补 relay 配置（默认 `GATEWAY_RELAY_ENABLED=false` 时 `/v1` 不注册，调用返 404）：

```bash
GATEWAY_ENV=dev                                   # dev 不强制上游 https / TLS / 反代
GATEWAY_TOKEN_PEPPER=<openssl rand -hex 32>       # admin token + business key 共享，全环境必填
GATEWAY_RELAY_ENABLED=true
GATEWAY_RELAY_MODEL_NAME=gw-default               # 业务可见 model 名
GATEWAY_RELAY_UPSTREAM_PROVIDER_TYPE=openai_compat
GATEWAY_RELAY_UPSTREAM_BASE_URL=https://ark.cn-beijing.volces.com/api/v3   # 火山 ARK 示例
GATEWAY_RELAY_UPSTREAM_API_KEY=<你的上游 API key>
GATEWAY_RELAY_UPSTREAM_MODEL_NAME=doubao-seed-2-0-pro-260215               # 上游真实 model 名
GATEWAY_RELAY_PRICE_INPUT_PER_1M_MINOR=800        # ¥8 / 1M tok
GATEWAY_RELAY_PRICE_OUTPUT_PER_1M_MINOR=2000      # ¥20 / 1M tok
GATEWAY_RELAY_MAX_CONTEXT_TOKENS=32768
```

> 8 个 `GATEWAY_RELAY_*` 业务字段在 `ENABLED=true` 时全部必填；任一非法/缺失 → 进程 fail-fast 拒启动。`UPSTREAM_API_KEY` 写 `.env.local`（gitignore），勿提交。

### 6.2 准备业务账户 + 充值 + business key

`admin-cli` 直连 PG（同样读 `.env.local` 的 `PG_DSN`），无需 admin token：

```bash
make build   # 产出 bin/gateway + bin/admin-cli
make migrate-up   # 确保 schema ≥ 0004（business_account_api_key 表）

# 1) 建业务账户
./bin/admin-cli account create --id smoke-ark-001

# 2) 充值 ¥1000（100000 分）；idempotency-key 任意唯一串
./bin/admin-cli account recharge --id smoke-ark-001 --amount 100000 --idempotency-key smoke-topup-001

# 3) 建 business key（plaintext 仅本次返回，立即复制）
./bin/admin-cli business-key create \
    --description smoke-ark-key --business-account-id smoke-ark-001 --rpm 600
# → stdout JSON 的 "plaintext" 字段即业务调用用的 Bearer key
```

### 6.3 起网关 + 调用

```bash
# 前台起（或 ./bin/gateway &）
make run

# 等就绪后另开终端调用（把 <PLAINTEXT> 换成上一步的 key）
curl -sS -X POST http://127.0.0.1:8080/v1/chat/completions \
    -H "Authorization: Bearer <PLAINTEXT>" \
    -H "Content-Type: application/json" \
    -d '{"model":"gw-default","messages":[{"role":"user","content":"Reply with exactly: PONG"}],"max_tokens":32}'
```

启动日志应有 `业务 relay 路由已注册 path=/v1/chat/completions`；`/readyz` 应为 `ready`。
返回 `200` + OpenAI 兼容 body（含 `choices` / `usage`）即上游闭环打通。

> **Windows git-bash 提示**：`curl -d` 直接带中文可能因终端编码（GBK）发出乱码，上游会"看不懂"。验证闭环用纯 ASCII prompt，或把请求体写入 UTF-8 文件再 `--data @body.json`。网关对 body 原样透传，乱码非网关问题。

### 6.4 验证扣费闭环（dev 直查 PG）

MVP 业务侧无独立余额接口（余额由运维经 [Admin API](api/admin-api.md) 查）；dev 环境可直接查库验证两步式扣费：

```bash
# 余额投影：available 应 = recharge - used_total
docker exec api-gateway-pg psql -U gateway -d gateway -tAc \
  "SELECT available, reserved, used_total, recharge_total FROM business_account_balance WHERE business_account_id='smoke-ark-001';"

# ledger 流水：应见 recharge → reserve → commit 三条（reserve/commit 的 actor_type=business_key）
docker exec api-gateway-pg psql -U gateway -d gateway -c \
  "SELECT id, entry_type, amount, available_delta, used_delta, reference_type, actor_type FROM business_account_ledger WHERE business_account_id='smoke-ark-001' ORDER BY id;"
```

预期：`reserve`（按 input 估算 + max_tokens 上界预扣）→ 上游 200 后 `commit`（按真实 `usage` 结算，多扣自动释放）；账本不变量 `available + reserved + used_total = recharge_total` 恒成立。

---

## 7. 文件树速览

参见根目录 `README.md` 或本计划文档：[`docs/plans/2026-05-26-001-feat-phase-1-skeleton-and-migrations-plan.md`](plans/2026-05-26-001-feat-phase-1-skeleton-and-migrations-plan.md)。

关键目录：

- `internal/<subsystem>/`：领域子系统，Phase 2+ 落地
- `sql/queries/`：sqlc query 源
- `migrations/`：golang-migrate 风格 `NNNN_*.up.sql` / `.down.sql`
- `cmd/admin-cli/`：运维 CLI
- `docs/`：所有人类可读文档
- `docs/db/`：每版 schema 快照

---

## 8. 进一步阅读

- 项目宪法：[`CLAUDE.md`](../CLAUDE.md)
- 主设计文档：[`docs/multimedia-gateway-design.md`](multimedia-gateway-design.md)
- 5 份 ADR：[`docs/adr/`](adr/)
- 术语表：[`CONTEXT.md`](../CONTEXT.md)
- 当前 schema 快照：[`docs/db/schema.md`](db/schema.md)
- 业务调用契约：[`docs/api/business-api.md`](api/business-api.md)
- 运维侧管理契约：[`docs/api/admin-api.md`](api/admin-api.md)
- 计划归档：[`docs/plans/`](plans/)

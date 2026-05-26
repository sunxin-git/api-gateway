# api-gateway

> 商业化「多媒体 AI 网关 + 计费系统」。
> 作为多媒体创作平台的基础设施层，承载图像 / 视频 / 音频 / LLM 等模型调用 + 上游成本透明记账 + 客户级倍率/积分阶梯计费 + Webhook 反向通知。

**当前阶段**：Phase 1（项目骨架 + 初始 PG migrations）已完成。Phase 2 起进入业务工作流（账本 E / 路由 B-min / outbox C-min / Admin API D-min）。

---

## 快速上手（5 分钟）

```bash
cp .env.example .env.local      # 1. 环境变量样板
make install-tools              # 2. 装 migrate + golangci-lint
make dev-up                     # 3. 启动本地 PG + Redis
make migrate-up                 # 4. 应用 schema
make run                        # 5. 启动网关
```

完整说明：[`docs/dev-setup.md`](docs/dev-setup.md)

---

## 技术栈

| 层 | 选型 | ADR |
|---|---|---|
| 后端 | Go 1.25 + Gin v1.10+ + sqlc + pgx/v5 | — |
| 数据库 | PostgreSQL ≥ 15（单选） | [0002](docs/adr/0002-postgresql-only-no-multi-db.md) |
| 数据访问 | sqlc + database/sql（不用 GORM） | [0003](docs/adr/0003-sqlc-instead-of-gorm.md) |
| 任务队列 | Asynq + Redis（P1 引入） | — |
| 前端 | React 19 + Vite + shadcn/ui + Tailwind 4 + pnpm | [0005](docs/adr/0005-frontend-stack-industry-mainstream.md) |
| 观测 | log/slog (JSON) + Prometheus + OpenTelemetry | — |

---

## 仓库布局

```
api-gateway/
├── main.go                 # HTTP 服务入口
├── cmd/admin-cli/          # 运维 CLI（Cobra）
├── internal/               # 领域子系统
│   ├── config/             # koanf 配置加载
│   ├── obs/                # slog / metrics / tracing
│   ├── httpapi/            # HTTP 服务器 + middleware 套件
│   ├── db/                 # sqlc 生成代码（**勿手改**）
│   ├── ledger/             # Phase 2 工作流 E
│   ├── routing/            # Phase 2 工作流 B-min
│   ├── outbox/             # Phase 2 工作流 C-min
│   ├── admin/              # Phase 2 工作流 D-min
│   ├── relay/              # Phase 3 provider adapter
│   ├── auth/               # Phase 3 admin token / session
│   └── crypto/             # P1 envelope encryption
├── sql/
│   ├── sqlc.yaml
│   └── queries/            # sqlc query 源
├── migrations/             # golang-migrate NNNN_*.{up,down}.sql
├── web/admin/              # React 19 UI（UI Phase 初始化）
└── docs/                   # 所有人类可读文档
```

---

## 文档导航

- 项目宪法 / 第一性原理：[`CLAUDE.md`](CLAUDE.md)
- 术语表：[`CONTEXT.md`](CONTEXT.md)
- 主设计文档 v1.3：[`docs/multimedia-gateway-design.md`](docs/multimedia-gateway-design.md)
- 5 份 ADR：[`docs/adr/`](docs/adr/)
- Schema 快照 v0001：[`docs/db/schema-v0001.md`](docs/db/schema-v0001.md)
- 本地开发指南：[`docs/dev-setup.md`](docs/dev-setup.md)
- 计划归档：[`docs/plans/`](docs/plans/)
- Codex 评审历史：[`docs/reviews/`](docs/reviews/)

---

## 许可与合规

本项目 **reimplement-only**（详见 [ADR-0001](docs/adr/0001-reimplement-only-no-fork-new-api.md)）：
代码 100% 自写，不基于 new-api (AGPLv3) 二次开发，仅参考其架构思想。
`third-party/new-api/` 目录仅作只读参考，**永不入仓**（`.gitignore` 已生效）、**永不 import**（golangci-lint depguard + CI grep 双重守门）。

---

> 作者：sunxin (<wzzwj2026@gmail.com>) · GitHub：[sunxin-git/api-gateway](https://github.com/sunxin-git/api-gateway)

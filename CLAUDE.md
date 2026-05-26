## Agent skills

### Issue tracker

Issues live in GitHub Issues for `sunxin-git/api-gateway` (uses `gh` CLI). See `docs/agents/issue-tracker.md`.

### Triage labels

Default canonical labels (`needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`) used verbatim. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context layout (`CONTEXT.md` + `docs/adr/` at repo root). See `docs/agents/domain.md`.

---

# 项目开发总纲（v1.0 — 2026-05-26）

> 本节是 api-gateway 项目所有 AI Agent + 人类工程师必读的开发约束。
> 写代码、写文档、做评审前先回到此处校准。
> 与上面 `## Agent skills` 共存，不冲突。

## 一、产物语言：统一中文

| 产物 | 语言 |
|---|---|
| 所有面向人的文档（设计文档、ADR、CONTEXT、运营 SOP、CHANGELOG） | **中文** |
| 代码注释（含函数 doc / 行内注释 / TODO） | **中文** |
| Commit message + PR 描述 + Issue | **中文** |
| AI Agent 中间推理过程 / 状态汇报 / 错误说明 | **中文** |
| 终端用户面向的 UI 文案 | 走 i18next，**zh + en 双语**，源串为英文键名 |
| **保留英文**：变量 / 函数 / 类型 / SQL 关键字 / 包名 / 外部 API 错误码常量 / Prometheus 指标名 / OpenTelemetry attribute |

**理由**：项目主作者母语中文，所有中间产物用中文降低沟通损耗；UI 文案双语是商业平台标配；标识符用英文是工程惯例。

---

## 二、七条第一性原理（写代码 / 文档 / 决策前必读）

### 1. 不假设你清楚自己想要什么
动机或目标不清晰时，**停下来讨论**。
- 触发场景：用户说"加一个功能"但没说为什么、谁会用、和现有什么关联
- 正确动作：用 1-2 个澄清问题问出真实动机；不要假设并开干

### 2. 目标清晰但路径不是最短的，直接说更好的办法
不为了"照办"而照办；看到弯路立即提出。
- 触发场景：用户说"用 X 实现 Y"，但 Z 明显更直接
- 正确动作：先说"X 可以做但 Z 更短，区别是…"，让用户选

### 3. 追根因，不打补丁
每个修复都要回答"**为什么这里坏了**"。
- 反例：测试挂了，加 `if err == nil { return }` 让它通过 ❌
- 正例：测试挂了，定位到 ledger CAS 在并发下少了 version 字段，补 version 字段并加并发测试 ✅

### 4. 输出说重点，砍掉一切不改变决策的信息
回答 / 报告 / PR 描述都遵守"如果删掉这段，决策会变吗？"
- 不变 → 删
- 变 → 留

### 5. reimplement 纪律（ADR-0001 硬约束）
**不复制 `third-party/new-api/` 的代码**。详见 ADR-0001 第 "操作纪律" 节。
- 写代码窗口不打开 `third-party/new-api/*.go`
- 需要查上游 provider 协议 → 看 provider **官方文档**，不看 new-api adapter 实现
- 需要查 new-api 实现思路 → 单独开 Agent 总结架构后再写新代码
- PR 自检：本 PR 是否引入了 new-api 代码片段？引入则拒绝合并

### 6. 商业平台优先级：稳定 > 正确 > 性能 > 优雅
四者冲突时按此顺序取舍。
- 例：sqlc 生成代码不够优雅但稳定，**不**手工优化引入风险
- 例：账本 CAS 写法不是最快但正确，**不**为性能引入复杂的乐观锁优化
- 例：日志 JSON 化稍慢但便于排查，**不**用纯文本日志节省 CPU

### 7. AI Agent 协作纪律
- 大任务用 Agent，小任务用直接工具（Read / Edit / Bash）
- 并行 Agent 不超过 2-3 个，避免 review 容量过载
- Agent 完成后必须 milestone review，**不串联多个 Agent 不看中间产出**
- Agent 写代码后人类必须扫一遍关键变更（不能盲信"Agent 说做完了"）

---

## 三、技术栈硬约束

详见 [`docs/multimedia-gateway-design.md`](docs/multimedia-gateway-design.md) v1.3 顶部「技术栈」块。**任何新引入的依赖必须先开 ADR 讨论**。

简版速查：
```
后端: Go 1.25/1.26 + Gin v1.10+ + sqlc + database/sql + PostgreSQL ≥ 15 + Asynq + Redis
前端: React 19 + Vite v6 + TypeScript + shadcn/ui + Tailwind v4 + pnpm
      + TanStack Router/Query/Table/Virtual + Zustand + react-hook-form + zod
      + lucide-react + Recharts + i18next (zh + en)
观测: log/slog (JSON) + prometheus/client_golang + OpenTelemetry Go SDK
认证: Session Cookie (UI) + Admin Token Bearer (API)
密钥: GATEWAY_KEK_V* envelope encryption (P0 环境变量, P1+ KMS)
工具: testify + dockertest + koanf + golang-migrate + GitHub Actions
布局: Monorepo (api-gateway/{main.go, cmd/admin-cli/, internal/, web/admin/, sql/queries/, migrations/, docs/})
      Go embed.FS 嵌入前端 dist 进单二进制
```

---

## 四、项目实际用到的设计原则

只列我们项目里**真用**的，不堆砌 SOLID 全套术语。

### 1. 开闭原则 OCP — 新增计费规则 / 路由规则不改代码
**实例：** `billingexpr` 表达式 DSL（v1 / v2 / vp 三版本）。新增模型只需在管理后台填表达式，**不改一行 Go 代码**。
**应用准则：** 如果某类配置预期会频繁变化（如计费、路由、provider 凭据），**用 DSL / 配置驱动**而非硬编码 switch。

### 2. 单一职责 SRP — 严格分层
**实例：** `handler → service → repository (sqlc queries)` 三层。handler 只做 HTTP 转换；service 只做业务逻辑；sqlc 只做数据访问。
**应用准则：** 一个 .go 文件不超过 ~400 行；一个函数不超过 ~50 行；一个 service 方法**只做一件事**。

### 3. 依赖倒置 DIP — 接口先行
**实例：** `LedgerService` 是接口，`PostgresLedgerService` 是实现；测试时用 `InMemoryLedgerService` mock。
**应用准则：** 跨子系统依赖**只依赖接口**，不依赖具体实现。Constructor injection（不要 global state，不要 init() 隐式连接）。

### 4. 接口隔离 ISP — Admin Token scope 拆分
**实例：** Admin Token 的 scope 分 `business_account:read`/`create`/`recharge`/`refund`/`token:write`/`webhook:manage` 等 10+ 细粒度，而不是一个 `admin:all`。
**应用准则：** **不给客户端比它实际需要更多的能力**。新增 API 时先想"谁会调？需要什么 scope？"

### 5. 失败优先（Fail-Closed by Default）
**实例：** `channel_routing_rule.fallback_policy` 默认 `strict`（候选不可用直接 503，不降级）。`isolation_required` 是硬开关。outbox 启动校验 fail-fast。
**应用准则：** 当不确定该 fail-open 还是 fail-closed 时，**选 fail-closed**。商业平台事故成本远高于"用户偶尔多看一次错误页"。

### 6. 显式优于隐式（v1.2.4 七轮评审的血泪教训）
**实例：** ledger 事务必须显式传 `tx *sql.Tx`（不能用 service 持有的 `db`）；状态机 CAS 必须显式列 from/to 状态（不能 `UPDATE WHERE id = ?` 不带 status 条件）。
**应用准则：** 涉及金钱、状态机、并发的代码，**禁止省略校验**。即使编译器允许、即使测试通过。

---

## 五、项目实际用到的设计模式

### 1. 工厂 / 抽象工厂 — Provider Adapter
**用法：** `relay/provider_factory.go` 按 `provider_type` 返回 `ProviderAdapter` 接口实现（火山 seedance / 阿里万相 / OpenAI / ...）。新增 provider = 加一个文件 + 注册到工厂。
**禁忌：** 不要在 handler 里 `switch provider_type {...}`，永远走工厂。

### 2. 策略模式 — fallback_policy 路由降级
**用法：** `RoutingFallbackStrategy` 接口 + 4 个实现（StrictStrategy / NextRuleStrategy / GlobalPoolStrategy / LegacyDistributorStrategy）。Distributor 持有当前 strategy 引用。
**配合：** 与 ISP（每个 strategy 只暴露 `Apply(ctx, candidates)` 方法）+ DIP（Distributor 依赖接口）配合。

### 3. 装饰器模式 — Gin middleware chain
**用法：** `TokenAuth → RateLimit → IsolationCheck → Distribute → SetupContext`。每个 middleware 装饰下一个 handler。
**注意：** middleware 不能有副作用穿透下一层（如设置全局变量）；必须用 `gin.Context` 传值。

### 4. 状态机模式 — Task 8 态 + CAS 转换
**用法：** `task.go` 定义 `TaskStatus` enum 和 `AllowedTransitions` 表；所有状态变更**只**通过 `CompareAndSwapTaskStatus(id, from, to)`。
**禁忌：** **永远不直接** `UPDATE tasks SET status = ?`，必须带 from 条件做 CAS。

### 5. Outbox 模式 — Webhook 事件可靠投递
**用法：** ledger 写入与 outbox 写入**同事务**；独立 worker 扫描 outbox 推送，失败重试，DLQ 兜底。
**关键：** outbox 表必须与 ledger 同库（详见 v1.3 文档 9bis.4.1）。

### 6. 模板方法 — Relay Handler 流程
**用法：** `RelayHandler` 抽象基类定义流程骨架：`Authorize → Reserve → CallUpstream → Settle → Refund-if-failed → PublishWebhook`。具体 provider 只重写 `CallUpstream` 一步。
**收益：** 新接 provider 只关心"怎么调上游"，不关心账本/路由/计费/通知。

---

## 六、PR / 代码评审硬规则

| 规则 | 通过门槛 |
|---|---|
| Commit message + PR 描述 | **中文** + 引用相关 ADR（如 "符合 ADR-0001 reimplement-only"） |
| 文件级 reimplement 自检 | PR 不含从 `third-party/new-api/` 复制的代码（搜索 commit diff，可疑片段必须人工证清白） |
| SQL 变更 | 必须附 `EXPLAIN ANALYZE` 输出在 PR 评论 |
| 涉及账本 / 状态机 / 凭据加密 | **必须**附并发 / 边界单元测试 |
| 涉及 Admin API | **必须**附 scope 检查 + 阀门验证 |
| 新增依赖 | 先开 ADR；通过后才能改 `go.mod` / `package.json` |
| schema 变更 | 必须有 `migrations/NNNN_*.up.sql` + `.down.sql`；不允许在线手改 DB |
| 删除生产数据的代码 | 必须有 `--dry-run` 模式；CI 必须跑 dry-run 测试 |
| 任何 panic / fatal 路径 | 必须列在 PR 描述里说明何时触发 |

---

## 七、参考文档导航

- 主设计文档：[docs/multimedia-gateway-design.md](docs/multimedia-gateway-design.md) (v1.3+)
- 决策记录：[docs/adr/](docs/adr/) 当前 0001-0005
- 术语表：[CONTEXT.md](CONTEXT.md)
- Schema 快照（当前生效版本，含所有 migrations 演化）：[docs/db/schema.md](docs/db/schema.md)
- 本地开发指南：[docs/dev-setup.md](docs/dev-setup.md)
- 计划归档：[docs/plans/](docs/plans/)
- Codex 评审历史：[docs/reviews/](docs/reviews/) 7 轮迭代归档
- 网关选型对比：[docs/模型网关选型建议.md](docs/模型网关选型建议.md)
- new-api 技术分析（只读参考）：[docs/new-api-technical-analysis.md](docs/new-api-technical-analysis.md)

---

## 八、本总纲的修订

本总纲是项目宪法级文档。修订规则：
- **小修订（typo / 链接更新 / 例子补充）**：直接 PR
- **新增原则 / 新增模式 / 修改约束**：必须先开 ADR 讨论，通过后修订本文 + 引用 ADR
- 修订时**保留版本号 + 日期**（顶部 v1.0 — 2026-05-26）

下一次修订可能的触发：第一个 provider 接完后，会有"provider adapter 实现模式"的实战经验沉淀。

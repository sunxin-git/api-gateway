---
title: "feat: 网关管理后台配置线（Admin API + 配置 DB 化 + 会话认证 + React 后台）"
type: feat
status: active
date: 2026-05-31
origin: docs/brainstorms/2026-05-31-gateway-admin-console-config-requirements.md
---

# feat: 网关管理后台配置线（Admin API + 配置 DB 化 + 会话认证 + React 后台）

## Overview

用**前端管理后台（React）+ DB-backed Admin API** 取代异步视频计划（`docs/plans/2026-05-28-001-feat-async-video-relay-mvp-plan.md`）的 **Unit 11（admin-cli 方案，作废）**，让运维在线完成网关全部功能配置：模型 catalog、结构化计费规则、渠道凭据（write-only）、账户×模型 entitlement、账户×模型并发上限覆写。配置改动即时对运行中的网关与 Asynq workers 生效，无需重启、无需碰数据库。

配套：把现有异步视频中继的配置来源从 **env 切到 DB**（fail-closed 兜底）；新增**用户名/密码会话认证**（初始管理员经 env 种子开通，再由其开通其余运维账户）；从零搭 React 管理后台并经 Go embed.FS 嵌入单二进制。

本计划是 **Deep / 全栈** 工作，分 6 个阶段交付；每个 Unit 可独立 ce-review + 提交。

## Problem Frame

网关的运维配置目前只能改 env + 重启或手刷 SQL；其中 channel 凭据与 entitlement 连配置面都没有。运维需要一个前端管理后台在线完成全部配置（见 origin）。消费者是内部运维（非公众用户）；业务系统仍走 Key 鉴权，不受影响。

## Requirements Trace

承接 origin 文档 R1–R17：

- R1/R2/R3/R3a. 用户名/密码**会话登录** + 初始管理员开通其余运维账户；单一运维角色（无精细 RBAC/2FA）；Admin Token Bearer 与会话并存（UI 走会话，自动化走 Bearer）。
- R4/R5/R6. 模型 catalog 由 env 单条改 **DB 多条**：gateway model→上游绑定+能力描述符；保存时 fail-fast 校验自洽；后台可查当前生效配置。
- R7/R8. 计费**结构化字段**（非 DSL）：(model, resolution, has_input_video)→{W×H, CNY/百万token, 倍率, 最低token下限, 计费单位}；调价只影响新提交，inflight 用快照价（settle≤reserve 不变量）。
- R9/R10. 渠道凭据后台**write-only**：明文加密入库、绝不回显/不入日志；列表/详情仅掩码；启停软下线。
- R11. entitlement grant/revoke/list（account×model）。
- R12. per-(account,model) 并发 cap 覆写 DB 持久化 + 默认兜底；可查 inflight。
- R13/R14. 配置改动**即时生效**无需重启；异步视频中继 env→DB 切换 + **fail-closed**（DB 无配置拒绝提交，不静默放行）。
- R15/R16. React 管理后台从零脚手架（CLAUDE.md §三栈）+ embed.FS；UI 文案 i18next zh+en + 表单双重校验 + 关键操作确认。
- R17. 配置写操作记审计（actor/对象/动作；**不记凭据明文/签名URL**）。

## Scope Boundaries

- **不做 admin-cli 功能配置命令**（Unit 11 CLI 作废；唯一例外 = 初始管理员的非交互 env 种子）。
- **不做 billingexpr DSL 求值器**（结构化字段够用；异形计费 SKU 出现再开 ADR）。
- **不做精细 RBAC / 2FA / 公开注册 / 密码自助找回**（推后）。
- **不做 KEK 凭据批量重加密命令**（ADR-0006 清单项，与本配置线正交，作为独立后续任务；本线只用当前最高版本 KEK 加密新写入凭据）。
- **不做输入媒体上传、人像库、多 provider 适配**；上游适配器仍只 seedance，本线只做「多模型/多档配置」的数据与 UI。
- 运行时配置「即时生效」= **秒级 TTL 窗口内**（见 Key Decisions），非毫秒级强一致；pg LISTEN/NOTIFY 即时失效推后。

## Context & Research

### Relevant Code and Patterns

- **Admin API 范式**：`internal/admin/{handler,business_account_handler,dto,errors}.go` —— Handler struct + constructor fail-fast；DTO `binding:"required"` + 自写 `validate()` 返中文；`errors.go` 的 `errorTable` + `MapError`（errors.Is 命中 sentinel→HTTP）。`main.go:registerAdminRoutes` 中间件链 `HSTS→AdminBodyLimit→AdminTokenAuth→AdminThrottle→scope→AdminAudit→handler`。权威范式见 `docs/plans/2026-05-27-003-feat-workflow-d-min-admin-api-plan.md` + `docs/api/admin-api.md`（scope 命名规约）。
- **env→DB 切换总装配点**：`main.go:buildVideoTaskService`（+`buildRelayHandler`）。`video.VideoCatalog` 是接口（`Lookup`/`DefaultEntry`/`All`），`*video.ConcurrencyLimits` + `video.{VideoModelEntry,Pricing,ResolutionTier,Capability}` 值类型**全部可复用**，只换配置来源。`task.Config{Catalog, Limits, Creds, Channels, ...}` 依赖接口 → **新 DB 实现满足接口则 task.Service 本体零改动**。`internal/config/config.go` 的 `GATEWAY_VIDEO_RELAY_*` 全部键 + `config.validate()` 跨字段校验是迁移对象。
- **settle 侧不动**：定价 DB 化只影响 submit 侧 `video.EstimateReserveMinor`；inflight 走 `internal/task/snapshot.go` 的冻结快照价（`SettleMinor`），账本不变量结构性安全。
- **channel（几乎现成）**：`internal/channel/{service,postgres}.go` 的 `channel.Service` 完整（Create/Update/List/GetByID/SetEnabled/Delete/GetCredentialsForUpstream + 掩码视图 + 解密 fail-closed），**仅缺 Admin handler/路由**。
- **entitlement（缺 service）**：仅 `sql/queries/entitlement.sql` + 生成层（`internal/db/entitlement.sql.go`）；`Grant/Revoke/List` 目前**无调用方**（只有 `Check` 被 `internal/task/read.go` 用）。需薄 service 或 handler 直调 `db.Queries`。
- **并发 cap**：`internal/relay/video/concurrency.go` 的 `NewConcurrencyLimits(default, overrides)`，`overrides` 当前写死 `nil`（注释「预留 Unit 11」）；DB 占位查询 `ClaimConcurrencySlot`（条件 UPSERT + cap=0 守卫，已 EXPLAIN，见 `docs/reviews/2026-05-30-explain-analyze-units-1-3.md`）。
- **密码哈希参照**：`internal/admintoken/postgres.go` 的 HMAC+pepper 适用**高熵 token**；运维密码低熵 → 必须 bcrypt/argon2（反向警示）。`golang.org/x/crypto`（含 `bcrypt`）**已在 go.mod 依赖树**（indirect），非新依赖。
- **审计/metrics**：`internal/audit/{logger,record}.go`（`AuditLogger.Emit` + Tier1 同步/Tier2 异步；Actor 为 string）；`internal/httpapi/middleware/admin_audit.go`（actor 取自 `GetAdminTokenValidation`，`determineTier` 规则）；`internal/obs/metrics.go`（自有 Registry + 末尾一次性 MustRegister）。
- **测试范式**：直连真 PG（非 dockertest），DSN = 包 env key→`LEDGER_TEST_PG_DSN`→默认；连不上 `t.Skipf`。`internal/admin/handler_test.go`（httptest+真PG+testPepper+testOutbox）、`internal/channel/postgres_test.go`（mustPool+mustKeyring+uniqueName+t.Cleanup）。`make test = go test ./... -race -count=1 -p=1`。
- **前端**：`web/admin/` 仅 `.gitkeep`；**零 go:embed**；Makefile 无前端 target；ADR-0004（P0 含 React UI + Session Cookie + CSRF + embed 单二进制 + 6 核心页）、ADR-0005（前端栈 + 初始化清单）**已预批**。

### Institutional Learnings

- `docs/solutions/` 目录**不存在**（无既有沉淀）；等价教训从 ADR/reviews/plans 提取（见下）。本线完成后可用 `/ce:compound` 首建该目录。
- `docs/reviews/codex-review-v1.md`（行 117–125）：凭据更新必须 **merge-preserving DTO**（否则编辑路径静默丢凭据）；密文带 `key_version`；**仅最小作用域解密，禁日志/panic/通用 JSON 序列化暴露明文**。
- `docs/reviews/codex-review-v1.1.md`（行 79–85）：KEK 轮换保留期须 ≥ max(任务最长执行期, DLQ 保留, 财务审计补偿窗口)；凭据轮换须对齐 inflight 生命周期。
- `docs/reviews/codex-review-v1.md`（Critical 23–37）：计费正确性靠「账本 + 原子预占 + 幂等流水 + 价格快照」；结构化定价表必须保留快照能力（R8）维持 settle≤reserve。

### External References

- 无（本线复用强本地范式；前端栈已 ADR-0004/0005 预批；会话认证为标准 cookie+bcrypt，不引外部新依赖）。

## Key Technical Decisions

- **配置面 = 前端管理后台 + DB-backed Admin API，取代 admin-cli**（origin 用户决策）。
- **计费 = 结构化 DB 定价表，DSL 推后**（origin 用户决策）；定价表保留**价格快照**入 `financial_snapshot`，调价只影响新提交。
- **运行时读 DB 配置策略（解 origin Deferred R13）**：
  - **并发 cap**：折进 `ClaimConcurrencySlot` 的 `COALESCE((SELECT cap FROM override ...), @default)` —— 永远最新、零额外往返、保原子占位语义。
  - **catalog/pricing/capability + safety/floor/ttl**：提交热路径用**短 TTL 内存缓存**包裹 DB 读（TTL 可配，默认约 15s），过期惰性刷新；「即时生效」= TTL 窗口内。LISTEN/NOTIFY 即时失效推后。
  - **entitlement check**：保持每请求 DB 查（已是现状，单次索引查询，无需缓存）。
- **fail-closed（R14）**：DB 无对应 model catalog / pricing 档 → 提交直接拒绝（明确 4xx），**不**回落 env、**不**静默放行。env 配置在切换后仅作**一次性种子导入**来源（或彻底弃用，见 Open Questions）。
- **会话认证 = 用户名/密码 + bcrypt + PG 会话表**（ADR-0008）：密码低熵故 bcrypt（非 HMAC）；会话存 PG（admin-only 部署可能无 Redis，PG 与 fail-closed 一致）；初始管理员经 env 种子幂等开通（表空时种子，不依赖 CLI）。
- **统一 AdminPrincipal 抽象**：auth 前置中间件解析「会话 Cookie→operator」或「Bearer→admin_token」为归一化 principal；下游 Throttle/Scope/Audit 读 principal。operator 拥有全部配置能力（单一角色）；admin_token 仍按 token scope 受限。审计 actor 扩 `operator:<id>`。
- **会话通道 CSRF**：会话 Cookie 的状态变更请求须带 CSRF token（双提交 cookie 或 header）；Bearer 通道无 ambient auth，豁免 CSRF。Cookie 设 `HttpOnly+Secure+SameSite`。
- **catalog/pricing/cap 全部 DB 化**——显式**推翻** `docs/plans/2026-05-28-001-*` 原 Scope Boundaries 的「catalog DB 化推后」（ADR-0007 记录）。
- **凭据写操作审计豁免明文**：`computeRequestHash` 含 body[:64KiB] → 凭据提交端点须**不 hash body 或确保 hash 不含明文**（沿用 codex-review-v1 铁律）。

## Open Questions

### Resolved During Planning

- 运行时读取策略：cap 折进 claim SQL；catalog/pricing 短 TTL 缓存；entitlement 每请求查（见 Key Decisions）。
- 密码哈希：bcrypt（x/crypto 已在树，非新依赖）。
- 会话存储：PG 表。
- 初始管理员：env 种子幂等（表空时），不引 CLI。
- AdminPrincipal：会话/Bearer 双通道归一化，operator 全配置能力。

### Deferred to Implementation

- env→DB 切换的灰度路径：**保留 env 作一次性种子导入** vs 直接以 DB 为唯一真相源（影响 Unit 13；倾向一次性种子 + 之后 DB 权威，落地核对运维流程）。
- catalog/pricing 短 TTL 缓存的失效粒度（全表失效 vs 按 model）与 TTL 默认值（落地按提交 QPS 与运维改配频率定）。
- bcrypt cost factor 取值（落地按目标登录延迟基准定）；是否叠加 pepper 预哈希（默认不叠，简单优先）。
- CSRF 具体形态（double-submit cookie vs 同步器 token）落地定。
- 多单位定价表（token/video_second）字段统一表达（对齐设计 §5.1 + storyboard-assistant 定价表；落地核对）。
- ClaimConcurrencySlot 加 COALESCE 覆写后的 EXPLAIN 复核（须附 PR）。

## High-Level Technical Design

> *以下示意意图与边界，供评审校验方向，不是实现规范。实现 agent 当作上下文，非照抄。*

**鉴权双通道归一化（Unit 4）：**

```
请求 → /admin/v1/*
  ├─ 带 Session Cookie  → 会话中间件: cookie→admin_session 查活跃会话→ AdminPrincipal{operator, id, caps=ALL_CONFIG} + CSRF 校验
  └─ 带 Bearer Token    → AdminTokenAuth(现有): → AdminPrincipal{admin_token, id, scopes=token.scopes}
                                  │
        下游 Throttle / Scope / Audit 统一读 AdminPrincipal（operator 放行全部配置 scope；token 按 scopes 校验；audit actor = operator:<id>|admin_token:<id>）
```

**env→DB 配置读取（Unit 13 装配切换；submit 热路径）：**

```
[submit 提交流程]
  能力校验/定价/绑定: DBVideoCatalog.Lookup(model)  ──(短TTL内存缓存,miss→查DB)──▶ gateway_model_catalog + model_resolution_pricing
        │  DB 无该 model/档 → fail-closed 拒绝(4xx)
        ▼
  reserve 估算: EstimateReserveMinor(req, pricing)   # pricing 来自上面缓存值;价格快照冻结进 financial_snapshot
        ▼
  并发占位: ClaimConcurrencySlot(account, model, @default)  # SQL 内 COALESCE(override.cap, @default);占不到→429
        ▼
  落 task + 入队（不变）
[settle] 读 financial_snapshot 冻结价（不读 catalog，零改动）
```

**新增数据表（详见各 Unit）：** `operator_account` / `admin_session`（Unit 2）、`gateway_model_catalog` / `model_resolution_pricing`（Unit 5）、`account_model_concurrency_override`（Unit 6）。已存在复用：`channel` / `business_account_model_entitlement` / `account_model_concurrency`（计数行）。

## Implementation Units

### Phase 0 — ADR 与契约冻结

- [x] **Unit 1: ADR-0007 + ADR-0008（决策与契约冻结，无代码）**

**Goal:** 落两份 ADR，冻结跨单元契约：配置 DB 化 + 在线生效（推翻 catalog 推后）；会话认证边界（用户名/密码 + bcrypt + PG 会话 + 初始管理员种子 + 明确不做 RBAC/2FA）；并冻结新 scope 命名、新表列集、AdminPrincipal 抽象、运行时读取策略。

**Requirements:** 全局（为后续单元定契约）

**Dependencies:** 无

**Files:**
- Create: `docs/adr/0007-ops-config-db-and-online-effect.md`
- Create: `docs/adr/0008-admin-console-session-auth.md`

**Approach:**
- ADR-0007：记 catalog/pricing/cap 的 DB 化与运行时读取/失效策略（cap 折 SQL、catalog 短 TTL、entitlement 每请求）；显式声明推翻 `2026-05-28-001` 的「catalog DB 化推后」；fail-closed 语义；env→DB 灰度方向。
- ADR-0008：用户名/密码会话登录 + bcrypt（记 x/crypto 已在树非新依赖）+ PG 会话表 + CSRF + 初始管理员 env 种子；明确**不做** RBAC/2FA/公开注册的边界与未来扩展位；AdminPrincipal 双通道归一化；会话 Cookie 与 Bearer 通道并存不混用（对齐 `docs/api/admin-api.md` D10）。
- 冻结：新 scope 命名（如 `channel:write`/`catalog:write`/`entitlement:grant`/`concurrency:write`/`operator:manage`，沿用 `admin-api.md` 风格）；新表列集；scope 仅约束 Bearer 通道，operator 默认全配置能力。

**Test scenarios:** Test expectation: none —— 纯决策文档。

**Verification:** 两份 ADR Accepted；后续单元引用其决策；CLAUDE.md §八（改约束先开 ADR）满足。

### Phase 1 — 会话认证后端

- [ ] **Unit 2: 认证 schema + sqlc（operator_account + admin_session）**

**Goal:** 建运维账户表与会话表 + sqlc 查询。

**Requirements:** R1/R2/R3

**Dependencies:** Unit 1

**Files:**
- Create: `migrations/0011_admin_console_auth.up.sql` / `.down.sql`
- Create: `sql/queries/operator_account.sql`（create/get-by-username/list/set-enabled/update-password）
- Create: `sql/queries/admin_session.sql`（insert/get-active-by-token-hash/delete/delete-expired）
- Modify: 生成的 `internal/db/*`（sqlc generate）
- Modify: `docs/db/schema.md`（**补登 0008–0010 滞后演化 + 0011 新表**）

**Approach:**
- `operator_account`：`id` / `username`（UNIQUE）/ `password_hash`（bcrypt）/ `enabled` / `created_by`（`operator:<id>` 或 `seed`）/ created_at / updated_at。
- `admin_session`：`session_token_hash`（HMAC，查会话热路径，不存明文）/ `operator_id`（FK CASCADE）/ `expires_at` / `created_at` / 可选 `last_seen_at`、`csrf_token`。
- 会话查询走 token_hash（与 admintoken 同思路）；过期清理查询供后台 sweep。
- migration up/down 配套；down 删两表。

**Patterns to follow:** `migrations/0005_*`（up/down 风格）；`sql/queries/business_account_api_key.sql`（sqlc 写法）。

**Test scenarios:** Test expectation: none —— schema + 查询生成；行为测试在 Unit 3/4 覆盖。

**Verification:** `make sqlc` 通过 + diff guard 干净；`make migrate-up/down/up` 往返成功；schema.md 同步（含补登 0008–0011）。

- [ ] **Unit 3: operator service（口令哈希 + 认证 + 初始管理员种子）**

**Goal:** 实现运维账户域服务：bcrypt 哈希、认证、create/disable/list、初始管理员 env 种子幂等。

**Requirements:** R1/R2/R3

**Dependencies:** Unit 2

**Files:**
- Create: `internal/operator/types.go`（OperatorAccount 视图，**不含 password_hash**）
- Create: `internal/operator/service.go`（接口 + sentinel：ErrNotFound/ErrInvalidParam/ErrAuthFailed/ErrUsernameExists）
- Create: `internal/operator/postgres.go`（bcrypt 哈希 + CRUD + Authenticate）
- Create: `internal/operator/bootstrap.go`（表空时从 env 种子初始管理员，幂等）
- Modify: `internal/config/config.go`（`GATEWAY_ADMIN_BOOTSTRAP_USERNAME`/`_PASSWORD` 读取 + production 校验）
- Test: `internal/operator/postgres_test.go`、`internal/operator/bootstrap_test.go`

**Execution note:** 涉口令/认证，测试先行覆盖哈希往返 + 认证边界（CLAUDE.md：涉凭据必须边界单测）。

**Approach:**
- bcrypt（`golang.org/x/crypto/bcrypt`，cost 落地定）；`Authenticate(username, password)` 常量时间路径，禁用账户拒登；**绝不**回显/记录 password_hash 或明文。
- 种子：启动时若 `operator_account` 空且 env 提供 bootstrap 账户 → 建初始管理员（`created_by="seed"`）；幂等（表非空跳过）；production 下表空且无 bootstrap env → fail-fast 或明确告警（落地定）。
- service 仅返回不含哈希的视图。

**Patterns to follow:** `internal/businesskey/{service,postgres,types,errors}.go` 同构分层；`internal/admintoken/postgres.go`（pepper/hash 装配风格，但密码用 bcrypt）。

**Test scenarios:**
- Happy path：建账户→Authenticate 正确口令通过；list 不含哈希。
- Edge case：username 唯一冲突→ErrUsernameExists；空/超长 username/弱口令长度边界。
- Error path：错口令/禁用账户→ErrAuthFailed（不区分「不存在 vs 错口令」避免枚举）；不存在→ErrNotFound。
- 安全断言：任何返回/日志/错误不含 password_hash 或明文。
- Integration：种子幂等——表空+env→建初始管理员；再启动表非空→不重复建。

**Verification:** 哈希往返 + 认证边界 + 种子幂等测试通过（真 PG）；明文/哈希不出现在任何输出。

- [ ] **Unit 4: 会话中间件 + login/logout + CSRF + AdminPrincipal 归一化**

**Goal:** 实现会话登录端点 + cookie→session 鉴权中间件（与 Bearer 并存）+ CSRF，并把 Throttle/Scope/Audit 改读统一 AdminPrincipal。

**Requirements:** R1/R3/R3a/R17

**Dependencies:** Unit 3

**Files:**
- Create: `internal/httpapi/middleware/admin_principal.go`（AdminPrincipal 类型 + 从 ctx 取/注入）
- Create: `internal/httpapi/middleware/admin_session_auth.go`（cookie→admin_session 查活跃→注入 operator principal + CSRF 校验）
- Create: `internal/httpapi/session_handler.go`（`POST /admin/login`、`POST /admin/logout`；登录成功 SetCookie + 下发 CSRF token）
- Modify: `internal/httpapi/middleware/admin_token_auth.go`（Bearer 成功也注入归一化 AdminPrincipal）
- Modify: `internal/httpapi/middleware/admin_scope.go`（operator principal 放行全部配置 scope；token 按 scopes 校验）
- Modify: `internal/httpapi/middleware/admin_audit.go`（actor 取自 AdminPrincipal，支持 `operator:<id>`；`determineTier` 后续 Unit 13 扩展）
- Modify: `main.go`（注册 `/admin/login|logout`；`/admin/v1` 链改为「会话 OR Bearer」二选一前置）
- Test: `internal/httpapi/session_handler_test.go`、`internal/httpapi/middleware/admin_session_auth_test.go`

**Execution note:** 公网/认证入口，测试先行覆盖未登录/过期会话/CSRF 缺失/伪造 cookie。

**Approach:**
- 登录：校验用户名/口令→建会话（token_hash 入库，明文 token 写 `HttpOnly+Secure+SameSite` cookie）+ 下发 CSRF token（前端后续请求带 header）。
- 鉴权前置：同一 `/admin/v1` 端点接受会话 cookie **或** Bearer；二者择一成功即注入 AdminPrincipal，否则 401。会话路径校验 CSRF（状态变更方法）。
- Scope：operator → 全配置能力；admin_token → 现有 scope 校验不变（向后兼容既有 5 端点）。
- 登出：删会话行 + 清 cookie。

**Patterns to follow:** `internal/httpapi/middleware/admin_token_auth.go`（注入 ctx 验证结果模式）；`docs/api/admin-api.md` D10（通道并存不混用）。

**Test scenarios:**
- Happy path：login 正确→Set-Cookie + CSRF；带 cookie+CSRF 调 `/admin/v1` 通过（operator principal）；logout 后 cookie 失效。
- Edge case：会话过期→401；并存——同端点 Bearer 仍可调（既有 5 端点回归不破）。
- Error path：错口令→401；缺 CSRF 的状态变更→403；伪造/篡改 cookie→401；未登录→401。
- 安全断言：cookie `HttpOnly+Secure+SameSite`；会话明文 token 不入库/不入日志。
- Integration：operator 经会话调既有 business_account 端点与新配置端点均放行；过期会话被 sweep 后再调→401。

**Verification:** 双通道并存生效；既有 Bearer 端点回归通过；CSRF/过期/伪造路径被拒。

### Phase 2 — 配置 DB 化后端（schema + service + DB 实现）

- [ ] **Unit 5: catalog/pricing schema + sqlc**

**Goal:** 建 gateway 模型 catalog 表 + 分辨率档定价表 + sqlc。

**Requirements:** R4/R5/R7

**Dependencies:** Unit 1

**Files:**
- Create: `migrations/0012_video_catalog_pricing.up.sql` / `.down.sql`
- Create: `sql/queries/video_catalog.sql`（model upsert/get-by-name/list/set-enabled/delete）
- Create: `sql/queries/model_pricing.sql`（按 model 列定价档 / upsert 档 / 删档）
- Modify: 生成的 `internal/db/*`
- Modify: `docs/db/schema.md`（0012 新表）

**Approach:**
- `gateway_model_catalog`：`gateway_model`（UNIQUE）/ `upstream_provider_type` / `upstream_model` / `channel_name`（绑定，FK 或软引用）/ `enabled` / 能力档（duration/fps/ratio/resolution 取值，jsonb 或列）/ `safety_factor_bp` / `min_token_floor` / `result_url_ttl_seconds` / timestamps。
- `model_resolution_pricing`：`gateway_model`（FK CASCADE）/ `resolution` / `has_input_video` / `width` / `height` / `price_per_1m_minor` / `billing_multiplier_bp` / `unit`（token/video_second）/ 复合唯一 (model, resolution, has_input_video)。
- 多单位：`unit` 字段表达（对齐设计 §5.1）。
- up/down 配套。

**Patterns to follow:** `migrations/0005_*`；`internal/relay/video/catalog.go` 的 `VideoModelEntry`/`Pricing`/`ResolutionTier` 字段结构（DB 列对齐值类型，便于 Unit 7 复用）。

**Test scenarios:** Test expectation: none —— schema + 查询生成；行为测试在 Unit 7。

**Verification:** `make sqlc` + diff guard；migrate 往返；schema.md 同步。

- [ ] **Unit 6: 并发 cap 覆写 schema + sqlc（claim 查询 COALESCE）**

**Goal:** 建 per-(account,model) cap 覆写表 + 改 ClaimConcurrencySlot 用 `COALESCE(覆写, 默认)`，保原子占位语义。

**Requirements:** R12

**Dependencies:** Unit 1

**Files:**
- Create: `migrations/0013_concurrency_cap_override.up.sql` / `.down.sql`
- Create: `sql/queries/concurrency_override.sql`（set/get/delete/list cap 覆写）
- Modify: `sql/queries/task.sql`（`ClaimConcurrencySlot` 改 `COALESCE((SELECT cap FROM account_model_concurrency_override WHERE ...), @default_cap)`，保留 cap=0 守卫与单条原子 UPSERT）
- Modify: 生成的 `internal/db/*`
- Modify: `docs/db/schema.md`（0013 新表 + claim 语义说明）
- Test: 并发占位回归在 `internal/task/*_test.go`（既有并发测试扩 cap 覆写场景）

**Execution note:** 涉并发原子占位，测试先行覆盖「覆写生效 + cap=0 禁用 + TOCTOU 不超卖」。

**Approach:**
- `account_model_concurrency_override`：`business_account_id` / `model` / `cap`（int, CHECK ≥0）/ timestamps；复合主键 (account, model)。
- claim SQL：默认仍传 `@default_cap`（Go 侧 ConcurrencyLimits 默认），DB 内 `COALESCE` 命中覆写则用覆写值；**保留** `WHERE $cap >= 1` 守卫（cap=0 禁用）与单条 UPSERT 原子性。
- 覆写表与计数行表（`account_model_concurrency`）分离：覆写可先于任何 inflight 存在。

**Patterns to follow:** ADR-0006 决策 2 的 claim UPSERT；`docs/reviews/2026-05-30-explain-analyze-units-1-3.md`（EXPLAIN 基线，改后须复核附 PR）。

**Test scenarios:**
- Happy path：设覆写 cap=N→该 (account,model) 占 N 个后第 N+1 个 429；无覆写→走默认 cap。
- Edge case：覆写 cap=0→该 (account,model) 提交一律 429（禁用）；删覆写→回落默认。
- Error path / 并发：N 并发恰好卡覆写上限，仅 cap 个成功 claim 其余 429（TOCTOU 不超卖）。
- Verification：改后 `ClaimConcurrencySlot` 的 EXPLAIN ANALYZE 附 PR（CLAUDE.md §六 SQL 变更要求）。

**Verification:** 真 PG 下覆写/默认/禁用/并发 TOCTOU 全绿；EXPLAIN 附 PR；不破坏既有占位回归。

- [ ] **Unit 7: catalog/pricing service + DBVideoCatalog（实现 video.VideoCatalog）**

**Goal:** 实现 DB 配置服务（模型/定价 CRUD + 保存时 fail-fast 校验）+ `DBVideoCatalog` 实现 `video.VideoCatalog` 接口（多模型，短 TTL 缓存），复用现有值类型。

**Requirements:** R4/R5/R6/R7

**Dependencies:** Unit 5

**Files:**
- Create: `internal/videocfg/service.go`（模型/定价 CRUD + 保存校验；sentinel error）
- Create: `internal/videocfg/postgres.go`（读写 catalog/pricing 表 → 组装 `video.VideoModelEntry`）
- Create: `internal/relay/video/db_catalog.go`（`DBVideoCatalog` 实现 `VideoCatalog`：`Lookup`/`DefaultEntry`/`All` 从 service + 短 TTL 缓存）
- Test: `internal/videocfg/postgres_test.go`、`internal/relay/video/db_catalog_test.go`

**Approach:**
- 保存校验迁移自 EnvVideoCatalog 的 fail-fast（`buildPricing`/`buildCapability`/identity 校验）→ 改为「保存配置时校验自洽」（每分辨率档须有 W×H + 定价，否则拒绝保存）。
- `DBVideoCatalog`：复用 `video.{VideoModelEntry,Pricing,ResolutionTier,Capability}` 值类型；短 TTL 内存缓存（TTL 可配），miss/过期查 DB；**DB 无该 model/档 → 返回 not-found 让调用方 fail-closed**。
- 并发安全只读（缓存读写加锁或原子替换快照）。

**Patterns to follow:** `internal/relay/video/catalog.go` + `catalog_build.go`（EnvVideoCatalog 校验逻辑搬迁）；`internal/channel/postgres.go`（service+postgres 分层）。

**Test scenarios:**
- Happy path：建模型+定价档→`Lookup(model)` 返正确 entry（含各档 W×H/价/能力）；`All` 列多模型。
- Edge case：缓存 TTL 内改 DB→旧值；过期后→新值；多模型并发 Lookup 安全。
- Error path：保存缺某档定价/W×H→拒绝保存（fail-fast）；`Lookup` 未知 model→not-found（调用方 fail-closed）。
- Integration：service 写入后 DBVideoCatalog（绕缓存或等 TTL）读到一致配置。

**Verification:** 保存校验矩阵 + 多模型 Lookup + 缓存语义 + 未知 model fail-closed 测试通过。

- [ ] **Unit 8: 并发 cap DB 解析 + 计费参数 DB 化**

**Goal:** 让并发 cap 默认/覆写 + safety/floor/ttl 等计费参数来自 DB（或经 catalog），供装配切换用。

**Requirements:** R12/R7/R13

**Dependencies:** Unit 6/7

**Files:**
- Modify: `internal/relay/video/concurrency.go`（`Cap()` 默认值来源；覆写由 claim SQL 承载，本层只持默认 + 可选 DB 默认）
- Modify: `internal/videocfg/*`（暴露 model 级 safety_factor_bp/min_token_floor/result_url_ttl 给 handler/reserve）
- Test: `internal/relay/video/concurrency_test.go`（默认来源回归）、`internal/videocfg/*_test.go`

**Approach:**
- cap 覆写已在 claim SQL（Unit 6）；本单元确保 Go 侧默认 cap 与 safety/floor/ttl 从 catalog/config 读，供 Unit 13 装配注入 `httpapi.NewVideoHandler` 与 reserve。
- safety/floor/ttl 作 catalog 模型级字段（Unit 5 已建列）→ DBVideoCatalog entry 暴露。

**Patterns to follow:** 现有 `concurrency.go`；`internal/httpapi/video_handler.go`（safetyFactorBP/minTokenFloor/ttl 入参）。

**Test scenarios:**
- Happy path：模型级 safety/floor/ttl 从 DB 取，reserve 估算用之。
- Edge case：缺省回落默认；多模型各自档参数独立。
- Error path：缺必填参数→保存期拒绝（Unit 7 校验兜底）。

**Verification:** 计费参数 DB 来源测试通过；不破坏 Unit 7 reserve 口径。

### Phase 3 — Admin API 端点

- [ ] **Unit 9: channel Admin API（write-only 凭据 CRUD）**

**Goal:** 暴露渠道凭据后台端点（create/list/get 掩码/轮换 merge-preserving/启停/删），复用 `channel.Service`。

**Requirements:** R9/R10/R17

**Dependencies:** Unit 4（鉴权）

**Files:**
- Create: `internal/admin/channel_handler.go`（CRUD handler）
- Create: `internal/admin/channel_dto.go`（请求 DTO + merge-preserving 凭据更新）
- Modify: `internal/admin/handler.go`（注入 `channel.Service`）、`internal/admin/errors.go`（追加 channel sentinel 映射）
- Modify: `main.go:registerAdminRoutes`（注册 channel 端点 + `channel:write`/`channel:read` scope + 审计）
- Test: `internal/admin/channel_handler_test.go`（httptest + 真 PG）

**Execution note:** 涉凭据，测试先行覆盖明文不回显 + 轮换不丢字段。

**Approach:**
- create：接 5 段凭据明文→`channel.Service.Create`（加密入库）；返回掩码视图。
- update/轮换：**merge-preserving DTO**——未提交的凭据字段保留原值，不静默清空（codex-review-v1 铁律）；显式置空走专门标记。
- list/get：仅掩码；启停/删走 SetEnabled/Delete。
- 凭据端点 body 不进 audit request hash 明文（见 Unit 13）。

**Patterns to follow:** `internal/admin/{handler,dto,errors}.go` 既有 5 端点范式；`channel.Service` 安全契约。

**Test scenarios:**
- Happy path：create→入库密文，list/get 返掩码不含明文；轮换部分字段→未提交字段保留。
- Edge case：缺凭据字段友好中文错误；重复 channel name UNIQUE 冲突映射；启停/删幂等。
- Error path：未授权 scope→403；无效 body→400；删不存在→404/明确码。
- 安全断言：任何响应/日志/审计不含凭据明文；DB 内为密文。

**Verification:** channel CRUD 端到端（真 PG）通过；明文 write-only；merge-preserving 轮换不丢字段；scope 校验生效。

- [ ] **Unit 10: catalog/pricing Admin API（模型 + 定价）**

**Goal:** 暴露模型 catalog 与定价后台端点（增改启停模型 + 定价档 CRUD），经 Unit 7 service。

**Requirements:** R4/R5/R6/R7/R17

**Dependencies:** Unit 4、Unit 7

**Files:**
- Create: `internal/admin/catalog_handler.go`、`internal/admin/catalog_dto.go`
- Modify: `internal/admin/handler.go`（注入 `videocfg` service）、`errors.go`（追加 sentinel）
- Modify: `main.go:registerAdminRoutes`（`catalog:write`/`catalog:read` scope + 审计）
- Test: `internal/admin/catalog_handler_test.go`

**Approach:**
- 模型增改启停 + 定价档 CRUD；保存走 Unit 7 fail-fast 校验（缺档/缺 W×H 拒绝）。
- get 返当前生效配置（含掩码后的绑定 channel 标识）。

**Patterns to follow:** Unit 9 channel handler 同构；DTO `validate()` 风格。

**Test scenarios:**
- Happy path：建模型+各档定价→get 返一致；改价→新提交生效（配合 Unit 13）。
- Edge case：定价档边界（分辨率枚举、价格非负、倍率 bp 范围）；删模型连带定价。
- Error path：保存不自洽（缺档/W×H）→400 明确；未授权 scope→403。

**Verification:** 模型/定价端到端通过；保存校验拒绝不自洽配置；scope 生效。

- [ ] **Unit 11: entitlement + 并发 cap 覆写 Admin API**

**Goal:** 暴露 entitlement（grant/revoke/list）与并发 cap 覆写（set/get/delete/list）后台端点。

**Requirements:** R11/R12/R17

**Dependencies:** Unit 4、Unit 6

**Files:**
- Create: `internal/admin/entitlement_handler.go`、`internal/admin/concurrency_handler.go`、对应 dto
- Create: `internal/entitlement/service.go`（薄包装 sqlc Grant/Revoke/List；或 handler 直调 db.Queries —— 落地择一）
- Modify: `internal/admin/handler.go`、`errors.go`
- Modify: `main.go:registerAdminRoutes`（`entitlement:grant`/`concurrency:write` 等 scope + 审计）
- Test: `internal/admin/entitlement_handler_test.go`、`internal/admin/concurrency_handler_test.go`

**Approach:**
- entitlement：grant 幂等（既有 upsert sql）；revoke 返是否原本存在；list by account。
- cap 覆写：set/get/delete/list（Unit 6 表）；list 可联 `account_model_concurrency` 显示当前 inflight。
- FK 不存在账户→友好中文错误。

**Patterns to follow:** `sql/queries/entitlement.sql`；`internal/channel` 分层（若建 entitlement service）。

**Test scenarios:**
- Happy path：grant→check 通过；list 返已授权 model；set cap→提交侧 429 阈值随之变（配合 Unit 6/13）。
- Edge case：重复 grant 幂等；revoke 不存在→明确码；cap=0 覆写禁用。
- Error path：FK 不存在账户 grant→友好错误；未授权 scope→403。

**Verification:** entitlement/cap 端到端通过；幂等/边界/scope 生效。

- [ ] **Unit 12: operator 账户 Admin API（开通/停用/列运维账户）**

**Goal:** 暴露运维账户管理端点（建/停用/列），供初始管理员开通其余运维账户。

**Requirements:** R2/R3/R17

**Dependencies:** Unit 3、Unit 4

**Files:**
- Create: `internal/admin/operator_handler.go`、`internal/admin/operator_dto.go`
- Modify: `internal/admin/handler.go`（注入 `operator.Service`）、`errors.go`
- Modify: `main.go:registerAdminRoutes`（`operator:manage` scope + 审计；建账户→Tier1）
- Test: `internal/admin/operator_handler_test.go`

**Approach:**
- 建账户（用户名 + 初始口令）→ operator service（bcrypt 入库）；停用/列；口令明文仅创建入参，**不回显**。
- 单一运维角色：所有 operator 可访问配置端点；账户管理端点需 `operator:manage`（初始管理员具备）。

**Patterns to follow:** Unit 9/10 handler 同构；`internal/operator` service。

**Test scenarios:**
- Happy path：建运维账户→该账户可登录（配合 Unit 4）；list 不含哈希。
- Edge case：用户名冲突→明确错误；停用账户后该账户登录被拒。
- Error path：无 `operator:manage`→403；弱口令长度边界拒。
- 安全断言：响应/日志不含 password_hash/明文。

**Verification:** 运维账户管理端到端通过；停用即时阻断登录；scope 生效。

### Phase 4 — 集成切换（env→DB）

- [ ] **Unit 13: 装配切换 + fail-closed + 审计扩展**

**Goal:** `main.go` 装配从 env 切到 DB 配置（DBVideoCatalog + cap 覆写 + 计费参数），fail-closed；env 作一次性种子导入；审计 `determineTier` 扩配置写规则 + 凭据端点 hash 豁免。

**Requirements:** R13/R14/R17

**Dependencies:** Unit 7/8/9/10/11/12

**Files:**
- Modify: `main.go`（`buildVideoTaskService`/`buildRelayHandler` 注入 DBVideoCatalog + DB cap 默认 + 短 TTL；移除/弱化 EnvVideoCatalog 装配为种子）
- Modify: `internal/config/config.go`（`config.validate()` 中 video relay 跨字段校验搬到保存路径；env 改为种子/可选）
- Modify: `internal/httpapi/middleware/admin_audit.go`（`determineTier` 加配置写高价值规则：建账户/删渠道/改价→Tier1；凭据端点 request hash 豁免明文）
- Create: `internal/videocfg/seed.go`（可选：首次从 env 一次性导入 catalog/pricing/cap，幂等）
- Test: `internal/httpapi/video_handler_test.go`（fail-closed：DB 无 model→拒）、`main` 装配冒烟、`admin_audit` Tier 测试

**Execution note:** 切换触及提交热路径 + 资金路径，测试先行覆盖 fail-closed 与既有视频流回归。

**Approach:**
- 装配点注入 DB 实现（接口不变 → task.Service 零改动）；DB 无配置 → 提交 fail-closed 拒绝（不回落 env）。
- env→DB 灰度：保留 env 作**一次性种子导入**（幂等 seed），之后 DB 权威（最终是否彻底弃用 env 见 Open Questions）。
- 审计：配置写操作记 Tier1/2（建账户/删渠道/改价 → Tier1）；凭据 body 不入 request hash 明文。

**Patterns to follow:** `main.go:buildVideoTaskService` 现有装配；`internal/httpapi/middleware/admin_audit.go:determineTier`。

**Test scenarios:**
- Happy path：DB 配好 model→提交按 DB catalog/pricing/cap 鉴权/校验/计费/限并发；改价后**新提交**用新价、inflight 用快照价。
- Edge case：TTL 窗口内改 catalog→旧值；过期→新值。
- Error path：DB 无该 model/档→提交 fail-closed 4xx（不回落 env、不静默放行）。
- Integration：env 种子导入一次→DB 有配置→视频流端到端回归（提交→submit→settle）不破；审计 Tier 正确、凭据明文不入 hash。

**Verification:** env→DB 切换后视频流全链回归通过；fail-closed 生效；审计 Tier/明文豁免正确。

### Phase 5 — React 管理后台

- [ ] **Unit 14: web/admin 脚手架 + embed.FS + 静态路由**

**Goal:** 从零搭 React 管理后台脚手架（CLAUDE.md §三栈 / ADR-0005 清单）+ go:embed 嵌入 + SPA 静态路由 + Makefile/dev-setup。

**Requirements:** R15

**Dependencies:** Unit 4（鉴权契约）

**Files:**
- Create: `web/admin/`（pnpm + Vite v6 + TS + shadcn/ui + Tailwind v4 + TanStack Router/Query + i18next zh+en 脚手架）
- Create: `internal/webui/embed.go`（`//go:embed web/admin/dist` + `http.FS` SPA fallback handler）
- Modify: `main.go`（挂静态路由到 httpapi.Server engine，SPA fallback index.html）
- Modify: `Makefile`（`pnpm build` 前置 target，产物到 `web/admin/dist`）
- Modify: `docs/dev-setup.md`（前端开发/构建/嵌入流程）

**Approach:**
- 照 ADR-0005 §实施清单初始化；同源调用 `/admin/v1` 无 CORS（ADR-0004）；embed 单二进制。
- SPA fallback：未命中静态资源回 index.html。

**Test scenarios:** Test expectation: none —— 脚手架/构建集成；行为在 Unit 15 页面 + 后端测试覆盖。冒烟：`go build` 含 embed 成功、静态根路由返回 index.html。

**Verification:** `pnpm build` 产物 embed 进单二进制；启动后访问根路径返回 SPA；dev-setup 文档可复现。

- [ ] **Unit 15: 管理后台页面（登录 + 5 域配置）**

**Goal:** 实现登录页 + 5 域配置页（渠道/模型+定价/entitlement/并发/运维账户），i18next zh+en，表单双重校验，关键操作确认。

**Requirements:** R15/R16

**Dependencies:** Unit 9/10/11/12/14

**Files:**
- Create: `web/admin/src/`（登录页 + 渠道/模型定价/entitlement/并发/运维账户 页面 + API client + i18n zh/en 资源）
- Test: `web/admin/`（组件/表单校验测试，按前端栈测试约定）

**Approach:**
- 登录走 `/admin/login`（会话 cookie + CSRF）；各配置页调对应 `/admin/v1` 端点。
- 凭据页 write-only（不展示明文，掩码显示）；改价/删渠道/撤 entitlement 二次确认。
- 文案 i18next zh+en（源串英文键名，CLAUDE.md §一）。

**Patterns to follow:** ADR-0004 的 6 核心页信息架构；shadcn/ui + react-hook-form + zod 校验。

**Test scenarios:**
- Happy path：登录→渠道/模型/entitlement/并发/账户 各页 CRUD 流程可用。
- Edge case：表单校验（必填/范围/枚举）客户端拦截 + 服务端错误回显。
- Error path：401 跳登录；403 scope 提示；网络/校验错误友好展示。
- 安全断言：凭据明文不在前端展示/不落 console；CSRF token 随状态变更请求带上。

**Verification:** 5 域配置经 UI 端到端可用；zh+en 切换正常；write-only/确认/校验生效。

### Phase 6 — 可观测、审计、文档收尾

- [ ] **Unit 16: metrics + 审计字段 + 文档收尾**

**Goal:** 补配置写操作 metrics + 审计字段齐全无敏感 + 文档（schema.md 补全、API 文档、dev-setup、更新作废的旧计划 Unit 11 指向本计划）。

**Requirements:** R17 + 成功标准

**Dependencies:** Unit 9–15

**Files:**
- Modify: `internal/obs/metrics.go`（config_write_total 等：按域/动作/结果计数）
- Modify/Verify: `internal/httpapi/middleware/admin_audit.go`（配置写审计字段齐全、无凭据明文/签名URL）
- Create: `docs/api/admin-config-api.md`（配置面 Admin API 契约 + scope + 错误码）
- Modify: `docs/db/schema.md`（确认 0008–0013 全登记）
- Modify: `docs/plans/2026-05-28-001-feat-async-video-relay-mvp-plan.md`（Unit 11 标注「作废 → 由本计划取代」并链接）
- Modify: `docs/dev-setup.md`（运维配置后台操作流程）
- Test: `internal/obs/metrics_test.go`（新指标累加）

**Approach:**
- metrics 注册照 `NewMetrics` 一次性 MustRegister 风格。
- 审计标签不含 prompt/凭据/签名URL（对齐既有红线）。
- 旧计划 Unit 11 不删，加显式作废注记 + 指向本计划（可追溯）。

**Test scenarios:**
- Happy path：各配置写操作后对应指标累加；审计记录字段齐全。
- 安全断言：metrics 标签/审计字段不含凭据明文/签名URL/口令。

**Verification:** 指标/审计字段测试通过；文档自洽链接有效；旧计划 Unit 11 有作废指向。

## System-Wide Impact

- **Interaction graph:** 新增 `/admin/login|logout` + `/admin/v1/{channels,catalog,pricing,entitlements,concurrency,operators}` 端点；`/admin/v1` 鉴权链改为「会话 OR Bearer」；提交热路径 catalog/pricing/cap 读 DB（缓存/COALESCE）；前端 SPA 静态路由。复用 business 中间件链 + 账本 + audit/metrics。
- **Error propagation:** 配置保存不自洽→fail-fast 400；DB 无配置→提交 fail-closed 4xx；凭据解密失败→fail-closed；会话过期/CSRF 缺失→401/403。
- **State lifecycle risks:** 凭据轮换 merge-preserving（防静默丢字段）；会话过期清理 sweep；catalog TTL 缓存与「即时生效」语义；env→DB 种子幂等（防重复导入）。
- **API surface parity:** 既有 5 个 business_account 端点（Bearer）回归不破；会话与 Bearer 双通道同端点等价；OpenAI/admin 错误形状一致。
- **Integration coverage:** 登录→配置→提交端到端（真 PG + 会话 + DB catalog/pricing/cap）；改价只影响新提交、inflight 用快照价；并发 cap 覆写跨提交生效。
- **Unchanged invariants:** 账本不变量 `available+reserved+used_total=recharge_total` 不变；**settle 侧零改动**（读快照）；业务 Key 鉴权/pepper 不变；同步 relay 不动；既有 Bearer Admin API 兼容。

## Risks & Dependencies

| Risk | Mitigation |
|------|------------|
| env→DB 切换破坏运行中视频流 | 接口不变（task.Service 零改动）；fail-closed 显式拒绝而非静默；切换后视频流全链回归测试；env 一次性种子保平滑 |
| 凭据编辑静默丢字段 | merge-preserving DTO（codex-review-v1 铁律）；轮换测试断言未提交字段保留 |
| 会话认证引入鉴权回归 | 双通道并存，既有 Bearer 端点回归测试；会话路径独立中间件，不改 token 校验逻辑 |
| 凭据明文经审计/日志泄露 | 凭据端点 request hash 豁免明文；掩码 write-only；最小作用域解密；安全断言测试 |
| 并发 cap 改 claim SQL 破坏原子性/超卖 | 保留单条 UPSERT + cap=0 守卫；COALESCE 不引幻读；并发 TOCTOU 测试 + EXPLAIN 复核附 PR |
| catalog TTL 缓存导致改配未即时生效 | 文档明确「即时=TTL 窗口内」；TTL 可配小值；cap/entitlement 不走缓存（即时） |
| 口令低熵被暴力 | bcrypt（非 HMAC）；禁用账户拒登；不区分「不存在 vs 错口令」防枚举；登录限速（复用 throttle） |
| 前端脚手架 + embed 全新 | ADR-0004/0005 已预批清单照做；Unit 14 单列脚手架 + 冒烟；前端栈无新 ADR |
| 初始管理员种子鸡生蛋 | env 种子幂等（表空时），不引 CLag；production 表空且无 bootstrap env → fail-fast 告警 |
| schema.md 文档滞后(到 0007) | Unit 2 补登 0008–0011，各 schema 单元同步补当期 |
| 推翻原计划 Scope Boundaries | ADR-0007 显式记录推翻 catalog DB 化推后；旧计划 Unit 11 加作废指向 |

## Documentation / Operational Notes

- 部署需：production 首启提供 `GATEWAY_ADMIN_BOOTSTRAP_*` 种子初始管理员；前端 `pnpm build` 产物须在 `go build` 前生成（Makefile 串联）。
- ADR-0007/0008 须先 Accepted（Unit 1）再动 schema/认证代码。
- env→DB 切换上线建议：先种子导入校验 DB 配置等价 env，再切 fail-closed；保留回滚（暂留 env 种子）。
- 旧计划 `docs/plans/2026-05-28-001-*` 的 Unit 11 作废但保留（可追溯），由本计划取代。

## Sources & References

- **Origin document:** [docs/brainstorms/2026-05-31-gateway-admin-console-config-requirements.md](../brainstorms/2026-05-31-gateway-admin-console-config-requirements.md)
- 取代：[docs/plans/2026-05-28-001-feat-async-video-relay-mvp-plan.md](2026-05-28-001-feat-async-video-relay-mvp-plan.md) Unit 11
- Admin API 范式：[docs/plans/2026-05-27-003-feat-workflow-d-min-admin-api-plan.md](2026-05-27-003-feat-workflow-d-min-admin-api-plan.md)、`docs/api/admin-api.md`
- 前端：`docs/adr/0004-p0-includes-full-react-ui.md`、`docs/adr/0005-frontend-stack-industry-mainstream.md`
- 复用骨架：`internal/admin`、`internal/channel`、`internal/crypto`、`internal/admintoken`、`internal/relay/video`、`internal/audit`、`internal/obs`
- 评审教训：`docs/reviews/codex-review-v1.md`、`docs/reviews/codex-review-v1.1.md`、`docs/reviews/2026-05-30-explain-analyze-units-1-3.md`
- 项目宪法：[CLAUDE.md](../../CLAUDE.md)

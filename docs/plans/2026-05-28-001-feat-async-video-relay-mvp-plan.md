---
title: "feat: 异步 text_to_video Relay 核心闭环（seedance 2.0）"
type: feat
status: active
date: 2026-05-28
origin: docs/brainstorms/2026-05-28-gateway-async-video-mvp-requirements.md
---

# feat: 异步 text_to_video Relay 核心闭环（seedance 2.0）

## Overview

为网关接入**首个异步多媒体能力**：火山 seedance 2.0 的 `text_to_video`（文生视频）。打通"业务提交 → 鉴权/entitlement/能力校验 → reserve 预扣 → Asynq 提交上游 → 回调优先/轮询兜底 → 按真实 token usage 结算 → TOS 落盘签名 URL → 业务轮询取结果"的端到端闭环，让 storyboard-assistant 把一个真实视频工作流灰度迁移到网关。

这是设计文档真正的 P0 核心（D-min/F-min 暂时绕过的异步任务链）。复用已建账本/鉴权/audit/metrics 骨架；引入 Asynq+Redis 异步执行基座（设计文档 §9 原计划）。

**第一刀定位（诚实声明）**：text_to_video 是工程上最干净、但价值上**非代表性**的切片——它恰好不需要人像库（虚拟/真人），且 storyboard-assistant 已直连跑通。本刀验证的是**接入管道打通 + 网关账本自洽 + 计费可对账**，**不是**网关核心差异化价值（屏蔽厂商/多 provider/人像库，均推后）。"换模型不改业务"在单 model/单 channel/单 provider 下无法被验证，需待下一刀（image_to_video+人像库 或 第二 provider）兑现。一次性建全套异步基座的理由：它是后续所有视频工作流不可绕过的前置，分阶段反而重复成本。

## Problem Frame

业务系统当前各自直连 seedance 2.0 + 自做积分。网关作为基础设施层（`docs/multimedia-gateway-design.md` §三bis）要收编这种各自为政的接入：统一 API、屏蔽厂商、统一计费、企业凭据隔离。详见 origin: `docs/brainstorms/2026-05-28-gateway-async-video-mvp-requirements.md`。

## Requirements Trace

- R1/R2. 统一提交端点 `POST /v1/video/generations`，第一刀仅 `text_to_video`（无输入媒体）。
- R3/R6/R7. env 单条 catalog + 仅驱动校验的能力描述符；换上游改 catalog 不改业务。
- R4/R5/R5a. 业务轮询 `GET /v1/video/generations/{id}`（只读本地状态）+ 查余额/用量；按 id 查询强制 `business_account_id` 归属，不符 404。
- R8/R10/R11. 提交流程（前置校验失败在 reserve 前短路）+ 状态机 CAS + 结算（commit≤reserve/release）+ 非二元结局 + 终态收敛 + ledger 反查对账。
- R9/R9a. 回调优先（火山支持）+ 轮询兜底；回调 ingress per-task token + 回调体 usage 不可信（commit 反查上游）+ 防重放。
- R12/R12a. Channel 5 段凭据 AES-GCM 加密（KEK）+ per 企业 project_id；凭据不入日志/write-only。
- R13. 最小 entitlement（账户→可用 model，403）。
- R14. token 口径计费（reserve 估 token 上界 → settle 真实 usage）+ catalog 价格 + 快照。
- R15. 账户×模型并发硬上限（跨副本一致 = **DB 原子 claim 权威**；Asynq 仅执行不作上限）→ 超限 429。
- R16/R17/R17a. 第一刀仅 TOS **结果**存储 + 签名 URL（TTL/单对象/不可枚举/不入日志）；输入媒体上传推后。
- R18. 运维 admin-cli/API 配置 channel/catalog/entitlement/并发。
- R19/R20/R21. audit（无 PII/凭据/签名URL）+ metrics + settle 重试终态契约 + 崩溃恢复。

## Scope Boundaries

- 仅 `text_to_video`；其余 task_type（图生/首尾帧/storyboard/续写）枚举占位但校验拒绝。
- 输入媒体 TOS 代理上传、人像库/真人验证、模型全局并发排队降级、catalog DB 化/多 model、能力描述符 UI/文档驱动、webhook 推送业务、OSS 月度对账、多 provider、运维 UI——全部推后。
- 同步 chat（F-min）不动。

## Context & Research

### Relevant Code and Patterns

- 账本：`internal/ledger/service.go` + `internal/ledger/postgres.go`（Reserve/Commit/Release；**硬约束**：Commit 校验 `ActualCost ≤ reserve.Amount`→`ErrCommitExceedsReserved`，Release 金额须 = reserve.Amount）。
- 同步 relay（**平行参照，不复用**）：`internal/relay/`（ProviderAdapter 仅 `ChatCompletion`；EnvCatalog；RelayHandler 单请求闭环）。异步需新 async adapter 接口。
- 鉴权：`internal/businesskey/`（HMAC pepper）+ `internal/httpapi/middleware/business_*.go`（中间件链可复用）。
- audit/metrics：`internal/audit/`、`internal/obs/metrics.go`（注册风格）。
- schema 骨架（已建，0 sqlc 查询，待接）：`migrations/0001_init.up.sql` 的 `task`（9 态 `task_status` 枚举 + `submit_locked_until`/`submit_recover_count`/`financial_snapshot` jsonb/`idx_task_inflight` 部分索引/`idx_task_submit_recover`）、`channel`（`credentials_encrypted` bytea + `key_version`）、`webhook_event_outbox`。
- 装配：`main.go`（F-min `/v1` 装配 + cleanup defer 模式）、`internal/config/config.go`（`RedisAddr`/`GatewayKEKV1` 已有占位）。

### Institutional Learnings

- `docs/solutions/` 当前为空，无既有沉淀。

### External References

- 参考实现 storyboard-assistant（`F:\AiWorkspace\storyboard-assistant\storyboard-assistant`，仅参考不照搬 new-api）：`backend/app/providers/seedance.py`（异步 submit/poll、`POST /contents/generations/tasks`→task_id、`GET .../{id}`、content[]+role、ratio/duration/quality/watermark/generate_audio）、`backend/app/services/video_model_capabilities.py`（能力描述符模式）、`backend/app/services/credit/*`（token 口径计费：`(model,resolution,has_input_video)→CNY/百万token`、estimate=`floor((in+out_dur)×W×H×fps/1024)`、`×100×倍率1.1`、settle 用 `usage.completion_tokens`）、`docs/superpowers/specs/2026-04-22-billing-gateway-design.md`（hold→settle = reserve→commit/release）。
- 设计文档 `docs/multimedia-gateway-design.md` §9（Asynq 异步执行）、§9ter（任务财务状态机/双快照/`UPSTREAM_SUBMITTING` 防孤儿）、§9.4（per-租户队列 concurrency）。**注**：§5.1 多单位架构（`per_video_second` / `per_1m_token` 并存）保留不变；seedance 这一 SKU 采用 token 单位（理由见参考实现），**不判 §5.1 过时**。

## Key Technical Decisions

- **异步执行基座 = Asynq + Redis（仅作执行器）**（设计文档 §9 原计划；Redis 容器已在运行 `api-gateway-redis`）：提交/结算/对账/恢复走 Asynq，享其重试/退避/调度/可重入 handler。**Asynq 不承载 R15 并发硬上限**——其 concurrency 是 server 进程级、跨副本不共享，队列级仅 priority/weight 而非硬上限。**需开 ADR-0006**（新依赖 hibiken/asynq + redis 客户端；ADR 内**一并定 TOS 接入方式 SDK vs 签名 REST**，含火山是否有可用 Go SDK / V4 签名工作量，避免推到实现期再做架构选型）。
- **R15 并发硬上限 = DB 原子 claim（权威）**：提交前用条件 INSERT / `SELECT ... FOR UPDATE` 计数行**原子占位**（账户×模型粒度），占不到 → 429。消除 check-then-act TOCTOU；DB 是单一真相源（设计文档 §9.5），无跨副本漂移。Asynq 队列仅作执行隔离/优先级，**不**作并发上限。
- **计费 = token 单位（per-SKU 选择，非网关级口径）**：seedance 这一 SKU 采用 token 单位。**设计文档 cost catalog 本就多单位（`per_video_second` / `per_1m_token` 并存），不改为单一 token 口径**——Unit 13 仅补注"seedance SKU 用 token 单位"，**不**判 §5.1 过时、**不**抹掉多单位架构（避免锁死未来 per-second/per-frame provider）。reserve 估 token 上界（按请求**实际分辨率档的 W×H** + 安全系数 → `CNY/百万token` 定价），settle 用上游真实 `usage.completion_tokens`；价格快照入 `financial_snapshot`。
- **catalog 第一刀 env 单 model 多档**（沿用 F-min env 模式，但**按分辨率档暴露 {W×H, 单价}**：480p/720p/1080p 各一组），仅绑一个 seedance channel；能力描述符仅驱动校验。**理由**：reserve 可证上界要求按请求实际档的 W×H/价格估算；单一价/单一 W×H 无法保证 settle ≤ reserve（会撞 `ErrCommitExceedsReserved`）。
- **异步 adapter 与同步 relay 平行**：新 `AsyncProviderAdapter`（Submit→upstream_task_id / Poll→status+usage），provider_type 扩 `volc_seedance`；不动现有 `ChatCompletion`。
- **凭据加密 = stdlib AES-GCM**（KEK 取 `GATEWAY_KEK_V1`，无新依赖）；解密 fail-closed；明文绝不入日志；密钥类字段绝不回显明文片段；**KEK 轮换语义**（`GATEWAY_KEK_V2` + key_version 解旧写新 + admin-cli 重加密命令）在 ADR-0006 定义，即使 KMS 推后。
- **回调优先 + 轮询兜底**：提交时注册带 per-task token 的回调 URL；**token 不放 query string**（放路径不可枚举段，或上游支持的 header）、**含 token 的 callback URL 不入 access/错误日志/span**；回调 handler 校验 token + 防重放 + **per-task 去抖（已有 settle 在队列/已终态则不再触发 Poll）**；**回调体 usage 不可信，commit 用量由网关 Poll 反查上游**；任务终态后置空 `callback_token`；Asynq scheduled fetch 作对账兜底；评估上游是否提供 HMAC 回调签名，有则补为第二道闸；本地测回调需内网穿透。
- **提交流程**：POST 内 reserve → **DB 原子 claim 占并发位** → 落 task（SUBMITTED）→ 入队 Asynq submit，立即返 task_id；submit worker CAS 推进。**双重提交防护**：我方 task_id 唯一 + 提交前 DB claim 拦入口重复提交；submit worker 调上游前先持久化提交意图。**上游既无幂等键、又无按我方标识 list/查询能力**（ADR-0006 已确认成立）→ recover **fail-closed**：`UPSTREAM_SUBMITTING` lease 过期**不自动重投**，超阈值直接 CAS→FAILED + release + 告警人工介入（宁可漏生成，不可双扣，符合失败优先原则）。
- **缺 usage / Poll 持续失败 = settle 失败（不猜扣额）**：上游返回成功但无 usage 字段、或 settle 内 Poll 持续失败 → **一律落 `settle_failed` 终态 + 告警 + 进对账队列**（人工/对账 worker 定真实扣额）。**不**按 reserve 上界全额 commit（避免系统性多收），**不**静默 release。
- **TOS 第一刀仅结果存储**（text_to_video 无输入媒体）：成功后产物落企业 bucket + 签名 URL（**TTL 最小化为业务取回所需，而非整个轮询窗口**；交付业务方，文档提示勿落入其日志）。

## Open Questions

### Resolved During Planning

- 异步基座：Asynq + Redis（用户确认），**仅作执行器**（重试/退避/调度/可重入），不承载并发上限。
- 首个 task_type：`text_to_video`（用户确认）。
- 计费单位：token（**per-SKU**；设计文档多单位 cost catalog 不变，仅 seedance SKU 用 token 单位，不判 §5.1 过时）。
- R15 并发硬上限：**DB 原子 claim 作权威**（用户确认 2026-05-28）；Asynq 队列不作上限。
- 缺 usage / Poll 持续失败：落 `settle_failed` + 对账队列（用户确认 2026-05-28），不按 reserve 上界 commit、不静默 release。
- 提交模型：reserve → DB claim → 落 task → 入队 Asynq submit（非内联提交上游）。

### Resolved by ADR-0006（2026-05-29，Ark 官方文档 + 生产参考实现交叉验证）

- **seedance 提交幂等键 / 按我方标识 list 反查：均不支持**（Ark 官方确认；`safety_identifier` 是终端用户标识非去重键，`filter` 仅 status/task_ids/model/service_tier）→ recover 定为 **fail-closed**：崩溃在「Submit 成功 → 存 upstream_task_id」之间无法安全判定，`UPSTREAM_SUBMITTING` lease 过期不自动重投，超阈值 CAS→FAILED + release + 告警；双提交防护由我方 task_id 唯一 + 提交前 DB claim 承载，「反查重投」分支已排除。
- **seedance usage 字段 = `usage.completion_tokens`**（视频模型 `total_tokens == completion_tokens`，输入 token 计 0）；**Seedance 2.0 有最低 token 计费下限**，reserve 上界与 settle 都须覆盖（见 Unit 7）。
- **TOS 接入 = 官方 Go SDK `ve-tos-golang-sdk/v2`**（Apache-2.0；`PutObjectV2` + `PreSignedURL(GET, Expires秒)`）；手动 `TOS4-HMAC-SHA256` V4 签名作去依赖 fallback 备查（见 Unit 9）。
- **上游时效**：status 6 态 `queued/running/cancelled/succeeded/failed/expired`（`cancelled` 仅排队中可取消，`execution_expires_after` 默认 48h）；`content.video_url` mp4 仅 **24h 有效**、提交 `id` 保留 **7 天** → TOS 转存须在完成后 24h 内（见 Unit 6/8/9）。

### Deferred to Implementation

- 火山 seedance 回调的精确 payload 格式 / 是否带上游签名(HMAC) / 回调 URL 注册字段名 / token 承载位置(path vs header)（实现时查官方文档，本地用内网穿透验证）。
- token 估算公式的安全系数具体值（确保按各分辨率档可证上界）。（usage 字段与最低 token 下限已由 ADR-0006 定稿，见上）
- entitlement 表结构（account×model 授权）的精确字段。
- （0005 列集冻结见 Unit 2：callback_token + `settle_failed` enum 值 + 核对设计文档示例引用的 `upstream_submitted_at` 等字段一次性定稿，再生成 sqlc。）

## High-Level Technical Design

> *以下示意意图与方向，供评审校验，不是实现规范。实现 agent 应当作上下文，而非照抄的代码。*

异步任务状态机与各承载者（CAS 转换，所有变更只走显式 from→to）：

```
[POST /v1/video/generations]
  鉴权/entitlement/能力校验(失败→reserve 前短路) → reserve 预扣
   → DB 原子 claim 占并发位(账户×模型;占不到→429) → 落 task → 入队 submit(Asynq)
        │
        ▼
   SUBMITTED ──(Asynq submit worker: CAS)──▶ UPSTREAM_SUBMITTING
        │                                          │ 提交上游(注入凭据+回调URL+token+幂等标识)
        │                                          ▼  存 upstream_task_id
        │                                    UPSTREAM_SUBMITTED
        │                                     │            │
        │            (回调优先: callback handler)     (兜底: Asynq scheduled fetch 轮询)
        │                                     ▼            ▼
        │             COMPLETED / FAILED / CANCELLED / EXPIRED(超时收敛)
        │                              │ (CAS 上游终态: 释放 claim + 赢家唯一入队 settle)
        ▼                              ▼
 (reconciler 兜底:               SETTLING ──▶ SETTLED(成功不可变终态)
  ① 扫 SUBMITTED 无 job→重投       │   commit(实际usage,≤reserve)/release;成功 TOS 落盘+签名URL
  ② 扫 ledger orphan reserve→释放) └─▶ settle_failed(失败终态: 缺usage/Poll持续失败/重试耗尽 → 告警+对账队列)
[GET /v1/video/generations/{id}] 业务只读本地 task 状态(强制归属校验)
```

> 注：`settle_failed` 为**第 10 个 `task_status`**（0001 的 9 态之外；enum 增值须独立 migration 文件，见 Unit 2）。**claim = 上游并发槽**：只数 SUBMITTED/UPSTREAM_SUBMITTING/UPSTREAM_SUBMITTED 三态，进上游终态（COMPLETED/FAILED/CANCELLED/EXPIRED）即释放；settle_failed 不持 claim。**不变量**：未结算 reserve（上游终态→SETTLED 窗口、及 settle_failed）不受 claim 约束，其资金敞口由 reserve 时 available 余额单独约束。

计费 token 单位（per-SKU；reserve 与 settle 同单位，仅 token 数来源不同）：

```
reserve_tokens(估上界) = ceil( (duration × W×H[按请求分辨率档] × fps / 1024) × 安全系数 )
reserve_minor = ceil( reserve_tokens / 1_000_000 × 单价CNY[按档] × 倍率 )  # 尺寸/价格按请求实际档,快照冻结
settle_tokens = upstream usage.completion_tokens                          # 网关反查上游,非回调体
settle_minor  = ceil( settle_tokens / 1_000_000 × 快照单价 × 快照倍率 )    # 须 ≤ reserve_minor
# 缺 usage / Poll 持续失败 → 不 commit 上界、不 release → settle_failed + 对账队列
```

## Implementation Units

### Phase 0 — 异步基座与依赖

- [x] **Unit 1: ADR + Asynq/Redis 装配**

**Goal:** 开 ADR 确立异步执行基座（Asynq+Redis）+ TOS 接入方式选型；引入依赖；装配 Asynq client/server + Redis 连接 + 配置 fail-fast。

**Requirements:** R8/R9/R15/R21（基座）

**Dependencies:** 无

**Files:**
- Create: `docs/adr/0006-async-execution-asynq-redis.md`（含 TOS SDK vs REST 决策）
- Modify: `go.mod` / `go.sum`（hibiken/asynq + redis 客户端；TOS 依赖按 ADR）
- Modify: `internal/config/config.go`（`REDIS_ADDR` 读取 + production fail-fast；新增 Asynq 队列/并发相关配置键）
- Create: `internal/asyncq/`（Asynq client + server 封装、队列命名约定、优雅停机）
- Modify: `main.go`（装配 Asynq server goroutine + Redis ping fail-fast + cleanup defer）
- Test: `internal/config/config_test.go`（Redis 配置校验）、`internal/asyncq/asyncq_test.go`（client 构造 + 队列命名）

**Approach:**
- ADR-0006 记录：选 Asynq+Redis 的理由（设计文档 §9 一致、Redis 已运行、原生重试/退避/调度/可重入 handler）、被否方案（进程内+DB轮询）；**明确 Asynq 仅作执行器、R15 硬上限由 DB 原子 claim 承载（不靠队列 concurrency）**；**TOS 接入 SDK vs 签名 REST 调研定稿**（含火山是否有可用 Go SDK / V4 签名工作量）；**KEK 轮换语义**（GATEWAY_KEK_V2 + key_version 解旧写新 + 重加密命令）。
- Redis 连接复用 `RedisAddr`；启动 ping fail-fast（与 pgxpool 同风格）。
- 队列命名 `biz_{account_id}`（执行隔离/优先级用）+ 默认/维护队列；**并发上限不在队列层表达**。

**Patterns to follow:** `main.go` newPGXPool fail-fast + cleanup defer；`internal/config` 既有 fail-fast 校验链。

**Test scenarios:**
- Happy path：Redis 可达时 Asynq client/server 构造成功；队列名按 `biz_{account}` 生成。
- Edge case：`REDIS_ADDR` 缺失/不可达 → 启动 fail-fast 报错（production）。
- Error path：Asynq server 启动失败 → main 返错并按序 cleanup。

**Verification:** 进程能起、Redis ping 通过、Asynq server 注册空 handler 不 panic；`go.mod` 变更有对应 ADR。

### Phase 1 — 数据层与凭据

- [x] **Unit 2: 0005 migration + channel/task/entitlement sqlc 查询**

**Goal:** 补 schema（task.callback_token、entitlement 表）+ 为 channel/task/entitlement 写全部 sqlc 查询，含 task 状态机 CAS。

**Requirements:** R8/R10/R11/R13/R12/R15

**Dependencies:** 无（可与 Unit 1 并行）

**Files:**
- Create: `migrations/0005_async_video_relay.up.sql` / `.down.sql`（task 加 `callback_token` text NULL；新增 `business_account_model_entitlement` 表；**新增并发计数行表 `account_model_concurrency`**（每 (account,model) 一行 `inflight int`，支持原子占位/释放）；核对并一次性补齐 adapter/快照所需列如 `upstream_submitted_at`——**列集冻结后再生成 sqlc**）
- **migration 拆分（PG enum 硬约束）**：`task_status` enum 增 `settle_failed` 值**必须单独成一个 migration 文件**（PG 不能在同事务 USE 新增 enum 值，见 0004 先例）；**重建 `idx_task_inflight` 把 `settle_failed` 纳入排除谓词**也须放在 ADD VALUE 之后的独立 migration 文件；TOS 的 `oss_object_meta`（原 Unit 9 拟 0006）相应顺延编号
- Create: `sql/queries/channel.sql`（CRUD + 按 id/provider 查活跃）
- Create: `sql/queries/task.sql`（insert、get、`CompareAndSwapTaskStatus(id, from, to, ...)` 显式 from/to、inflight count by (account,model)、recover scan by `submit_locked_until`、按 upstream_task_id 查、终态收敛扫描）
- Create: `sql/queries/entitlement.sql`（grant/revoke/check account×model）
- Modify: 生成的 `internal/db/*`（sqlc generate）
- Modify: `docs/db/schema.md`（追加 0005 演化）
- Test: `internal/db` 查询的集成测试归各 service 单元（见 Unit 3/6）

**Approach:**
- task 状态机 CAS：`UPDATE task SET status=@to ... WHERE id=@id AND status=@from`，返回受影响行数判断 CAS 成败（CLAUDE.md 状态机模式硬约束：必带 from 条件）。
- **R15 原子 claim（单一形态）**：每 (business_account_id, gateway_model) 一行计数器，占位 = `UPDATE account_model_concurrency SET inflight=inflight+1 WHERE (account,model)=... AND inflight<cap RETURNING inflight`（影响 0 行 = 占不到 = 429）；释放 = `UPDATE ... SET inflight=inflight-1 WHERE inflight>0`，**在进上游终态（COMPLETED/FAILED/CANCELLED/EXPIRED）的 CAS 赢家同事务内执行**（claim=上游并发槽，只数 SUBMITTED/UPSTREAM_SUBMITTING/UPSTREAM_SUBMITTED 三态；settle_failed 不持 claim）。**不**用 `count(*) ... FOR UPDATE`（挡不住并发新行 INSERT 的幻读 TOCTOU）。计数行首次提交 ON CONFLICT lazy upsert。`idx_task_inflight` 仅供 reconciler 扫卡住任务，不作 cap 计数。
- entitlement 表：`(business_account_id, gateway_model)` 唯一 + 时间戳。
- migration 必须 up/down 配套；down 删新表 + 删 callback_token 列（**enum 增的 `settle_failed` 值不回删**，PG enum 不支持删值）。

**Patterns to follow:** `migrations/0004_*`（up/down 风格）；`sql/queries/business_account_api_key.sql`（sqlc 写法）；CONTEXT.md 状态机模式。

**Test scenarios:** Test expectation: none —— 本单元为 schema + 查询生成；行为测试在调用方 service 单元（Unit 3/6/7）覆盖。

**Verification:** `make sqlc` 通过 + diff guard 干净；`make migrate-up`/`down`/`up` 往返成功；schema.md 同步。

- [x] **Unit 3: 凭据加解密（AES-GCM）+ Channel service**

**Goal:** 实现 KEK 派生的 AES-GCM 凭据加解密 + Channel 5 段凭据结构 + CRUD service，凭据明文绝不入日志、解密 fail-closed。

**Requirements:** R12/R12a/R18（凭据部分）

**Dependencies:** Unit 2

**Files:**
- Create: `internal/crypto/envelope.go`（AES-GCM 加解密，KEK 取 `cfg.GatewayKEKV1`，key_version 标记）
- Create: `internal/channel/types.go`（ChannelCredentials 5 段：APIKey/ARK AK-SK/TOS AK-SK/bucket/project_id）
- Create: `internal/channel/service.go` + `internal/channel/postgres.go`（CRUD + 加解密；GetDecrypted 仅内部最小作用域；List/Get 不回显明文）
- Test: `internal/crypto/envelope_test.go`、`internal/channel/postgres_test.go`

**Approach:**
- AES-GCM：随机 nonce 前置；密文存 `credentials_encrypted` bytea + `key_version`；解密失败返 sentinel error（调用方 fail-closed 拒绝提交）。
- ChannelCredentials marshal 为 JSON 后整体加密。
- service 提供 write-only 创建/更新 + 掩码视图；**密钥类字段（ARK Secret Key / TOS Secret Key）一律显示固定占位（如"已设置"），绝不回显任何明文片段**；末 N 位掩码仅用于非机密标识符（bucket / project_id / APIKey 前缀）。明文仅 `GetCredentialsForUpstream(ctx, channelID)` 返回，调用即用即弃。

**Execution note:** 涉凭据加密，测试先行覆盖加解密往返 + 边界（CLAUDE.md：涉凭据必须附边界单测）。

**Patterns to follow:** `internal/businesskey/postgres.go`（service+postgres 分层 + rowmap）。

**Test scenarios:**
- Happy path：5 段凭据 encrypt→decrypt 往返一致；key_version 正确标记。
- Edge case：空凭据/超长字段；nonce 唯一性（两次加密同明文密文不同）。
- Error path：密文被篡改/截断 → 解密返 error（GCM 认证失败）；错误 KEK → 解密失败；解密失败时 service 不返回明文。
- Integration：创建 channel 后 List/Get 返回掩码不含明文；DB 中 `credentials_encrypted` 非明文可读。
- 安全断言：日志/错误信息中不出现凭据明文片段。

**Verification:** 加解密往返 + 篡改检测测试通过；channel CRUD 集成测试（真 PG）通过；明文不出现在任何返回/日志。

### Phase 2 — 模型适配

- [x] **Unit 4: video catalog + 能力描述符（校验级，env 单条）**

**Goal:** 为视频提供 env 单条 catalog（gateway-model → seedance channel 绑定 + pricing + 能力描述符），能力描述符驱动 text_to_video 请求校验。

**Requirements:** R3/R6/R7/R14（价格来源）

**Dependencies:** Unit 3（channel 绑定）

**Files:**
- Create: `internal/relay/video/catalog.go`（VideoCatalog 接口 + EnvVideoCatalog 单条；含 pricing + capability descriptor + channel 绑定）
- Create: `internal/relay/video/capability.go`（声明式 capability：task_type 支持集 + params[]（key/type/enum/min-max/default/required））
- Create: `internal/relay/video/validate.go`（按 capability 校验请求：task_type 支持、参数取值档、必填）
- Modify: `internal/config/config.go`（新增 `GATEWAY_VIDEO_RELAY_*` env：model/upstream_model/channel 绑定/pricing/duration·resolution·fps 取值档）
- Test: `internal/relay/video/catalog_test.go`、`internal/relay/video/validate_test.go`

**Approach:**
- 第一刀 capability 仅声明 `text_to_video` + 校验所需字段子集（prompt 必填、duration[4-15]、resolution{480p/720p/1080p}、ratio 枚举、fps）；其余 task_type 不在支持集 → 校验拒绝。
- pricing **按分辨率档**：`(resolution, has_input_video=false)→{W×H, CNY/百万token, 倍率}`（480p/720p/1080p 各一组），env 配置；对齐参考实现价格表 + `_resolution_dimensions`。**catalog 必须暴露每档 W×H**，供 reserve 按请求实际档算可证上界（单一价/单一 W×H 无法保证 settle ≤ reserve）。
- 能力描述符预留 `schema_version` 扩展位（供后续 UI/文档驱动），第一刀其余扩展字段可空。
- routing_keys/UI/文档驱动不做（推后）。

**Patterns to follow:** `internal/relay/catalog.go`（EnvCatalog fail-fast 校验风格）；storyboard-assistant `video_model_capabilities.py` 的描述符结构。

**Test scenarios:**
- Happy path：合法 text_to_video 请求（prompt+duration+resolution）通过校验；catalog 返回正确 channel/pricing/capability。
- Edge case：duration 边界（4/15 通过，3/16 拒）、resolution 非枚举值拒、prompt 缺失拒。
- Error path：task_type=image_to_video（不支持）→ 校验拒绝（明确错误码）；env 配置缺字段 → catalog 构造 fail-fast。
- 校验输出形状：拒绝时返回足够信息映射为 OpenAI 兼容 400（type/code）。

**Verification:** catalog 构造 fail-fast 矩阵 + 校验通过/拒绝矩阵测试全绿。

- [x] **Unit 5: seedance 异步 provider adapter**

**Goal:** 实现 `AsyncProviderAdapter`（Submit/Poll），对接 seedance 视频 API（提交 + 查询），注册带 token 的回调 URL，映射上游状态到 task 状态。

**Requirements:** R8/R9（上游交互）

**Dependencies:** Unit 3（凭据）、Unit 4（catalog）

**Files:**
- Create: `internal/relay/video/provider_adapter.go`（`AsyncProviderAdapter` 接口：`Submit(ctx, entry, params, callbackURL)→(upstreamTaskID, error)`、`Poll(ctx, entry, upstreamTaskID)→(status, usage, resultURL, error)`）
- Create: `internal/relay/video/seedance_adapter.go`（OpenAI 不兼容形态：`POST /contents/generations/tasks`、`GET .../{id}`；content[] 文本、ratio/duration/quality/watermark/generate_audio；Authorization 注入企业 APIKey）
- Create: `internal/relay/video/upstream_status.go`（上游状态多别名 → COMPLETED/FAILED/RUNNING 收敛；usage 字段解析）
- Test: `internal/relay/video/seedance_adapter_test.go`（httptest mock 上游）

**Approach:**
- 业务参数改写为 seedance 原生 body（model→上游真实 model 名，prompt→content[type=text]）；其他参数透传。
- callbackURL 含 per-task token（Unit 8 生成，置于路径不可枚举段或上游支持的 header，不入 query），注册给上游（字段名实现时查官方文档）。
- **双重提交防护（fail-closed）**：上游**无幂等键、无法按我方标识反查**（ADR-0006 官方确认）→ adapter **不**实现 `PollByIdempotency`，仅提供按**上游 task_id** 的 `Poll`；崩溃在「Submit 成功 → 存 upstream_task_id」之间由 Unit 6 recover **fail-closed** 兜底（不重投，超阈值 FAILED+release+告警）。
- Poll 返回规范化状态 + usage（completion_tokens）+ 产物 URL；状态别名收敛参照 storyboard-assistant。
- 网络错误/超时/非法 JSON 分类为 sentinel（参照同步 relay 的 classifyClientErr 思路，但不复用代码）。

**Patterns to follow:** `internal/relay/openai_compat.go`（HTTP 调用 + 错误分类 + body 限读思路，平行不复用）；storyboard-assistant `seedance.py`（调用形态、状态别名集、video_url 抽取）。

**Test scenarios:**
- Happy path：Submit 返 upstream_task_id；Poll 命中 succeeded → 返 COMPLETED + usage + video_url。
- Edge case：上游 running/pending 多别名 → 归 RUNNING；succeeded 但缺 usage → 返特殊标记（交 settle 兜底）。
- Error path：上游 4xx（参数错）/5xx/超时/连接失败/非法 JSON → 各自 sentinel；Poll 未知 task → error。
- Integration：mock 上游全流程 submit→poll(running)→poll(succeeded)，验证状态机推进与 usage 解析。

**Verification:** mock 上游下 Submit/Poll 全路径测试通过；状态别名/usage 解析正确。

### Phase 3 — 异步任务闭环

- [x] **Unit 6: 任务状态机 + 提交流程 + Asynq workers**（6a 提交流程/FSM/settle + 6b 周期兜底/崩溃恢复 sweep，均已 ce-review + 推送）

**Goal:** 实现 task service（提交流程 + 状态机 CAS）+ Asynq submit/fetch-reconciler/settle workers + 崩溃恢复 + 终态收敛；R15 并发硬上限由 **DB 原子 claim**（Unit 2 计数行）承载，Asynq 队列仅作执行隔离不作上限。

**Requirements:** R8/R10/R11/R15/R21

**Dependencies:** Unit 1/2/3/4/5

**Files:**
- Create: `internal/task/service.go`（提交流程：鉴权后 reserve → 入队 submit；状态机 CAS 封装；financial_snapshot 组装）
- Create: `internal/task/workers.go`（Asynq handlers：submit（CAS UPSTREAM_SUBMITTING→调 adapter.Submit→存 upstream_task_id→CAS UPSTREAM_SUBMITTED）、settle（reserve→commit/release + TOS 落盘触发）、fetch_reconciler（scheduled：扫 UPSTREAM_SUBMITTED 超时/卡住→Poll→推终态→入队 settle；并扫 status=SUBMITTED 且 submitted_at 超阈值 = 入队丢失/Redis 抖动导致无 Asynq job 的任务 → 幂等重投 submit job，入队前用 CAS/lease 防与正常 worker 竞争）、recover（扫 `submit_locked_until` 过期的 UPSTREAM_SUBMITTING 任务：**fail-closed**——上游无幂等键/不可反查(ADR-0006)，不自动重投，CAS→FAILED + release + 告警人工介入；注：SUBMITTED 且无 Asynq job 者由 fetch_reconciler 安全重投，因其确未调上游）、expire（超最长执行期→EXPIRED+release））
- Create: `internal/task/snapshot.go`（TaskFinancialSnapshot：授权快照 + 价格快照 + reserve ledger ref）
- Modify: `main.go`（注册 Asynq handlers + scheduled tasks；队列 concurrency 仅作执行隔离，**R15 上限走 DB 原子 claim 不在此装配**）
- Test: `internal/task/service_test.go`、`internal/task/workers_test.go`（真 PG + mock adapter + 并发/重入测试）

**Approach:**
- 提交事务边界：先 reserve（独立 ledger tx），再在**单 tx 内 DB claim 占位 + 落 task**；该 tx 失败立即 Release。崩溃在 reserve 与 task tx 之间 → orphan reserve，由 **reconciler 显式查询**兜底：扫 ledger active reserve 反查无对应 task 行者 → Release（落成具体 worker/SQL）；**加最小年龄阈值**（`reserve.created_at < now()-N`，N ≫ reserve→task tx 正常间隔），只回收确陈旧者，**避免误回收 in-flight 窗口内、task tx 即将提交的 reserve**（否则该 task settle 时反查不到 reserve → 资金损失）。
- 所有状态变更走 `CompareAndSwapTaskStatus`（显式 from/to）；handler 可重入（CAS 已推进则放弃 + ledger correlation 幂等）。**双路 settle 去重**：入队 settle **只由 CAS 推终态的赢家执行**（回调与 reconciler 竞争时仅一方 CAS 成功）；即便双 settle job 都跑，靠同 correlation 命中账本幂等兜底。**丢失 settle job 恢复**：reconciler 另扫"已进上游终态（COMPLETED 等）但超阈值未进 SETTLED/settle_failed"的任务 → 幂等重投 settle（"只由赢家入队"仅约束 transition 去抖；恢复路径允许非赢家重投，安全靠 correlation 幂等 + Commit 的 `ErrAlreadySettled` 兜底，避免丢 job → reserve 永久锁死）。
- settle 用独立 ctx；**settle 内 Poll 反查有独立超时 + 有限重试**；Poll 持续失败而任务已终态 → 落 `settle_failed` + 对账队列（**不**无限重试、**不**按上界 commit），与缺 usage 同口径（见 Unit 7）；commit 实际 usage（≤reserve，超则 cap+告警，对齐账本硬约束）。
- **并发上限 = DB 原子 claim（权威）**，非队列 concurrency。依赖顺序：claim 占位/释放查询在 Unit 2，上限值解析（`concurrency.go`）在 Unit 8；Unit 6 先用静态默认上限独立运行/测试，Unit 8 接入覆写。
- 终态收敛不变量：所有 task 必在有限时间进终态（expire worker 兜底）；**claim 在进上游终态（COMPLETED/FAILED/CANCELLED/EXPIRED）的 CAS 同事务释放**（claim=上游并发槽，settle_failed 不持 claim）；未结算 reserve 不受 claim 约束，资金敞口由 available 余额约束。

**Execution note:** 涉状态机 + 账本 + 并发，测试先行覆盖 CAS 并发与重入幂等（CLAUDE.md：涉账本/状态机必须并发测试）。

**Patterns to follow:** 设计文档 §9.5 可重入 handler 范式；`internal/ledger` CAS 重试风格；F-min `relay/handler.go` 的 settle 独立 ctx + 重试。

**Test scenarios:**
- Happy path：提交→SUBMITTED→submit worker→UPSTREAM_SUBMITTED；回调/poll→COMPLETED→settle→SETTLED；ledger reserve+commit 落账。
- Edge case：submit worker 并发抢同一 task → 仅一个 CAS 成功；重复 settle/回调 → 幂等（状态机 CAS + ledger correlation 双层）。
- Error path：上游 submit 失败 → release + FAILED；上游永不返终态 → expire worker EXPIRED + release；UPSTREAM_SUBMITTING 的 submit_locked_until 过期 → recover **fail-closed**：FAILED + release + 告警（**不**回 SUBMITTED 重投，避免上游 double-submit，ADR-0006）；SUBMITTED 无 job 者由 reconciler 安全重投。
- Integration：提交→submit worker 崩溃于 UPSTREAM_SUBMITTING（submit_locked 未释放）→ recover **fail-closed**：FAILED+release+告警（不重投，验证不双扣）；reserve 后 task 落库失败 → Release 兜底无 orphan；入队丢失（task=SUBMITTED 无 Asynq job）→ reconciler 扫 SUBMITTED 超阈值 → 幂等重投 → 完成。
- 并发：N 并发提交同账户×模型，inflight 计数正确（配合 Unit 8）。

**Verification:** 真 PG 下全状态机路径 + 并发 CAS + 重入幂等 + 崩溃恢复测试通过；无 orphan reserve。

- [x] **Unit 7: 计费（token 口径 reserve→settle）**（billing.go: `EstimateReserveMinor` reserve 权威公式 + `BilledMinorCeil` 单一换算真相源(溢出饱和 fail-closed)；settle 侧复用 6a 的 `snapshot.SettleMinor`，跨包 settle≤reserve 不变量测试钉死不漂移；已 ce-review + 推送。**残留(按计划/ADR 延后)**：安全系数 1.2× 与 adaptive 档 W×H 上界须 Unit 5 实测校准；`MinTokenFloor` 单一配置源由 Unit 10 接线，settle≤reserve 由账本 cap 兜底）

**Goal:** 实现 reserve 估算（token 上界）与 settle 实算（真实 usage），价格走 catalog + 快照，保证 settle ≤ reserve。

**Requirements:** R14/R10

**Dependencies:** Unit 4（pricing）、Unit 6（接入提交/结算）

**Files:**
- Create: `internal/relay/video/billing.go`（`EstimateReserveMinor(params, pricing)→(reserveTokens, reserveMinor)`、`SettleMinor(usageTokens, snapshotPricing)→minor`，ceil 除法、可证上界安全系数）
- Modify: `internal/task/service.go` / `internal/task/workers.go`（提交时算 reserve + 快照价格；settle 时算实际 + cap≤reserve）
- Test: `internal/relay/video/billing_test.go`

**Approach:**
- estimate：`reserve_tokens = ceil((duration × W×H[按请求分辨率档] × fps/1024) × 安全系数)`；`reserve_minor = ceil(reserve_tokens/1e6 × 单价[按档] × 倍率)`。**W×H 与单价取请求实际分辨率档**（catalog 暴露各档）；安全系数确保 ≥ 该档真实上界。**reserve_tokens 另取 `max(估算值, Seedance 2.0 最低 token 计费下限)` 作下界保护**（下限值见 ADR-0006，落地核对官方），确保覆盖上游最低计费。
- settle：`settle_tokens = max(usage.completion_tokens, 最低 token 下限)`（与上游计费口径对齐）、`settle_minor = ceil(settle_tokens/1e6 × 快照单价 × 快照倍率)`；若 > reserve（疑似异常）→ cap 到 reserve + 告警 metric。**缺 usage / Poll 持续失败 → 落 `settle_failed` + 告警 + 对账队列**（用户确认政策：不按 reserve 上界全额 commit，不静默 release）。
- **settle_failed 账务收敛契约**：reserve 在 settle_failed 期间仍锁在 `reserved`（claim 已在上游终态释放，**不**占并发）；对账 worker / admin-cli `reconcile settle` 必须在 SLA 内按上游真实用量补 commit（≤reserve）或确认未生成则 release；定义**最长滞留时限 + 超时告警升级**，避免 `available` 被长期低估、合法请求误拒 402。
- 价格快照存 `financial_snapshot`，settle 用快照价（调价不影响 inflight）。
- **correlation 钉死**：reserve 与 commit/release **必须用同一 correlation_id**（取自 `financial_snapshot.ReservationCorrelationID`，按 task_id 固定派生），且派生规则不得与账本内部 `:release` 后缀冲突。settle 反查用该 correlation 命中 active reserve（否则 `ErrReserveNotFound` → orphan reserve 钱锁死）。测试断言：用 reserve 的 correlation 反查能命中。

**Patterns to follow:** F-min `relay/handler.go` computeReserveMinor/computeCostMinor + 缺 usage 兜底；storyboard-assistant `credit/pricing.py` 估算公式。

**Test scenarios:**
- Happy path：720p/5s/24fps → reserve_tokens/reserve_minor 计算正确；settle 用真实 token < reserve → commit 实际 + 差额 release。
- Edge case：分辨率/时长/fps 各档价格正确；ceil 取整边界；safety 系数使 reserve ≥ 各档真实上界；**极短时长命中最低 token 计费下限时 reserve/settle 均不低于下限**。
- Error path：settle > reserve → cap 到 reserve + 告警；上游缺 usage / Poll 持续失败 → 落 settle_failed + 对账队列（不 commit 上界、不 release）。
- 对账：同一任务 reserve_minor / settle_minor 与参考实现口径一致（容差内）。

**Verification:** 计费矩阵测试全绿；reserve 可证 ≥ settle；缺 usage / 超额兜底路径覆盖。

- [x] **Unit 8: 回调 ingress + 安全 + 账户×模型并发**（callback_handler.go + callback_token.go(per-task token 生成/常量时间校验/HandleCallback 去抖) + concurrency.go(并发上限值解析，接入 service claim) + middleware/callback.go(全局令牌桶限速 + body 上限，零新依赖) + config/main 装配；回调体不可信(Poll 反查)、终态置空 token、token 仅路径段不入日志；复用 6a/6b 的 markUpstreamTerminal CAS 推终态 + 赢家唯一入队 settle。已 ce-review(并发/重放/跨任务 token/时钟回拨/cap=0 禁用测试齐备；adversarial double-release P0 经 PG 行锁串行化证伪 + 并发回调测试钉死)+推送。**残留(延后)**：限速 per-replica(MVP 单副本)、并发覆写热加载/DB 化(Unit 11)、上游回调超时预算实测校准(Unit 5)）

**Goal:** 实现回调接收端点（per-task token 校验、回调体不可信、防重放、限速）+ 账户×模型并发硬上限（**DB 原子 claim 权威**，非 Asynq 队列 concurrency）。

**Requirements:** R9/R9a/R15

**Dependencies:** Unit 6

**Files:**
- Create: `internal/httpapi/callback_handler.go`（`POST /v1/callbacks/video/{task_id}`：校验 token、查 task、CAS 终态 + 入队 settle；回调体仅作触发，usage 由 settle 时 Poll 反查）
- Create: `internal/task/callback_token.go`（per-task 随机 token 生成 + 常量时间校验 + 与 task_id 绑定）
- Create: `internal/relay/video/concurrency.go`（账户×模型并发上限**值**解析：默认 + 覆写；R15 实施 = Unit 2 的 **DB 原子 claim** 查询，**不**映射 Asynq 队列 concurrency）
- Modify: `internal/httpapi/middleware/`（回调端点独立限速 + body 大小上限；提交前 inflight 超限 → 429）
- Modify: `main.go`（注册回调路由 + 队列 concurrency 配置）
- Test: `internal/httpapi/callback_handler_test.go`、`internal/task/callback_token_test.go`、`internal/relay/video/concurrency_test.go`

**Approach:**
- 回调 token：提交时生成随机 token 存 `task.callback_token`，**置于回调 URL 路径不可枚举段或上游支持的 header（不放 query string）**；回调校验常量时间比较 + 绑定 task_id；缺/错 token → 401 不改状态；**含 token 的 callback URL 不入 access/错误日志/span**。
- 回调体 status/usage **不可信**：回调仅触发"去查"，commit 用量由 settle worker Poll 反查上游。评估上游 HMAC 回调签名，有则补为第二道闸。
- 防重放 + **去抖**：状态机 CAS 保证终态只推进一次；ledger correlation 长期幂等；**同一 task 已有 settle 在队列/已终态 → 不再触发 Poll**（防泄露 token 强制重复 Poll 的放大攻击）。
- token 失效：任务进终态后置空 `task.callback_token`，后续携带该 token 的请求一律走"已终态 → 200 忽略"分支。
- 未知/已终态 task_id → 快速 200/忽略（不触发昂贵反查）；限速 + body 上限防刷。
- **并发 = DB 原子 claim（权威）**：提交前 Unit 2 的原子 claim 占位（账户×模型），占不到 → 429（唯一能即时返 429 的路径）；Asynq 队列不作上限。

**Execution note:** 公网入口 + 资金触发，测试先行覆盖伪造/重放/越权回调。

**Patterns to follow:** F-min `middleware/business_*.go` 限速/body 限制；设计文档 §9.4 队列 concurrency。

**Test scenarios:**
- Happy path：合法 token 回调 → CAS 终态 + 入队 settle；inflight < 上限 → 提交通过。
- Edge case：未知 task_id / 已终态 task → 200 忽略不反查；inflight = 上限 → 提交 429。
- Error path：缺 token/错 token → 401 不改状态；重放同一回调 → CAS 幂等只结算一次；伪造 succeeded+高 usage → 不被采信（commit 以反查为准）。
- 并发：多副本/多并发提交同账户×模型，DB 原子 claim 跨副本一致不超卖（含 **TOCTOU 竞争测试**：N 并发恰好卡上限，仅 cap 个成功 claim、其余 429）。

**Verification:** 伪造/重放/越权回调被拒；并发上限跨副本生效；回调体 usage 不影响最终扣费。

### Phase 4 — 对外面、存储、运维

- [x] **Unit 9: TOS 结果存储 + 签名 URL**（internal/storage/tos.go: ObjectStore 接口 + 火山官方 SDK ve-tos-golang-sdk/v2 实现(PutObjectV2+ForbidOverwrite / PreSignedURL 离线现签 / ErrObjectExists 归一)；migrations/0010 oss_object_meta + 专用部分索引 + sqlc；独立 video:store Asynq job：settle 成功→入队→re-poll 产物 URL→fetch→Put→写 meta，确定性 key 幂等；6b fetchReconcileOnce 加 recoverMissingStore 兜底(24h 窗内)。签名 URL 读时现签不入库。已 ce-review(security/adversarial/reliability/data-migrations/correctness/testing/project-standards)：修 projectID 路径穿越净化、产物 URL SSRF 防护(禁重定向+scheme+私网拒绝,测试放行环回)、429→可重试、超 24h 毒丸防护、ScanSettledNeedingStore 部分索引、go mod tidy(ve-tos direct)；补 fetch 分类/SSRF/key 净化/recover 负向过滤测试。已推送。**残留**：真实 TOS PutObjectV2 仅 fake 覆盖(待 bucket 凭据集成测)、DNS rebinding、签名 URL TTL/读路径=Unit 10。）

**Goal:** 任务成功后将产物落企业 TOS bucket + 生成受限签名 URL，记录 oss_object_meta。

**Requirements:** R16（仅结果）/R17/R17a

**Dependencies:** Unit 1（TOS 接入方式）、Unit 3（TOS 凭据）、Unit 6（settle 触发）

**Files:**
- Create: `internal/storage/tos.go`（上传产物 + 生成签名 URL；**ADR-0006 定用官方 SDK `ve-tos-golang-sdk/v2`**：`PutObjectV2`（建议 `ForbidOverwrite` 防覆盖）+ `PreSignedURL(GET, Expires秒)`；凭据取自 channel，即用即弃）。手动 `TOS4-HMAC-SHA256` V4 预签名为去依赖 fallback（已验证可行，本单元不实现）。
- Modify: `internal/task/workers.go`（settle 成功后落盘 + 写 oss_object_meta + 把签名 URL 写入 task 结果）
- Create: `migrations/0006_oss_object_meta.up.sql`/`.down.sql`（已核实 0001–0004 均无此表 → 无条件 `CREATE TABLE`，down 无条件 `DROP TABLE`；不要用"若不存在则建"的运行期条件，golang-migrate 无此分支语义）
- Test: `internal/storage/tos_test.go`（mock TOS endpoint）

**Approach:**
- 产物从上游 video_url 拉取 → 上传企业 bucket（project_id 隔离）；对象 key 含不可枚举随机段。**上游 `video_url` 仅 24h 有效（ADR-0006）→ 转存须在任务完成后 24h 内完成；超时未转存视为失败转人工对账。**
- 签名 URL：单对象 GET 只读、**TTL 最小化为业务取回所需（非整个轮询窗口）**，可配；URL 不入 audit/日志，文档提示业务方勿落入其日志。
- 上传失败 → 任务标记需人工对账（不影响已 commit，告警）。

**Patterns to follow:** storyboard-assistant `tos_storage.py`（上传 + 签名 URL 流程）。

**Test scenarios:**
- Happy path：mock TOS 上传成功 → 返签名 URL（TTL/单对象 GET）；oss_object_meta 落库。
- Edge case：对象 key 随机不可枚举；TTL 配置生效。
- Error path：TOS 上传失败 → 任务标记 + 告警，不破坏已 commit 账目。
- 安全断言：签名 URL 不出现在日志/audit。

**Verification:** mock TOS 全路径测试通过；签名 URL 受限且不入日志。

- [ ] **Unit 10: 业务对外 API（提交/查询）+ 跨租户隔离 + entitlement**

**Goal:** 暴露 `POST /v1/video/generations`（提交 text_to_video）+ `GET /v1/video/generations/{id}`（轮询本地状态）+ 查余额/用量；强制跨租户归属（404）+ entitlement 校验（403）。

**Requirements:** R1/R2/R4/R5/R5a/R13/R8（前置短路）

**Dependencies:** Unit 4/6/7/8

**Files:**
- Create: `internal/httpapi/video_handler.go`（提交：bind→entitlement→能力校验→reserve→入队；查询：读本地 task 强制 business_account_id 归属）
- Create: `internal/relay/video/errors.go`（OpenAI 兼容错误映射：400/401/402/403/404/429/5xx）
- Modify: `main.go`（注册 `/v1/video/*` 路由组，复用 business 中间件链）
- Modify: `internal/businesskey` 或新 entitlement service（account×model 校验，复用 Unit 2 查询）
- Test: `internal/httpapi/video_handler_test.go`（含跨租户越权 + entitlement + 前置短路）

**Approach:**
- 提交链：鉴权（business key 中间件）→ 跨租户/entitlement → 能力校验 → reserve → 入队 → 返 task_id（202/200）。任一前置失败在 reserve **前**短路（403/400/402）。
- 查询：`GET {id}` 只读本地 task；WHERE business_account_id = 当前账户，不符 404；不触发上游查询。
- 余额/用量查询同样强制归属。
- 错误 OpenAI 兼容形状（复用 F-min ErrorResponse 思路）。

**Execution note:** 多租户资金接口，测试先行覆盖跨租户越权（A 查 B 的 task → 404）。

**Patterns to follow:** F-min `relay/handler.go` + `relay/errors.go`（OpenAI 兼容错误）；business 中间件链。

**Test scenarios:**
- Happy path：合法提交 → 返 task_id（SUBMITTED）；GET 自己的 task → 状态/结果。
- Edge case：max_tokens/duration 越界 → 400；未开通 model → 403；余额不足 → 402（reserve 前）。
- Error path：缺鉴权 → 401；A 用自己 key 查 B 的 task_id → 404（不泄露存在性）；引用 B 的资源 → 404。
- Integration：提交→GET 轮询直到 succeeded，验证状态流转对业务可见；前置校验失败不产生 reserve。

**Verification:** 提交/查询全路径 + 跨租户越权 404 + entitlement 403 + 前置短路无 orphan reserve 测试通过。

- [ ] **Unit 11: 运维 admin-cli / Admin API 配置**

**Goal:** 运维通过 admin-cli/Admin API 管理 channel（write-only 凭据）、维护 video catalog/pricing、给账户开通 model（entitlement）、设并发上限。

**Requirements:** R18

**Dependencies:** Unit 3（channel）、Unit 4（catalog）、Unit 2（entitlement）

**Files:**
- Create: `cmd/admin-cli/cmd/channel.go`（create/list/update，凭据 write-only 不回显）
- Create: `cmd/admin-cli/cmd/entitlement.go`（grant/revoke/list account×model）
- Create: `cmd/admin-cli/cmd/video_model.go`（catalog/pricing/并发上限 维护；若 env 单条则为查看 + 覆写并发）
- Modify: `cmd/admin-cli/cmd/root.go`（注册子命令）、`cmd/admin-cli/cmd/cli_wiring.go`（服务装配 + cleanup）
- Test: `cmd/admin-cli/cmd/channel_test.go`、`entitlement_test.go`（真 PG）

**Approach:**
- channel create：接 5 段凭据（从 env/文件读，不落 shell history 风险提示），加密入库；list/get 掩码。
- entitlement grant/revoke：account×model。
- 并发上限：账户×模型默认 + 覆写写入配置（Asynq 队列 concurrency 来源）。

**Patterns to follow:** `cmd/admin-cli/cmd/business_key.go`（create/list/revoke + exitCoder + --out 模式）。

**Test scenarios:**
- Happy path：channel create → 入库加密；list 掩码不含明文；entitlement grant → check 通过。
- Edge case：channel 缺凭据字段 → 友好错误；entitlement 重复 grant 幂等。
- Error path：FK 不存在账户 grant → 友好中文错误；revoke 不存在 → exit 2。
- 安全断言：list/get 输出不含凭据明文。

**Verification:** 各子命令真 PG 集成测试通过；凭据 write-only 不回显。

### Phase 5 — 可观测与文档

- [ ] **Unit 12: 可观测 + audit + 可靠性收尾**

**Goal:** 补 metrics + business_video audit（无 PII/凭据/签名URL）+ settle 重试终态契约 + 灰度可回滚/对账支持。

**Requirements:** R19/R20/R21 + 成功标准（可对账/可回滚）

**Dependencies:** Unit 6/7/8/9/10

**Files:**
- Modify: `internal/obs/metrics.go`（video task 提交/成功/失败、settle 失败、并发拒绝、回调接收、对账兜底触发、缺 usage 兜底等指标）
- Create: `internal/task/audit.go` 或复用 audit（business_video 事件：account/model/task_id/status/cost/duration；不记 prompt/产物/凭据/签名URL）
- Modify: `internal/task/workers.go`（settle 重试终态契约：最大次数+退避→耗尽落 settle_failed + 告警 + 对账队列）
- Test: `internal/obs/metrics_test.go`、settle 重试终态测试归 `internal/task/workers_test.go`

**Approach:**
- metrics 注册风格对齐 F-min Unit 7。
- audit Tier：失败/资金相关升 Tier1（对齐 business_relay D8 规则）。
- settle 永久失败有确定收敛（settle_failed 终态 + 告警 + 进对账队列），保证"账目对得上"。
- 对账：ledger correlation_id ↔ task_id 可反查；**落具体交付物**——admin-cli `reconcile export` 命令/查询，导出 `(task_id, correlation, reserve, settle, status)` 供灰度逐任务对比新旧计费（成功标准④的依据）。

**Patterns to follow:** F-min Unit 7 metrics 注册 + business_audit Tier 分级。

**Test scenarios:**
- Happy path：各指标在对应事件后正确累加；audit 记录字段齐全无敏感内容。
- Edge case：settle 重试耗尽 → settle_failed 终态 + 告警 metric。
- 安全断言：audit/metrics 标签不含 prompt/凭据/签名URL。

**Verification:** 指标/审计字段测试通过；settle 永久失败有确定终态；对账可按 task_id/correlation 反查。

- [ ] **Unit 13: 文档（业务视频 API + 迁移指南 + schema + 设计文档校正）**

**Goal:** 落对外契约文档 + schema 演化 + CONTEXT 术语 + 校正设计文档过时计费口径。

**Requirements:** 文档单元（反映 R1-R21）

**Dependencies:** Unit 1-12

**Files:**
- Create: `docs/api/video-api.md`（业务接入：POST/GET 规约、task_type=text_to_video、错误码映射、计费 token 口径、轮询/回滚建议、quickstart）
- Create: `docs/api/migration-from-direct-seedance.md`（业务从直连 seedance 迁移到网关的指南 + 状态/字段映射对照 + 双轨回滚）
- Modify: `docs/db/schema.md`（0005/0006 演化）、`CONTEXT.md`（task/异步状态机/能力描述符/Channel 凭据/entitlement 术语）
- Modify: `docs/multimedia-gateway-design.md`（§5.1 计费口径 per_video_second → token 校正说明 + 引用本计划）

**Approach:** 镜像 `docs/api/business-api.md` 结构；迁移指南含状态枚举/字段路径/错误形状对照（review 指出的真实迁移摩擦点）。

**Test scenarios:** Test expectation: none —— 纯文档单元。

**Verification:** 文档自洽 + 链接有效；CI 文档相关 guard（若有）通过。

## System-Wide Impact

- **Interaction graph:** 新增 `/v1/video/*` 提交/查询 + `/v1/callbacks/video/*` 回调入口；Asynq workers（submit/settle/fetch/recover/expire）；TOS 上传。复用 business 中间件链 + 账本 + audit/metrics。
- **Error propagation:** 前置校验失败在 reserve 前短路（无 orphan）；上游错误经 adapter sentinel → task 终态 + release；settle 永久失败 → settle_failed + 告警 + 对账队列。
- **State lifecycle risks:** reserve/task 事务边界（先 reserve 后落库失败即 release + reconciler 兜底）；崩溃恢复（submit_locked_until）；终态收敛不变量（expire 兜底）；回调/轮询双路结算幂等（CAS + ledger correlation）。
- **API surface parity:** 异步 adapter 与同步 ChatCompletion 平行；OpenAI 兼容错误形状与 F-min 一致。
- **Integration coverage:** 提交→submit worker→上游→回调/poll→settle→TOS→业务轮询 全链（mock 上游 + mock TOS + 真 PG + 真账本 + 真 Asynq/Redis）。
- **Unchanged invariants:** 账本不变量 `available+reserved+used_total=recharge_total` 不变；F-min 同步 relay 不动；business key 鉴权/pepper 不变。

## Risks & Dependencies

| Risk | Mitigation |
|------|------------|
| 火山回调字段/签名机制未知 | 实现时查官方文档 + 内网穿透实测；回调体不可信，commit 以 Poll 反查为准，回调缺失由 fetch reconciler 兜底 |
| reserve 估偏低撞 `ErrCommitExceedsReserved` | reserve 按请求**实际分辨率档** W×H/价格 + 安全系数取可证上界；settle>reserve 时 cap+告警 |
| 多副本并发超卖上游配额 | **DB 原子 claim 作权威跨副本上限**（条件 INSERT/`FOR UPDATE`，消除 TOCTOU）；Asynq 队列不作上限 |
| 上游已提交但 upstream_task_id 丢失 → 双重提交 | **上游无幂等键、不可按我方标识反查（ADR-0006 确认）→ recover fail-closed**：lease 过期不自动重投，超阈值 FAILED+release+告警；入口重复提交由我方 task_id 唯一 + 提交前 DB claim 拦截 |
| 慢上游长期占并发名额 | claim=上游并发槽，释放于上游终态；慢任务确在占用上游并发，符合 R15 本意；expire 设最长执行期上界兜底 |
| reserve 后落 task 失败 → orphan reserve | claim+task 单 tx；reconciler 显式扫 ledger active reserve 无对应 task → Release（具体 worker/SQL）；**加最小年龄阈值防误回收 in-flight reserve** |
| 丢失 settle job（上游终态未结算） | reconciler 扫"上游终态超阈值未进 SETTLED/settle_failed" → 幂等重投 settle（correlation 幂等兜底，防 reserve 永久锁死） |
| 回调 token 经日志/代理泄露 | token 不入 query、含 token URL 不入日志/span、终态置空、per-task 去抖防重复 Poll 放大 |
| 缺 usage / settle Poll 持续失败 | 落 `settle_failed` + 告警 + 对账队列；reserve 留 `reserved`（claim 不占），对账 worker 按 SLA commit 真实额或 release；不按上界 commit、不静默 release |
| 上游既无幂等键又不可按我方标识查询 | recover **fail-closed**（不自动重投 → FAILED + release + 告警）；**经 ADR-0006 官方确认成立**（非待定） |
| Asynq/Redis/TOS 新依赖 | **ADR-0006 已 Accepted**：asynq(MIT)+go-redis(BSD)+ve-tos-golang-sdk/v2(Apache-2.0)；Redis 已运行；go.mod 闸门已过 |
| 凭据泄露 | AES-GCM 加密 + write-only + 密钥类不回显片段 + KEK 轮换语义 + 明文不入日志 + 边界测试 |
| 跨租户越权 | 所有按 id 查询/引用强制 business_account_id 归属，不符 404 + 越权单测 |
| 本地无法测回调 | 轮询兜底天然覆盖；回调专项测试需用户配内网穿透域名 |

## Documentation / Operational Notes

- ADR-0006（异步基座 + 并发上限 + TOS 接入 + KEK 轮换 + 上游幂等缺失应对）**已 Accepted（2026-05-29）**，go.mod 变更闸门已过。
- 部署需 Redis 可达；回调端点须公网可达（生产）或内网穿透（本地测）。
- **灰度回滚归属**：双轨切换开关在**业务侧**（业务路由 feature flag）；网关交付 = 迁移指南 + correlation 对账导出（Unit 12）。触发回滚判据：对账差异超容差 / 网关错误率阈值 / 任务丢失；网关不可用时业务侧自动切回直连。
- **放行/验收标准**：① 账目对得上（commit 合计 = 业务实际消费）② 不丢任务（回调丢失被轮询兜底捞回）③ 失败正确 release 无 orphan ④ 新旧逐任务对账差异在容差内 ⑤ **业务体验不退化**：端到端时延（提交→可取结果）退化在约定上限内、成功率 ≥ 直连基线、失败可按 task_id/correlation 定位。
- migrations 0005（+0006）up/down 配套；生产跑前暂停 reconciler（对齐 dev-setup §5.1）。

## Sources & References

- **Origin document:** [docs/brainstorms/2026-05-28-gateway-async-video-mvp-requirements.md](../brainstorms/2026-05-28-gateway-async-video-mvp-requirements.md)
- 设计文档：[docs/multimedia-gateway-design.md](../multimedia-gateway-design.md) §9/§9ter/§9.4（计费口径以本计划 token 口径校正）
- 参考实现：`F:\AiWorkspace\storyboard-assistant\storyboard-assistant`（seedance provider / 能力描述符 / credit pricing；仅参考不照搬）
- 复用骨架：`internal/ledger`、`internal/businesskey`、`internal/httpapi/middleware`、`internal/audit`、`internal/obs`
- 项目宪法：[CLAUDE.md](../../CLAUDE.md)（reimplement-only / 新依赖开 ADR / schema up+down / 涉账本·状态机·凭据必须并发边界测试）

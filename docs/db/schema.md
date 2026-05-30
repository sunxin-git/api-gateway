# Schema 总览（当前生效版本）

> **当前 migration 版本**：0011
> **migration 文件**：`0001_init` + `0002_ledger_fields_extension` + `0003_admin_token_usage_and_circuit` + `0004_business_account_api_key` + `0005_async_video_relay` + `0006_task_status_settle_failed` + `0007_task_inflight_index_exclude_settle_failed` + `0008_task_reconciler_indexes` + `0009_task_stuck_settling_index` + `0010_oss_object_meta` + `0011_admin_console_auth`（均含 `.{up,down}.sql`）
> **PG 版本要求**：≥ 15（项目宪法 CLAUDE.md 技术栈）
> **本文档命名约定**：不带版本号，永远描述「当前生效 schema」；每次 migration 后增量加章节
>
> 本文档是 schema 的**人类可读版**，配合 .sql 文件一起 review。
> 字段、类型、约束、索引均与 migration 文件一一对应；偏差被视为 bug，必须修复其中一方。

---

## Migration 演化时间线

| 版本 | 日期 | 内容 | 计划 |
|---|---|---|---|
| 0001 | 2026-05-26 | 初始 9 张表 + 5 个枚举 + 账本不变量 CHECK + 非负 CHECK | [Phase 1 plan](../plans/2026-05-26-001-feat-phase-1-skeleton-and-migrations-plan.md) |
| 0002 | 2026-05-26 | ledger 字段扩展（delta/reference/metadata/actor_type/actor_id/canonical_body_sha256）+ balance.last_ledger_id + 不可变 trigger（UPDATE/DELETE/TRUNCATE）+ entry-level CHECK + 复合 UNIQUE 索引（idempotency_key per entry_type、correlation_id per type）+ REVOKE 高危权限 + actor_type 枚举 | [Phase 2 工作流 E plan](../plans/2026-05-26-002-feat-workflow-e-ledger-infrastructure-plan.md) |
| 0003 | 2026-05-27 | gateway_admin_token 增加 single_refund_max + daily_refund_quota_limit 两阀门 + token_hash COMMENT 修订为 HMAC-SHA-256 with pepper + 新表 gateway_admin_token_usage（每日用量累计）+ 新表 gateway_admin_token_circuit（熔断器状态） | [Phase 2 工作流 D-min plan](../plans/2026-05-27-003-feat-workflow-d-min-admin-api-plan.md) |
| 0004 | 2026-05-27 | actor_type 枚举增 business_key + 新表 business_account_api_key（业务侧对外 API Key，HMAC pepper 复用） | [Phase 2 工作流 F-min plan](../plans/2026-05-27-004-feat-workflow-f-min-openai-compat-relay-plan.md) |
| 0005 | 2026-05-29 | 异步视频中继数据层：task 加 callback_token + upstream_submitted_at；新表 account_model_concurrency（R15 并发计数行）+ business_account_model_entitlement（账户×模型授权） | [异步视频中继 MVP plan](../plans/2026-05-28-001-feat-async-video-relay-mvp-plan.md) |
| 0006 | 2026-05-29 | task_status 枚举增 SETTLE_FAILED（第 10 态，结算失败终态）；独立文件（PG 不能在同事务使用新枚举值） | 同上 |
| 0007 | 2026-05-29 | 重建 idx_task_inflight，终态排除谓词纳入 SETTLE_FAILED（须在 0006 ADD VALUE 之后的独立文件） | 同上 |
| 0008 | 2026-05-30 | task 异步 reconciler / 回调热路径 4 个部分索引（idx_task_upstream_task_id / idx_task_stuck_upstream_submitted / idx_task_submitted_no_job / idx_task_expirable） | [异步视频中继 MVP plan](../plans/2026-05-28-001-feat-async-video-relay-mvp-plan.md) |
| 0009 | 2026-05-30 | idx_task_stuck_settling 部分索引（fetch reconciler 扫 SETTLING 超时） | 同上 |
| 0010 | 2026-05-30 | 新表 oss_object_meta（成功任务 TOS 转存产物持久元数据；签名 URL 不入库）+ idx_task_settled_needing_store 部分索引 | 同上 |
| 0011 | 2026-05-31 | 管理后台会话认证：新表 operator_account（运维登录账户，bcrypt 口令）+ admin_session（PG 会话，存 token HMAC + 每会话 csrf_token） | [管理后台配置线 plan](../plans/2026-05-31-001-feat-admin-console-config-plan.md) |

---

## 顶部：偏离记录（与 v1.3 设计文档的差异）

以下记录与设计文档章节的**已知差异**，均经 plan 评审后确认：

| 表 | 设计文档章节 | 差异内容 | 处理方式 |
|---|---|---|---|
| `business_account_ledger` | §3ter.2 | ~~设计文档列出 `available_delta` / `reserved_delta` / `used_delta` / `reference_type` / `reference_id` / `metadata` / `created_by` 字段；P0 0001 schema 不含。~~ | ✅ **已在 0002 补齐**（含 `canonical_body_sha256` 额外字段供幂等 body 校验） |
| `business_account_balance` | §3ter.2 | ~~设计文档含 `last_ledger_id` 投影游标字段；P0 0001 schema 不含。~~ | ✅ **已在 0002 补齐** |
| `business_account_ledger.created_by` | §3ter.2 | 设计文档用 `created_by varchar(64)` 单一字符串；本项目改用 `actor_type` enum + `actor_id` text 两列结构化审计 | 评审决策：防伪造（document-review pass 1 finding C11） |
| `business_account` | §9bis.7 | 设计文档用 `id bigint PK + business_account_id varchar(64) UNIQUE` 双 ID；P0 schema 直接 `id text PK`（业务外部 ID）。设计文档还含 `business_account_type` / `display_name` / `admin_token_id` / `activated_at` / `suspended_at` 字段；P0 不含。 | P1+ 按需补；当前 P0 工作流 D-min 仅需开通 + 充值 + 余额查询 |
| `gateway_admin_token` | §9bis.7 | 设计文档用 `business_system_name` + `status enum` + `last_used_at`；P0 schema 改用 `description` + `revoked_at` 时间戳标记 + 不存 last_used。 | P1 完整 Admin Token 管理 UI 落地时按需扩展（计划 Unit 7 字段列表为准） |
| `webhook_subscription` | §9bis.7 | 设计文档用 `business_system_name + url + secret_cipher + events`；P0 schema 改用 `business_account_id FK + endpoint_url + hmac_secret_encrypted + key_version + event_types text[]`。 | P0 schema 字段更结构化（直接 FK 业务账户 + 数组事件类型）；P1 webhook 订阅管理 UI 时不变 |
| `task` | §9 + §9ter | 设计文档含 `reservation_ledger_id` 字段，P0 直接放入 `financial_snapshot.ReservationLedgerID`（jsonb 内）。 | P0 简化：所有快照字段统一塞 financial_snapshot；P1 工作流 F 视 query 性能决定是否提升为顶层字段 |

**仲裁优先级**（CLAUDE.md 第六节）：设计文档 v1.3 > 计划 > CONTEXT.md > 个人判断。
本 schema 整体以「计划 Unit 7 字段清单」为基线（计划是设计文档的 P0 收敛子集，已被 review 通过）。

---

## A. 枚举类型（5 个）

| 类型名 | 取值 | 关联表 |
|---|---|---|
| `ledger_entry_type` | `recharge` / `reserve` / `commit` / `release` / `refund` / `cashout` / `recharge_reversal` / `adjust` / `expire` | `business_account_ledger.entry_type` |
| `task_status` | `SUBMITTED` / `UPSTREAM_SUBMITTING` / `UPSTREAM_SUBMITTED` / `COMPLETED` / `FAILED` / `CANCELLED` / `EXPIRED` / `SETTLING` / `SETTLED` / `SETTLE_FAILED`（0006 增） | `task.status` |
| `outbox_delivery_status` | `pending` / `delivering` / `delivered` / `failed` / `dead_letter` | `webhook_event_outbox.delivery_status` |
| `fallback_policy` | `strict`（默认） / `next_rule` / `global_pool` / `legacy_distributor` | `channel_routing_rule.fallback_policy` |
| `business_account_status` | `active` / `suspended` / `frozen` / `deleted` | `business_account.status` |

> **关键术语锚点**：
> - `ledger_entry_type` 详见 [CONTEXT.md「ledger entry type」](../../CONTEXT.md)
> - `task_status` 状态转移表见 [设计文档 §9ter.2](../multimedia-gateway-design.md)
> - `fallback_policy` 详见 [CONTEXT.md「fallback_policy」](../../CONTEXT.md) + 设计文档 §8.3
> - `business_account_status` 详见 设计文档 §9bis.7

---

## B. `business_account` —— 业务账户

> 设计文档 §9bis.7 + 基线决策 #20 + CONTEXT.md「business account」

| 字段 | 类型 | NULL | 默认 | 中文语义 |
|---|---|---|---|---|
| `id` | text | NO | — | **PK**；业务系统侧的账户 ID（外部 ID 即主键，P0 简化） |
| `status` | `business_account_status` | NO | `'active'` | 账户状态枚举 |
| `isolation_required` | boolean | NO | `false` | 企业隔离硬开关；true 时禁止跨企业降级（详见 §8.3.6） |
| `break_glass_until` | timestamptz | YES | NULL | 紧急逃生门截止时间；非 NULL 且未过期 → 允许临时跨企业降级 |
| `metadata` | jsonb | NO | `'{}'` | 业务侧标签（运营备注、外部映射等） |
| `created_at` | timestamptz | NO | `NOW()` | 创建时间 |
| `updated_at` | timestamptz | NO | `NOW()` | 最后更新时间 |

**约束**：
- `pk_business_account` PRIMARY KEY (id)

**索引**：仅主键索引，无额外二级索引（P0 阶段表行数极小，预计 < 1000）。

---

## C. `business_account_balance` —— 账户余额投影

> 设计文档 §3ter.2 + CONTEXT.md「balance」「账本不变量」

**核心定位**：ledger 的**严格投影**（不是缓存）。每次 ledger 写入同事务更新 balance；
后台 reconcile job 每 5 分钟按 ledger 重算校验，**drift 即冻结账户**（不仅告警）。

| 字段 | 类型 | NULL | 默认 | 中文语义 |
|---|---|---|---|---|
| `business_account_id` | text | NO | — | **PK + FK** → business_account(id) ON DELETE RESTRICT |
| `available` | bigint | NO | 0 | 可用余额（CONTEXT.md「三态余额」） |
| `reserved` | bigint | NO | 0 | 预占余额（inflight 任务持有） |
| `used_total` | bigint | NO | 0 | 累计已结算 |
| `recharge_total` | bigint | NO | 0 | 累计充值（单调递增，含初始 + 后续） |
| `refund_total` | bigint | NO | 0 | 累计退款（**审计字段**，不进入账本不变量） |
| `version` | bigint | NO | 0 | 乐观锁版本；CAS 更新时 `SET version = version + 1` |
| `frozen` | boolean | NO | false | drift 检测命中或运营手工冻结 |
| `frozen_reason` | text | YES | NULL | 冻结原因（如 `ledger_drift_detected: expected=... actual=...`） |
| `frozen_at` | timestamptz | YES | NULL | 冻结时间 |
| `updated_at` | timestamptz | NO | `NOW()` | 最后更新时间 |

**约束**：
- `pk_business_account_balance` PRIMARY KEY (business_account_id)
- `fk_business_account_balance_account` FK → business_account(id) ON DELETE RESTRICT
- `chk_business_account_balance_non_negative` CHECK (available >= 0 AND reserved >= 0 AND used_total >= 0 AND recharge_total >= 0 AND refund_total >= 0)
- **`chk_business_account_balance_invariant` CHECK (available + reserved + used_total = recharge_total) —— 账本不变量硬保障**

> **关键不变量**（CONTEXT.md「账本不变量」 + 设计文档 §3ter.2 + v1.2.1 数学校准）：
> ```
> available + reserved + used_total = recharge_total
> ```
> `refund_total` **不**进入不变量，因为 refund 语义是「used 退回 available」内部转移，
> 不改变账户累计入账。`refund_total` 仅作审计字段累计退回总额。

**索引**：仅主键索引。

---

## D. `business_account_ledger` —— 账本不可变流水

> 设计文档 §3ter.2 + CONTEXT.md「ledger」

**核心定位**：所有扣减 / 退款 / 对账的**唯一真相源**。永不 UPDATE / DELETE。

| 字段 | 类型 | NULL | 默认 | 中文语义 |
|---|---|---|---|---|
| `id` | bigserial | NO | — | **PK**，自增 |
| `business_account_id` | text | NO | — | FK → business_account(id) ON DELETE RESTRICT |
| `entry_type` | `ledger_entry_type` | NO | — | 流水类型枚举 |
| `amount` | bigint | NO | — | 金额（正负方向取决于 entry_type 语义） |
| `correlation_id` | text | NO | — | 关联同一笔业务流的多个 entry（如 reserve → commit / release） |
| `idempotency_key` | text | YES | NULL | 充值幂等键 sha256(external_ref + canonical_body)；NULL 表示非充值 |
| `snapshot` | jsonb | NO | `'{}'` | BillingSnapshot（表达式 + 哈希 + group ratio + cost catalog refs + 汇率 + usage） |
| `created_at` | timestamptz | NO | `NOW()` | 创建时间 |

**约束**：
- `pk_business_account_ledger` PRIMARY KEY (id)
- `fk_business_account_ledger_account` FK → business_account(id) ON DELETE RESTRICT

**索引**：
- `idx_ledger_account_created` (business_account_id, created_at DESC) —— 账户流水查询
- `idx_ledger_correlation` (correlation_id) —— reserve/commit/release 配对反查
- `uq_ledger_idempotency_key` (idempotency_key) **WHERE idempotency_key IS NOT NULL** —— 充值幂等（部分唯一索引）

---

## E. `channel` —— 渠道（上游 provider 凭据抽象）

> 设计文档 §8.2 + CONTEXT.md「channel」「ChannelCredentials」

| 字段 | 类型 | NULL | 默认 | 中文语义 |
|---|---|---|---|---|
| `id` | bigserial | NO | — | **PK**，自增 |
| `name` | text | NO | — | 运营可见名称（如「火山-真人-biz_001」） |
| `provider_type` | text | NO | — | provider 类型（`volc_seedance_v2` / `openai` / ...） |
| `enabled` | boolean | NO | true | 是否启用 |
| `restricted_business_accounts` | text[] | NO | `'{}'` | 业务账户白名单；空 = 不限制，非空 = 仅列内账户可用 |
| `channel_purpose` | text | YES | NULL | 标签，如 `seedance:realperson:biz_001`，人类可读 |
| `credentials_encrypted` | bytea | NO | — | envelope encryption 密文（P0 单一 AES-GCM；P1 接 KEK/DEK 分层） |
| `key_version` | int | NO | 1 | KEK 版本号，支持平滑轮换 |
| `other_settings` | jsonb | NO | `'{}'` | 限流 / 超时 / 模型映射等结构化设置 |
| `created_at` | timestamptz | NO | `NOW()` | 创建时间 |
| `updated_at` | timestamptz | NO | `NOW()` | 最后更新时间 |

**约束**：
- `pk_channel` PRIMARY KEY (id)
- `uq_channel_name` UNIQUE (name)
- `chk_channel_key_version_positive` CHECK (key_version >= 1)

**索引**：
- `idx_channel_restricted_accounts` GIN (restricted_business_accounts) —— distributor 过滤热路径
- `idx_channel_enabled` (enabled) **WHERE enabled = true** —— 启用 channel 列表（部分索引）

---

## F. `channel_routing_rule` —— 路由规则

> 设计文档 §8.2 + §8.3 + CONTEXT.md「channel_routing_rule」「fallback_policy」

| 字段 | 类型 | NULL | 默认 | 中文语义 |
|---|---|---|---|---|
| `id` | bigserial | NO | — | **PK**，自增 |
| `business_account_id` | text | YES | NULL | FK → business_account(id) ON DELETE CASCADE；NULL = 全局默认规则 |
| `priority` | int | NO | 100 | 规则求值优先级，**数值大优先** |
| `condition_expr` | text | NO | — | expr-lang/expr 表达式（如 `param("is_real_person") == true`） |
| `target_channel_ids` | bigint[] | NO | — | 候选 channel ID 列表 |
| `fallback_policy` | `fallback_policy` | NO | `'strict'` | 降级策略（R11 默认 strict） |
| `enabled` | boolean | NO | true | 是否启用 |
| `created_at` | timestamptz | NO | `NOW()` | 创建时间 |
| `updated_at` | timestamptz | NO | `NOW()` | 最后更新时间 |

**约束**：
- `pk_channel_routing_rule` PRIMARY KEY (id)
- `fk_channel_routing_rule_account` FK → business_account(id) ON DELETE CASCADE
- `chk_channel_routing_rule_targets_non_empty` CHECK (array_length(target_channel_ids, 1) >= 1)

**索引**：
- `idx_channel_routing_rule_account_priority` (business_account_id, priority DESC) —— 路由查询热路径
- `idx_channel_routing_rule_enabled` (enabled) **WHERE enabled = true** —— 启用规则列表

> **关键约束**：`isolation_required = true` 的业务账户，其规则的 `fallback_policy` **不允许**取 `global_pool` 或 `legacy_distributor`（运行时强校验，schema 层不约束以保留 break-glass 路径）。

---

## G. `webhook_event_outbox` —— 事件出箱

> 设计文档 §9bis.4.1（v1.2 强化主库约束 + v1.2.1 claim/lease 字段）+ CONTEXT.md「outbox」「claim/lease」

**核心定位**：**必须**部署在主库（与 ledger 同库），保证事件与账本同事务发布。

| 字段 | 类型 | NULL | 默认 | 中文语义 |
|---|---|---|---|---|
| `event_id` | bigserial | NO | — | **PK**，**单调递增**；业务系统拉取游标 |
| `business_account_id` | text | YES | NULL | FK → business_account(id) ON DELETE SET NULL；NULL = 全局事件 |
| `event_type` | text | NO | — | 事件类型（`account.created` / `account.recharged` / ...） |
| `payload` | jsonb | NO | — | 事件 payload，结构由 event_type 决定 |
| `is_financial` | boolean | NO | false | 财务事件标志（true → 保留 ≥ 1 年；false → ≥ 30 天） |
| `retention_until` | timestamptz | NO | — | 事件保留截止时间 |
| `delivery_status` | `outbox_delivery_status` | NO | `'pending'` | 投递状态 |
| `delivery_attempts` | int | NO | 0 | 重试次数（≥ 10 转 dead_letter） |
| `locked_by` | text | YES | NULL | claim/lease：当前 worker ID（hostname_pid） |
| `locked_until` | timestamptz | YES | NULL | lease 截止；NOW() > locked_until 时其他 worker 可抢占 |
| `delivery_idempotency_key` | text | NO | — | 业务侧去重键，**全局唯一**；默认 = event_id 字符串 |
| `last_pushed_at` | timestamptz | YES | NULL | 最近一次推送时间 |
| `created_at` | timestamptz | NO | `NOW()` | 创建时间 |

**约束**：
- `pk_webhook_event_outbox` PRIMARY KEY (event_id)
- `fk_webhook_event_outbox_account` FK → business_account(id) ON DELETE SET NULL
- `uq_webhook_event_outbox_idempotency` UNIQUE (delivery_idempotency_key)
- `chk_webhook_event_outbox_attempts_non_negative` CHECK (delivery_attempts >= 0)

**索引**：
- `idx_webhook_event_outbox_pending_event_id` (delivery_status, event_id) **WHERE delivery_status IN ('pending', 'delivering')** —— dispatcher 扫描热路径（部分索引）
- `idx_webhook_event_outbox_retention` (retention_until) —— 保留期清理 job

---

## H. `gateway_admin_token` —— 管理 Token

> 设计文档 §9bis.6 + 基线决策 #16 + CONTEXT.md「scope」「IP allowlist」「阀门」

| 字段 | 类型 | NULL | 默认 | 中文语义 |
|---|---|---|---|---|
| `id` | bigserial | NO | — | **PK**，自增 |
| `token_hash` | text | NO | — | bcrypt/argon2id 哈希；**UNIQUE**；明文绝不入库 |
| `description` | text | NO | — | Token 用途描述（运营标签） |
| `scopes` | text[] | NO | `'{}'` | 细粒度权限范围（CONTEXT.md「scope」） |
| `ip_allowlist` | cidr[] | NO | `'{}'` | CIDR 源 IP 白名单 |
| `daily_recharge_quota_limit` | bigint | YES | NULL | 单日充值上限阀门；NULL = 无限 |
| `daily_account_create_limit` | int | YES | NULL | 单日创建账户上限阀门；NULL = 无限 |
| `single_recharge_max` | bigint | YES | NULL | 单笔充值最大值；NULL = 无限 |
| `requests_per_minute` | int | YES | NULL | QPS 阀门；NULL = 无限 |
| `circuit_breaker_enabled` | boolean | NO | false | 是否启用自动熔断 |
| `created_by` | text | NO | — | 创建者标识（管理员账号） |
| `created_at` | timestamptz | NO | `NOW()` | 创建时间 |
| `expires_at` | timestamptz | YES | NULL | 过期时间；NULL = 永不过期 |
| `revoked_at` | timestamptz | YES | NULL | 吊销时间；NULL = 未吊销 |

**约束**：
- `pk_gateway_admin_token` PRIMARY KEY (id)
- `uq_gateway_admin_token_hash` UNIQUE (token_hash)
- 4 个阀门字段 `CHECK (... IS NULL OR ... >= 0)` 非负

**索引**：
- `idx_gateway_admin_token_active` (revoked_at) **WHERE revoked_at IS NULL** —— 鉴权热路径（仅活跃 Token）

---

## I. `webhook_subscription` —— Webhook 订阅

> 设计文档 §9bis + CONTEXT.md「outbox」相关

| 字段 | 类型 | NULL | 默认 | 中文语义 |
|---|---|---|---|---|
| `id` | bigserial | NO | — | **PK**，自增 |
| `business_account_id` | text | NO | — | FK → business_account(id) ON DELETE CASCADE |
| `endpoint_url` | text | NO | — | 回调 URL |
| `hmac_secret_encrypted` | bytea | NO | — | HMAC 签名密钥密文（envelope encryption） |
| `key_version` | int | NO | 1 | KEK 版本号 |
| `event_types` | text[] | NO | `'{}'` | 订阅的事件类型；空数组 = 订阅全部 |
| `enabled` | boolean | NO | true | 是否启用 |
| `created_at` | timestamptz | NO | `NOW()` | 创建时间 |
| `updated_at` | timestamptz | NO | `NOW()` | 最后更新时间 |

**约束**：
- `pk_webhook_subscription` PRIMARY KEY (id)
- `fk_webhook_subscription_account` FK → business_account(id) ON DELETE CASCADE
- `chk_webhook_subscription_key_version_positive` CHECK (key_version >= 1)

**索引**：
- `idx_webhook_subscription_account_enabled` (business_account_id, enabled) —— 按账户取订阅

---

## J. `task` —— 异步任务

> 设计文档 §9 + §9ter + v1.2.2 / v1.2.3 修正 + CONTEXT.md「task」「UPSTREAM_SUBMITTING」

| 字段 | 类型 | NULL | 默认 | 中文语义 |
|---|---|---|---|---|
| `id` | text | NO | — | **PK**；应用层生成（雪花 / ulid） |
| `business_account_id` | text | NO | — | FK → business_account(id) ON DELETE RESTRICT |
| `token_id` | bigint | YES | NULL | Token ID（业务关联） |
| `channel_id` | bigint | YES | NULL | FK → channel(id) ON DELETE SET NULL |
| `provider_type` | text | NO | — | provider 类型 |
| `model` | text | NO | — | 目标模型 |
| `status` | `task_status` | NO | `'SUBMITTED'` | 任务状态机；状态转移仅允许 §9ter.2 状态转移表方向（CAS） |
| `upstream_task_id` | text | YES | NULL | 上游任务 ID（UPSTREAM_SUBMITTED 后写入） |
| `upstream_submitted_at` | timestamptz | YES | NULL | 进入 UPSTREAM_SUBMITTED 的时刻（0005）；fetch reconciler 判上游超时 |
| `submit_locked_until` | timestamptz | YES | NULL | v1.2.3：worker 抢占 lease 截止；超时 cron 回退到 SUBMITTED |
| `submit_locked_by` | text | YES | NULL | 抢占 worker ID |
| `submit_recover_count` | int | NO | 0 | v1.2.3：UPSTREAM_SUBMITTING 崩溃恢复次数；≥ 3 转 FAILED |
| `financial_snapshot` | jsonb | NO | `'{}'` | TaskFinancialSnapshot（AuthSnapshot + PricingSnapshot + ReservationLedgerID） |
| `accounting_month` | text | NO | — | 跨月归属 YYYY-MM；提交时刻冻结，永不切分 |
| `submitted_at` | timestamptz | NO | `NOW()` | 提交时间 |
| `terminal_at` | timestamptz | YES | NULL | 终态时间（COMPLETED / FAILED / ...） |
| `error_code` | text | YES | NULL | 错误码 |
| `error_message` | text | YES | NULL | 错误信息 |
| `callback_token` | text | YES | NULL | 回调 per-task token（0005）；进终态后置空；绝不入日志 / 不放 query string |
| `updated_at` | timestamptz | NO | `NOW()` | 最后更新时间 |

**约束**：
- `pk_task` PRIMARY KEY (id)
- `fk_task_account` FK → business_account(id) ON DELETE RESTRICT
- `fk_task_channel` FK → channel(id) ON DELETE SET NULL
- `chk_task_submit_recover_count_non_negative` CHECK (submit_recover_count >= 0)
- `chk_task_accounting_month_format` CHECK (accounting_month ~ '^[0-9]{4}-(0[1-9]|1[0-2])$')

**索引**（8 个；0001 建 4 + 0008 补 4 个异步 reconciler/回调热路径）：
- `idx_task_inflight` (business_account_id, status) **WHERE status NOT IN ('COMPLETED', 'FAILED', 'CANCELLED', 'EXPIRED', 'SETTLED', 'SETTLE_FAILED')` —— inflight 查询（部分索引；0007 把 SETTLE_FAILED 纳入终态排除）
- `idx_task_submit_recover` (status, submit_locked_until) **WHERE status = 'UPSTREAM_SUBMITTING'** —— 崩溃恢复 cron 扫描
- `idx_task_accounting_month` (accounting_month, status) —— 月结查询
- `idx_task_channel_id` (channel_id) **WHERE channel_id IS NOT NULL** —— 按渠道反查

---

## 关键不变量速查（PR review checklist）

1. **账本不变量（CHECK 约束硬保障）**：
   `business_account_balance.available + reserved + used_total = recharge_total`

2. **状态机 CAS（应用层硬保障）**：
   `task.status` 只能按 §9ter.2 状态转移表方向变更；禁止 `UPDATE task SET status = ?` 不带 `WHERE status = <from>` 条件

3. **outbox 与 ledger 同事务（应用层硬保障）**：
   所有 `INSERT INTO business_account_ledger` 必须与 `INSERT INTO webhook_event_outbox` 在同一 `*sql.Tx` 内提交

4. **isolation_required 强校验（应用层 + 文档约束）**：
   `business_account.isolation_required = true` 时，关联的 `channel_routing_rule.fallback_policy` 不允许取 `global_pool` / `legacy_distributor`

5. **凭据密文绝不入库明文**：
   `channel.credentials_encrypted` / `webhook_subscription.hmac_secret_encrypted` 仅存 envelope encryption 密文

---

## migration 操作纪律

- **永远**通过 golang-migrate 跑 migrations，不允许在线手改 DB schema
- **永远**配套写 `.up.sql` 和 `.down.sql`，down.sql 必须能完整撤销 up.sql
- **永远**新增 schema 变更走新文件 `0003_xxx.up.sql` 等，**不**修改已合并的旧 migration
- SQL 变更 PR 必须附 `EXPLAIN ANALYZE` 输出（CLAUDE.md 第六节）
- **生产环境禁止 down 已有数据的 ledger 表**：0002 down.sql 首行 DO 块自动拦截

---

## 0002 字段增量（账本基础设施落地）

> 计划：[docs/plans/2026-05-26-002-feat-workflow-e-ledger-infrastructure-plan.md](../plans/2026-05-26-002-feat-workflow-e-ledger-infrastructure-plan.md)
> 评审：document-review pass 1 / pass 2 共识

### 新增枚举

| 类型 | 取值 | 用途 |
|---|---|---|
| `actor_type` | `admin_token` / `cli` / `system` / `task` | 结构化审计两列（与 `actor_id` 配对），防 created_by 自由字符串伪造 |

### `business_account_ledger` 新增字段

| 字段 | 类型 | 说明 |
|---|---|---|
| `available_delta` | bigint NOT NULL DEFAULT 0 | 此 entry 对 available 的影响（reconciler SUM 用） |
| `reserved_delta` | bigint NOT NULL DEFAULT 0 | 此 entry 对 reserved 的影响 |
| `used_delta` | bigint NOT NULL DEFAULT 0 | 此 entry 对 used_total 的影响 |
| `reference_type` | text NULL | 关联实体类型：task / topup_order / manual_adjust / monthly_settle |
| `reference_id` | text NULL | 关联实体 ID（如 task_id） |
| `metadata` | jsonb NOT NULL DEFAULT '{}' | 业务侧附加标签（不含 PII 原值） |
| `actor_type` | actor_type NOT NULL DEFAULT 'system' | 操作来源（结构化审计） |
| `actor_id` | text NOT NULL DEFAULT 'bootstrap' | 操作者 ID |
| `canonical_body_sha256` | bytea NULL | 充值幂等命中时比对的 canonical body sha256；防沉默篡改 |

### `business_account_balance` 新增字段

| 字段 | 类型 | 说明 |
|---|---|---|
| `last_ledger_id` | bigint NOT NULL DEFAULT 0 | 已聚合到的最大 ledger.id（投影游标 + rebuild CAS 锚点） |

### 新增约束

- **`chk_ledger_canonical_body_hash_len`**：`canonical_body_sha256` 必须为 32 字节（sha256 长度）
- **`chk_ledger_delta_by_type`**：按 `entry_type` CASE 校验 `available_delta` / `reserved_delta` / `used_delta` 组合守恒（recharge / reserve / commit / release / refund 五种 entry_type 各自约束）

### 新增索引

- **`uq_ledger_idempotency_key_per_type`** UNIQUE `(entry_type, idempotency_key) WHERE idempotency_key IS NOT NULL` — 替代 0001 的全表 UNIQUE，让不同 entry_type 共用 key 字符串不撞索引
- **`uq_ledger_correlation_per_type`** UNIQUE `(business_account_id, correlation_id, entry_type) WHERE correlation_id <> ''` — Reserve/Commit/Release/Refund 重试幂等
- **`idx_ledger_reference`** `(reference_type, reference_id) WHERE reference_type IS NOT NULL` — 按业务实体反查

### 新增 trigger

- **`ledger_prevent_update_or_delete`** BEFORE UPDATE OR DELETE FOR EACH ROW — 阻止 ledger 修改 / 删除（应用层 + DB 双兜底）
- **`ledger_prevent_truncate`** BEFORE TRUNCATE FOR EACH STATEMENT — 阻止 TRUNCATE（行 trigger 不覆盖此操作）

### 权限调整

- `REVOKE TRUNCATE, DELETE, UPDATE ON business_account_ledger FROM PUBLIC` — 双兜底，即便单一 connection 用户也无法跑高危操作（P1 进一步做 DB role 分级）

---

## 0003 演化（2026-05-27）

> 计划：[Phase 2 工作流 D-min plan](../plans/2026-05-27-003-feat-workflow-d-min-admin-api-plan.md) Unit 1

### `gateway_admin_token` 新增字段

| 字段 | 类型 | 说明 |
|---|---|---|
| `single_refund_max` | bigint NULL | 单笔退款金额上限（minor unit）；NULL = 无限制；document-review 添加（防 leaked refund-scope token 一次清空 used_total） |
| `daily_refund_quota_limit` | bigint NULL | 当日累计退款金额上限（minor unit, UTC day）；NULL = 无限制 |

### `gateway_admin_token` COMMENT 修订

- **`token_hash`**：Phase 1 注释 "bcrypt / argon2id" 是笔误；本次修订为 **`HMAC-SHA-256(GATEWAY_TOKEN_PEPPER, token_plaintext) 的 hex 字符串（64 char）`**。决策依据见 [D-min plan §决策 D1](../plans/2026-05-27-003-feat-workflow-d-min-admin-api-plan.md)：随机 token + HMAC pepper 是业界标准（GitHub PAT / Stripe API Key），防 DB 全量泄露离线穷举。

### 新表：`gateway_admin_token_usage`（每日累计用量）

| 字段 | 类型 | 说明 |
|---|---|---|
| `token_id` | bigint NOT NULL FK | 关联 gateway_admin_token.id；ON DELETE CASCADE |
| `day` | date NOT NULL | UTC 当日（决策 D9：多时区业务系统对齐） |
| `recharge_total_minor` | bigint NOT NULL DEFAULT 0 | 当日累计成功充值金额；仅 LedgerService outcome=FreshlyWritten 后累加 |
| `refund_total_minor` | bigint NOT NULL DEFAULT 0 | 当日累计成功退款金额；同上语义 |
| `account_create_count` | int NOT NULL DEFAULT 0 | 当日累计成功创建账户次数 |
| `updated_at` | timestamptz NOT NULL DEFAULT NOW() | 最近更新时刻 |

- **主键**：`(token_id, day)` 复合
- **外键**：`token_id → gateway_admin_token(id) ON DELETE CASCADE`
- **CHECK**：`recharge_total_minor >= 0 AND refund_total_minor >= 0 AND account_create_count >= 0`
- **索引**：`idx_gateway_admin_token_usage_day(day)` 用于日常清理 job

**用途**：阀门 `daily_recharge_quota_limit` / `daily_refund_quota_limit` / `daily_account_create_limit` 的真相源；用 ON CONFLICT DO UPDATE UPSERT 累加（行锁串行化保证并发原子性）。

### 新表：`gateway_admin_token_circuit`（熔断器状态）

| 字段 | 类型 | 说明 |
|---|---|---|
| `token_id` | bigint NOT NULL PK FK | 关联 gateway_admin_token.id；1:0..1 关系 |
| `window_started_at` | timestamptz NOT NULL DEFAULT NOW() | 1h 滚动窗口起点；> 1h 时下次 RecordCircuitError 重置 |
| `error_count` | int NOT NULL DEFAULT 0 | 当前窗口内累计 4xx/5xx 数；超 100 触发 TripCircuitBreaker |
| `breaker_tripped_until` | timestamptz NULL | 跳闸截止时间；NULL 或过去 = 未熔断；未来 = 熔断中 |
| `updated_at` | timestamptz NOT NULL DEFAULT NOW() | 最近更新时刻 |

- **主键**：`token_id` 单列（1:0..1 with `gateway_admin_token`）
- **外键**：`token_id → gateway_admin_token(id) ON DELETE CASCADE`
- **CHECK**：`error_count >= 0`

**用途**：阀门 `circuit_breaker_enabled = true` 时，UPSERT 单语句完成窗口滚动 + 错误累加（CASE WHEN 在 DO UPDATE SET 子句内实现 1h 重置）。手工解锁路径：`ResetCircuitBreaker` query 或 SQL `UPDATE ... SET breaker_tripped_until = NULL, error_count = 0, window_started_at = NOW()`。

### sqlc 生成代码

`internal/db/admin_token.sql.go` 11 个函数 + 多个 Row struct：

- `InsertAdminToken` / `FindActiveAdminTokenByHash` / `FindAdminTokenByID` / `RevokeAdminToken` / `ListActiveAdminTokens`
- `IncrementTokenUsage` / `GetTokenUsage`
- `RecordCircuitError` / `TripCircuitBreaker` / `GetCircuitState` / `ResetCircuitBreaker`

类型映射（与 sqlc.yaml override 一致）：

- `ip_allowlist cidr[]` → `[]netip.Prefix`（Go 1.18+ 标准库）
- nullable `bigint` → `pgtype.Int8`
- nullable `int` → `pgtype.Int4`
- nullable `timestamptz` → `sql.NullTime`

---

## 0004 演化（2026-05-27）

> 计划：[Phase 2 工作流 F-min plan](../plans/2026-05-27-004-feat-workflow-f-min-openai-compat-relay-plan.md) Unit 1

### `actor_type` 枚举扩展

新增枚举值 `business_key`（与 admin_token / cli / system / task 平级）：

```sql
ALTER TYPE actor_type ADD VALUE IF NOT EXISTS 'business_key';
```

**用途**：业务系统通过 `/v1/chat/completions` 走 relay 写 ledger entry 时，actor 标识为 `business_key:{api_key_id}`，与 `admin_token:{token_id}` 同 pattern 便于审计反查。

**Plan §Unit 7 决策点提前**：原 plan 把此枚举扩展放 Unit 7 装配阶段；提前到 Unit 1 避免 Unit 2-7 期间 Go 常量 `ActorTypeBusinessKey` 已存在但 enum 未加导致运行时 INSERT 失败的半截状态。

**PG 限制**：`DROP VALUE` 不被支持；0004.down.sql 仅删表，actor_type `business_key` 枚举值保留（无害；彻底回滚 F-min 时按 down.sql 注释中 SOP 手工 CREATE TYPE _new + ALTER TABLE + DROP 老 TYPE）。

### 新表：`business_account_api_key`（业务系统对外 API Key）

业务系统通过 Bearer biz-key 调网关 `/v1/chat/completions`；与 admin token 共享 HMAC pepper（F-min 决策 D4：复用 `GATEWAY_TOKEN_PEPPER`，少一个运维负担）。

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | bigserial PK | 自增 |
| `business_account_id` | text NOT NULL FK | → business_account(id) ON DELETE CASCADE（账户删除时 key 一并失效）|
| `description` | text NOT NULL | 运营标签，如 "creator-platform-prod-key-1" |
| `key_hash` | text NOT NULL UNIQUE | HMAC-SHA-256(GATEWAY_TOKEN_PEPPER, plaintext) hex (64 char)；与 admin token 同 pepper 同算法 |
| `requests_per_minute` | int NULL | RPM 上限；NULL = 不限速；按 key.id 维度计数（InProcessRPM）|
| `created_by` | text NOT NULL | MVP admin-cli 硬编码 "cli:bootstrap" |
| `created_at` | timestamptz NOT NULL DEFAULT NOW() | |
| `revoked_at` | timestamptz NULL | revoke 时写入；查询用 WHERE revoked_at IS NULL 过滤 |
| `last_used_at` | timestamptz NULL | 鉴权命中时 best-effort 异步更新（5min 批量 flush）；运维查"长期未用 key"|
| `updated_at` | timestamptz NOT NULL DEFAULT NOW() | |

- **UNIQUE** `(key_hash)` —— 鉴权热路径单 row lookup（O(log n) btree）
- **INDEX** `idx_business_account_api_key_account_active(business_account_id) WHERE revoked_at IS NULL` —— 运维查"账户 X 有几个 key 在用"
- **FK CASCADE** `business_account_id → business_account(id) ON DELETE CASCADE` —— 删账户时 key 一并失效（防"账户没了但 key 还能 auth"）
- **CHECK** `requests_per_minute IS NULL OR requests_per_minute > 0` —— RPM 非负兜底（admin-cli 入参校验已保证）

### sqlc 生成代码

`internal/db/business_account_api_key.sql.go` 7 个函数：

- `InsertBusinessKey` / `FindActiveBusinessKeyByHash`（鉴权热路径）/ `FindBusinessKeyByID`（运维 / audit）/ `RevokeBusinessKey`（COALESCE 保留首次 timestamp）
- `ListActiveBusinessKeysByAccount` / `ListAllActiveBusinessKeys`（**不**返 key_hash）
- `TouchBusinessKeyLastUsed`（异步 best-effort 鉴权命中时间戳；5min 批量 flush）

---

## 0005 演化（2026-05-29）

> 计划：[异步视频中继 MVP plan](../plans/2026-05-28-001-feat-async-video-relay-mvp-plan.md) Unit 2；决策 [ADR-0006](../adr/0006-async-execution-asynq-redis.md)

### `task` 新增列

| 字段 | 类型 | 说明 |
|---|---|---|
| `callback_token` | text NULL | 回调 per-task token（Unit 8 生成）；进终态后置空；绝不入日志 / 不放 query string |
| `upstream_submitted_at` | timestamptz NULL | 进入 UPSTREAM_SUBMITTED 的时刻；供 fetch reconciler 判上游超时未终态 |

### 新表：`account_model_concurrency`（R15 并发硬上限权威计数行）

每 (account, model) 一行 `inflight` 计数；**claim = 上游并发槽**（只数 SUBMITTED/UPSTREAM_SUBMITTING/UPSTREAM_SUBMITTED 三态）。提交前原子占位 `UPDATE ... SET inflight=inflight+1 WHERE ... AND inflight<@cap RETURNING inflight`（影响 0 行 = 占不到 = 429）；进上游终态 CAS 同事务释放。详见 ADR-0006 决策 2。

| 字段 | 类型 | 说明 |
|---|---|---|
| `business_account_id` | text NOT NULL | 复合 PK + FK → business_account(id) ON DELETE CASCADE |
| `model` | text NOT NULL | 复合 PK；gateway 可见 model 名 |
| `inflight` | int NOT NULL DEFAULT 0 | 在途任务数；占位 +1 / 上游终态 -1 |
| `updated_at` | timestamptz NOT NULL DEFAULT NOW() | |

- **PK** `(business_account_id, model)`
- **FK CASCADE** `business_account_id → business_account(id)`
- **CHECK** `inflight >= 0`（防 double-release 为负；超上限由查询 `inflight < @cap` 保证，**cap 不入表**，由 Go 侧解析为查询参数）

### 新表：`business_account_model_entitlement`（账户×模型授权）

行存在 = 已授权；revoke = 删行；check = 存在性查询（计划 Unit 10）。

| 字段 | 类型 | 说明 |
|---|---|---|
| `business_account_id` | text NOT NULL | 复合 PK + FK → business_account(id) ON DELETE CASCADE |
| `gateway_model` | text NOT NULL | 复合 PK；gateway 可见 model 名 |
| `created_at` | timestamptz NOT NULL DEFAULT NOW() | |
| `updated_at` | timestamptz NOT NULL DEFAULT NOW() | grant 幂等时刷新 |

- **PK** `(business_account_id, gateway_model)`（天然唯一）
- **FK CASCADE** `business_account_id → business_account(id)`

### sqlc 生成代码

- `internal/db/channel.sql.go`：InsertChannel / GetChannelByID / ListActiveChannels / ListActiveChannelsByProvider / UpdateChannelCredentials / SetChannelEnabled / DeleteChannel
- `internal/db/task.sql.go`：InsertTask / GetTaskByID / GetTaskForAccount（归属校验）/ GetTaskByUpstreamTaskID / CompareAndSwapTaskStatus / MarkTaskSubmitting / MarkTaskUpstreamSubmitted / MarkTaskUpstreamTerminal / CountInflightByAccountModel / ScanRecoverableTasks / ScanStuckUpstreamSubmitted / ScanSubmittedNoJob / ScanExpirableTasks / ClaimConcurrencySlot / ReleaseConcurrencySlot / GetConcurrency
- `internal/db/entitlement.sql.go`：GrantEntitlement（幂等）/ RevokeEntitlement / CheckEntitlement / ListEntitlementsByAccount

---

## 0006 演化（2026-05-29）

> 计划：同上 Unit 2；决策 ADR-0006 决策 5

### `task_status` 枚举扩展

新增第 10 态 `SETTLE_FAILED`（沿用既有全大写约定）：

```sql
ALTER TYPE task_status ADD VALUE IF NOT EXISTS 'SETTLE_FAILED';
```

**用途**：上游已终态但结算失败（缺 usage / Poll 持续失败 / settle 重试耗尽）→ 落此终态 + 告警 + 进对账队列（不按 reserve 上界 commit、不静默 release）。**不持并发 claim**（claim 在进上游终态时已释放）。

**PG 限制**：`ADD VALUE` 不能在同事务被使用（migrate 每文件 1 事务）；故独立成 0006，索引重建（使用该值）放 0007。`DROP VALUE` 不被支持，down 保留枚举值（无害）。

---

## 0007 演化（2026-05-29）

> 计划：同上 Unit 2

### 重建 `idx_task_inflight`

终态排除谓词纳入 `SETTLE_FAILED`（须在 0006 ADD VALUE 之后的独立文件，否则同事务使用新枚举值报错）：

```sql
DROP INDEX IF EXISTS idx_task_inflight;
CREATE INDEX idx_task_inflight ON task (business_account_id, status)
    WHERE status NOT IN ('COMPLETED','FAILED','CANCELLED','EXPIRED','SETTLED','SETTLE_FAILED');
```

该索引仅供 reconciler 扫卡住任务 / metrics 聚合，**非** R15 cap 计数（cap 走 `account_model_concurrency` 原子 claim）。down 还原为 0001 的 5 终态排除谓词。

---

## 0008 演化（2026-05-30）

> 计划：[异步视频中继 MVP plan](../plans/2026-05-28-001-feat-async-video-relay-mvp-plan.md) Unit 2/6（ce-review 补索引）

`task` 新增 4 个部分索引，覆盖回调反查与各 reconciler scan（避免任务表增长后全表扫描）：

| 索引 | 谓词 | 用途 |
|---|---|---|
| `idx_task_upstream_task_id` | `WHERE upstream_task_id IS NOT NULL` | 回调入口 + fetch reconciler 按上游 task_id 反查 |
| `idx_task_stuck_upstream_submitted` | `WHERE status='UPSTREAM_SUBMITTED'` | fetch reconciler 扫上游超时 |
| `idx_task_submitted_no_job` | `WHERE status='SUBMITTED'` | reconciler 扫入队丢失滞留 |
| `idx_task_expirable` | `WHERE status IN (SUBMITTED,UPSTREAM_SUBMITTING,UPSTREAM_SUBMITTED)` | expire worker 扫三态超最长执行期 |

---

## 0009 演化（2026-05-30）

> 计划：同上 Unit 6b

`task` 新增 1 个部分索引：

| 索引 | 谓词 | 用途 |
|---|---|---|
| `idx_task_stuck_settling` | `WHERE status='SETTLING'`（按 updated_at） | fetch reconciler 扫硬崩溃于结算落账后、终态 CAS 前的卡住任务 |

---

## 0010 演化（2026-05-30）

> 计划：[异步视频中继 MVP plan](../plans/2026-05-28-001-feat-async-video-relay-mvp-plan.md) Unit 9；决策 [ADR-0006](../adr/0006-async-execution-asynq-redis.md) 决策 3

### 新表：`oss_object_meta`（TOS 转存产物持久元数据）

每个成功任务转存到企业 TOS 的产物对象的持久元数据；**签名 URL 读时现签不入库**，**不存上游源 URL**。

| 字段 | 类型 | 说明 |
|---|---|---|
| `task_id` | text NOT NULL | **PK** + FK → task(id) ON DELETE CASCADE；一任务一对象，天然幂等 |
| `business_account_id` | text NOT NULL | denormalized 归属（便于按账户清理 / 归属查询） |
| `bucket` / `object_key` / `region` / `endpoint` | text NOT NULL | 转存时绑定的 channel TOS 配置快照；`object_key` 含不可枚举随机段 + project_id 隔离前缀 |
| `content_type` | text NOT NULL DEFAULT '' | 对象内容类型 |
| `size_bytes` | bigint NOT NULL DEFAULT 0 | 对象大小（CHECK ≥ 0） |
| `stored_at` | timestamptz NOT NULL DEFAULT NOW() | 转存时刻 |

- **PK** `task_id`；**FK CASCADE** `task_id → task(id)`；**CHECK** `size_bytes >= 0`
- 新增 `idx_task_settled_needing_store`（`WHERE status='SETTLED' AND error_code IS NULL`，按 updated_at）：支持 `ScanSettledNeedingStore` 的 recoverMissingStore sweep（既有 idx_task_inflight 排除 SETTLED，不可用）。

---

## 0011 演化（2026-05-31）

> 计划：[管理后台配置线 plan](../plans/2026-05-31-001-feat-admin-console-config-plan.md) Unit 2；决策 [ADR-0008](../adr/0008-admin-console-session-auth.md)

### 新表：`operator_account`（运维登录账户）

管理后台会话认证；初始管理员经 env 种子，其余由初始管理员后台开通。

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | bigserial NOT NULL | **PK** |
| `username` | text NOT NULL | UNIQUE；登录用户名 |
| `password_hash` | text NOT NULL | **bcrypt(口令)**；低熵口令慢哈希（区别于 admin_token/business_key 的 HMAC）；明文/哈希绝不回显 |
| `enabled` | boolean NOT NULL DEFAULT true | 软禁用；false 拒绝登录 |
| `created_by` | text NOT NULL | 种子为 `'seed'`，后台开通为 `'operator:<id>'` |
| `created_at` / `updated_at` | timestamptz NOT NULL DEFAULT NOW() | |

- **PK** `id`；**UNIQUE** `username`

### 新表：`admin_session`（PG 会话）

会话存 PG（非 Redis）；存 token 的 HMAC 而非明文（ADR-0008 决策 3）。

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | bigserial NOT NULL | **PK** |
| `session_token_hash` | text NOT NULL | UNIQUE；`HMAC(pepper, 会话明文 token)` hex；明文仅在 HttpOnly Cookie |
| `operator_id` | bigint NOT NULL | FK → operator_account(id) ON DELETE CASCADE |
| `csrf_token` | text NOT NULL | 每会话 CSRF token；会话通道状态变更请求须带；Bearer 通道豁免 |
| `expires_at` | timestamptz NOT NULL | 鉴权校验 `expires_at > NOW()`；sweep 清过期 |
| `created_at` | timestamptz NOT NULL DEFAULT NOW() | |
| `last_seen_at` | timestamptz NULL | 最近活跃（best-effort） |

- **PK** `id`；**UNIQUE** `session_token_hash`；**FK CASCADE** `operator_id → operator_account(id)`
- **索引** `idx_admin_session_operator (operator_id)`：FK / 禁用账户批量清会话

### sqlc 生成代码

- `internal/db/operator_account.sql.go`：InsertOperatorAccount / GetOperatorAccountByUsername（含 password_hash，仅认证）/ GetOperatorAccountByID / ListOperatorAccounts / CountOperatorAccounts / SetOperatorAccountEnabled / UpdateOperatorPassword
- `internal/db/admin_session.sql.go`：InsertAdminSession / GetActiveAdminSessionByTokenHash（JOIN operator 校验 enabled + 未过期）/ TouchAdminSessionLastSeen / DeleteAdminSessionByTokenHash / DeleteAdminSessionsByOperator / DeleteExpiredAdminSessions

---

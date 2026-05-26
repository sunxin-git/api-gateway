# Schema v0001 —— 初始 schema 总览

> **migration 文件**：`migrations/0001_init.up.sql` / `migrations/0001_init.down.sql`
> **生效版本**：Phase 1（P0 必需 9 张表 + 5 个枚举）
> **PG 版本要求**：≥ 15（项目宪法 CLAUDE.md 技术栈）
> **生成时间**：2026-05-26
>
> 本文档是 schema 的**人类可读版**，配合 .sql 文件一起 review。
> 字段、类型、约束、索引均与 .up.sql 一一对应；偏差被视为 bug，必须修复其中一方。

---

## 顶部：偏离记录（与 v1.3 设计文档的差异）

本初始 schema 是 **P0 最小实现**，按 v1.3 §16 收敛后的 4 条工作流前置依赖落地。
以下记录与设计文档章节的**已知差异**，均经 plan 评审后确认为 P0 简化：

| 表 | 设计文档章节 | 差异内容 | 处理方式 |
|---|---|---|---|
| `business_account_ledger` | §3ter.2 | 设计文档列出 `available_delta` / `reserved_delta` / `used_delta` / `reference_type` / `reference_id` / `created_by` / `metadata` 字段；P0 schema **不含**这些。 | Phase 2 工作流 E 落地时按 `ALTER TABLE ADD COLUMN` 增量补充（用 0002+ migrations） |
| `business_account_balance` | §3ter.2 | 设计文档含 `last_ledger_id` 投影游标字段；P0 schema **不含**。 | Phase 2 工作流 E 落地时增量补充 |
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
| `task_status` | `SUBMITTED` / `UPSTREAM_SUBMITTING` / `UPSTREAM_SUBMITTED` / `COMPLETED` / `FAILED` / `CANCELLED` / `EXPIRED` / `SETTLING` / `SETTLED` | `task.status` |
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
| `submit_locked_until` | timestamptz | YES | NULL | v1.2.3：worker 抢占 lease 截止；超时 cron 回退到 SUBMITTED |
| `submit_locked_by` | text | YES | NULL | 抢占 worker ID |
| `submit_recover_count` | int | NO | 0 | v1.2.3：UPSTREAM_SUBMITTING 崩溃恢复次数；≥ 3 转 FAILED |
| `financial_snapshot` | jsonb | NO | `'{}'` | TaskFinancialSnapshot（AuthSnapshot + PricingSnapshot + ReservationLedgerID） |
| `accounting_month` | text | NO | — | 跨月归属 YYYY-MM；提交时刻冻结，永不切分 |
| `submitted_at` | timestamptz | NO | `NOW()` | 提交时间 |
| `terminal_at` | timestamptz | YES | NULL | 终态时间（COMPLETED / FAILED / ...） |
| `error_code` | text | YES | NULL | 错误码 |
| `error_message` | text | YES | NULL | 错误信息 |
| `updated_at` | timestamptz | NO | `NOW()` | 最后更新时间 |

**约束**：
- `pk_task` PRIMARY KEY (id)
- `fk_task_account` FK → business_account(id) ON DELETE RESTRICT
- `fk_task_channel` FK → channel(id) ON DELETE SET NULL
- `chk_task_submit_recover_count_non_negative` CHECK (submit_recover_count >= 0)
- `chk_task_accounting_month_format` CHECK (accounting_month ~ '^[0-9]{4}-(0[1-9]|1[0-2])$')

**索引**（4 个，全部针对 P0 已知热路径）：
- `idx_task_inflight` (business_account_id, status) **WHERE status NOT IN ('COMPLETED', 'FAILED', 'CANCELLED', 'EXPIRED', 'SETTLED')` —— inflight 查询（部分索引）
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
- **永远**新增 schema 变更走新文件 `0002_xxx.up.sql`，**不**修改已合并的旧 migration
- SQL 变更 PR 必须附 `EXPLAIN ANALYZE` 输出（CLAUDE.md 第六节）

-- ============================================================================
-- 0001_init.up.sql —— api-gateway 初始 schema（P0 必需 9 张表 + 5 个枚举）
--
-- 工具：golang-migrate v4（不要在本文件加 BEGIN/COMMIT，工具会自动包裹事务）
-- 引擎：PostgreSQL ≥ 15
-- 字符集：UTF8（PG 默认）
--
-- 字段定义来源（按优先级）：
--   1. docs/plans/2026-05-26-001-feat-phase-1-skeleton-and-migrations-plan.md Unit 7
--   2. docs/multimedia-gateway-design.md v1.3 §3ter.2 / §8.2 / §9 / §9bis.4.1 / §9bis.6 / §9bis.7
--   3. CONTEXT.md（账本不变量 / fallback_policy / isolation_required 等术语）
--
-- 偏离记录（已知 P0 简化，详见 docs/db/schema-v0001.md 顶部「偏离记录」节）：
--   - business_account_ledger 仅保留计划列出的最小字段（不含 *_delta / reference_*）
--   - business_account 用 text PK，不用 bigint + UNIQUE varchar 二级 ID
--   - gateway_admin_token / webhook_subscription 字段以计划为准，与 §9bis.7 表格略有出入
--
-- 关键不变量（CHECK 约束保障）：
--   - business_account_balance: available + reserved + used_total = recharge_total
--   - business_account_balance: 三态全部 >= 0
--
-- 创建顺序：枚举 → 父表（business_account / channel） → 子表（balance / ledger / routing / outbox / token / subscription / task）
-- ============================================================================


-- ----------------------------------------------------------------------------
-- A. 枚举类型（必须先于使用它的表创建）
-- ----------------------------------------------------------------------------

-- 账本流水类型枚举（CONTEXT.md「ledger entry type」）
--   recharge          充值入账
--   reserve           预扣（reserved 增加）
--   commit            结算（used 增加，reserved 减少）
--   release           释放预扣（reserved 减少，available 增加）
--   refund            返还额度（used 减少，available 增加，credit refund）
--   cashout           退款出账（recharge_total 减少，available 减少，cash refund）
--   recharge_reversal 充值冲销（罕见，用于订阅退订）
--   adjust            人工调账
--   expire            到期作废（v1.2 任务最长 30 天 EXPIRED 时释放预扣的对账目）
CREATE TYPE ledger_entry_type AS ENUM (
    'recharge',
    'reserve',
    'commit',
    'release',
    'refund',
    'cashout',
    'recharge_reversal',
    'adjust',
    'expire'
);

-- 任务状态机枚举（CONTEXT.md「任务状态枚举」 + 设计文档 §9ter.2 状态转移表）
--   SUBMITTED             已入库 + 已 reserve，等待 worker 抢占
--   UPSTREAM_SUBMITTING   worker 已 CAS 抢占本地提交权，尚未拿到 upstream_task_id（v1.2.2 中间态防孤儿）
--   UPSTREAM_SUBMITTED    已提交给上游，等待回执
--   COMPLETED / FAILED / CANCELLED / EXPIRED   四个终态（COMPLETED 上游 succeeded；EXPIRED 超过 30 天未完成）
--   SETTLING / SETTLED    账本结算阶段；SETTLED 是不可变终态
CREATE TYPE task_status AS ENUM (
    'SUBMITTED',
    'UPSTREAM_SUBMITTING',
    'UPSTREAM_SUBMITTED',
    'COMPLETED',
    'FAILED',
    'CANCELLED',
    'EXPIRED',
    'SETTLING',
    'SETTLED'
);

-- Outbox 投递状态（设计文档 §9bis.4.1 + v1.2.1 claim/lease 模式）
--   pending      待投递
--   delivering   已被某 worker 抢占（locked_by / locked_until 已写入）
--   delivered    业务系统已确认接收
--   failed       临时失败（delivery_attempts < 10），等待重试
--   dead_letter  达到 10 次重试上限，进 DLQ 人工介入
CREATE TYPE outbox_delivery_status AS ENUM (
    'pending',
    'delivering',
    'delivered',
    'failed',
    'dead_letter'
);

-- 路由降级策略（CONTEXT.md「fallback_policy」 + 设计文档 §8.3）
--   strict              候选不可用直接 503，**不降级**（默认）
--   next_rule           候选不可用 → 求值下一条规则（仍限本企业规则集）
--   global_pool         候选不可用 → 降级到全局规则池（NULL business_account_id）
--   legacy_distributor  候选不可用 → 降级到 new-api 风格的 model + group 选择
-- 注意：isolation_required = true 的企业仅允许 strict / 同企业 next_rule，
--       禁止 global_pool / legacy_distributor / 跨企业 next_rule（运行时强校验）
CREATE TYPE fallback_policy AS ENUM (
    'strict',
    'next_rule',
    'global_pool',
    'legacy_distributor'
);

-- 业务账户状态（设计文档 §9bis.7）
--   active      正常
--   suspended   暂停（不允许新提交，已 inflight 任务按快照继续）
--   frozen      冻结（drift 检测命中后冻结，所有 reserve 拒绝）
--   deleted     软删除（必须前置校验：无 inflight + 无未结余额）
CREATE TYPE business_account_status AS ENUM (
    'active',
    'suspended',
    'frozen',
    'deleted'
);


-- ----------------------------------------------------------------------------
-- B. business_account（业务账户）—— 设计文档 §9bis.7 + 基线决策 #20
-- ----------------------------------------------------------------------------
-- P0 简化：id 直接用业务系统外部 ID（text），不引入网关内部 bigint PK + UNIQUE varchar 模型
CREATE TABLE business_account (
    id                  text                    NOT NULL,
    status              business_account_status NOT NULL DEFAULT 'active',
    -- 企业隔离硬开关（v1.2 基线决策 #20）：true 时禁止任何形式的跨企业降级
    isolation_required  boolean                 NOT NULL DEFAULT false,
    -- 紧急逃生门：非 NULL 且未过期时允许临时跨企业降级（需 Root 双人审批，最长 24h）
    break_glass_until   timestamptz             NULL,
    -- 业务侧可选标签（运营标签、外部映射等）
    metadata            jsonb                   NOT NULL DEFAULT '{}'::jsonb,
    created_at          timestamptz             NOT NULL DEFAULT NOW(),
    updated_at          timestamptz             NOT NULL DEFAULT NOW(),
    CONSTRAINT pk_business_account PRIMARY KEY (id)
);

COMMENT ON TABLE  business_account                     IS '业务账户：业务系统侧企业账户在网关侧的镜像（CONTEXT.md business_account）';
COMMENT ON COLUMN business_account.isolation_required  IS '企业隔离硬开关；true 时禁止跨企业降级（见 8.3.6）';
COMMENT ON COLUMN business_account.break_glass_until   IS '紧急逃生门截止时间；NULL = 未启用';


-- ----------------------------------------------------------------------------
-- C. business_account_balance（账户余额投影）—— 设计文档 §3ter.2
-- ----------------------------------------------------------------------------
-- 严格投影（不是缓存）：ledger 每次写入同事务更新 balance；reconcile 每 5 分钟校验，drift 触发冻结
CREATE TABLE business_account_balance (
    business_account_id text         NOT NULL,
    -- 三态余额（CONTEXT.md「三态余额」）
    available           bigint       NOT NULL DEFAULT 0,
    reserved            bigint       NOT NULL DEFAULT 0,
    used_total          bigint       NOT NULL DEFAULT 0,
    -- 累计入账（单调递增）
    recharge_total      bigint       NOT NULL DEFAULT 0,
    -- 累计退款（审计字段，不进不变量；v1.2.1 数学校准）
    refund_total        bigint       NOT NULL DEFAULT 0,
    -- 乐观锁版本号（CAS 用，每次更新 +1）
    version             bigint       NOT NULL DEFAULT 0,
    -- drift 检测冻结字段（v1.2 新增）
    frozen              boolean      NOT NULL DEFAULT false,
    frozen_reason       text         NULL,
    frozen_at           timestamptz  NULL,
    updated_at          timestamptz  NOT NULL DEFAULT NOW(),
    CONSTRAINT pk_business_account_balance PRIMARY KEY (business_account_id),
    CONSTRAINT fk_business_account_balance_account
        FOREIGN KEY (business_account_id)
        REFERENCES business_account (id)
        ON DELETE RESTRICT,
    -- 三态非负（CLAUDE.md 显式优于隐式）
    CONSTRAINT chk_business_account_balance_non_negative
        CHECK (available >= 0 AND reserved >= 0 AND used_total >= 0 AND recharge_total >= 0 AND refund_total >= 0),
    -- 账本不变量（CONTEXT.md「账本不变量」 + 设计文档 §3ter.2）
    -- available + reserved + used_total = recharge_total
    -- refund_total **不**进入不变量（仅审计）
    CONSTRAINT chk_business_account_balance_invariant
        CHECK (available + reserved + used_total = recharge_total)
);

COMMENT ON TABLE  business_account_balance                  IS '账户余额投影：ledger 严格投影；drift 检测命中即冻结（CONTEXT.md balance）';
COMMENT ON COLUMN business_account_balance.refund_total     IS '累计退款（审计字段，不进入账本不变量）';
COMMENT ON COLUMN business_account_balance.version          IS '乐观锁版本；CAS 更新 balance 时必须 SET version = version + 1';


-- ----------------------------------------------------------------------------
-- D. business_account_ledger（不可变流水）—— 设计文档 §3ter.2
-- ----------------------------------------------------------------------------
-- 不可变：所有扣减 / 退款 / 对账的唯一真相源；永不 UPDATE / DELETE
-- P0 简化（已记录偏离）：未引入 *_delta / reference_type / reference_id / created_by 字段，
--   这些字段在 Phase 2 工作流 E 落地时按 ALTER TABLE ADD COLUMN 增量补充
CREATE TABLE business_account_ledger (
    id                  bigserial        NOT NULL,
    business_account_id text             NOT NULL,
    entry_type          ledger_entry_type NOT NULL,
    -- 金额（正数 = 入账，负数 = 出账，方向取决于 entry_type 语义）
    amount              bigint           NOT NULL,
    -- 关联同一笔业务流的多个 entry（如 reserve → commit / release 用同一 correlation_id）
    correlation_id      text             NOT NULL,
    -- 充值幂等键（sha256(external_ref + canonical_body)）；NULL 表示非充值场景
    idempotency_key     text             NULL,
    -- BillingSnapshot 快照（表达式 + 哈希 + group ratio + cost catalog 引用 + 汇率 + usage）
    snapshot            jsonb            NOT NULL DEFAULT '{}'::jsonb,
    created_at          timestamptz      NOT NULL DEFAULT NOW(),
    CONSTRAINT pk_business_account_ledger PRIMARY KEY (id),
    CONSTRAINT fk_business_account_ledger_account
        FOREIGN KEY (business_account_id)
        REFERENCES business_account (id)
        ON DELETE RESTRICT
);

-- 账户流水查询（管理后台、对账 job）
CREATE INDEX idx_ledger_account_created
    ON business_account_ledger (business_account_id, created_at DESC);

-- reserve → commit / release 配对（结算时反查同 correlation_id 的 entry）
CREATE INDEX idx_ledger_correlation
    ON business_account_ledger (correlation_id);

-- 充值幂等（部分唯一索引：仅对非 NULL idempotency_key 强制唯一）
CREATE UNIQUE INDEX uq_ledger_idempotency_key
    ON business_account_ledger (idempotency_key)
    WHERE idempotency_key IS NOT NULL;

COMMENT ON TABLE  business_account_ledger                 IS '账本不可变流水：所有扣减 / 退款 / 对账的唯一真相源（CONTEXT.md ledger）';
COMMENT ON COLUMN business_account_ledger.correlation_id  IS '关联同一笔业务流的多个 entry（reserve → commit / release）';
COMMENT ON COLUMN business_account_ledger.idempotency_key IS '充值幂等键 sha256(external_ref + canonical_body)，部分唯一';


-- ----------------------------------------------------------------------------
-- E. channel（渠道：上游 provider 凭据抽象）—— 设计文档 §8.2
-- ----------------------------------------------------------------------------
CREATE TABLE channel (
    id                          bigserial   NOT NULL,
    -- 运营可见名称（火山-真人-biz_001 等）
    name                        text        NOT NULL,
    -- provider 类型（volc_seedance_v2 / openai / alibaba_wanxiang 等）
    provider_type               text        NOT NULL,
    enabled                     boolean     NOT NULL DEFAULT true,
    -- 业务账户白名单：非空时仅列表内账户可用本 channel
    -- 用 text[] + GIN 索引，性能远好于 jsonb（PG 内置数组操作符）
    restricted_business_accounts text[]     NOT NULL DEFAULT '{}'::text[],
    -- 标签（如 seedance:realperson:biz_001），人类可读
    channel_purpose             text        NULL,
    -- 凭据密文（envelope encryption ciphertext；P0 用单一 AES-GCM 过渡，P1 接 KEK/DEK 分层）
    credentials_encrypted       bytea       NOT NULL,
    -- 加密 KEK 版本（v1 / v2 / ...）；P0 默认 1
    key_version                 int         NOT NULL DEFAULT 1,
    -- 渠道其他配置（限流、超时、模型映射等结构化设置）
    other_settings              jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at                  timestamptz NOT NULL DEFAULT NOW(),
    updated_at                  timestamptz NOT NULL DEFAULT NOW(),
    CONSTRAINT pk_channel PRIMARY KEY (id),
    CONSTRAINT uq_channel_name UNIQUE (name),
    CONSTRAINT chk_channel_key_version_positive CHECK (key_version >= 1)
);

-- restricted_business_accounts 数组查询（distributor 过滤热路径）
CREATE INDEX idx_channel_restricted_accounts
    ON channel
    USING GIN (restricted_business_accounts);

-- enabled channel 列表查询（管理后台与路由命中后过滤）
CREATE INDEX idx_channel_enabled
    ON channel (enabled)
    WHERE enabled = true;

COMMENT ON TABLE  channel                             IS '渠道：一组上游 provider 凭据的抽象（CONTEXT.md channel）';
COMMENT ON COLUMN channel.restricted_business_accounts IS '业务账户白名单；空数组 = 不限制，非空 = 仅列内账户可用';
COMMENT ON COLUMN channel.credentials_encrypted       IS 'envelope encryption 密文；明文绝不入库';
COMMENT ON COLUMN channel.key_version                 IS 'KEK 版本号，支持平滑轮换';


-- ----------------------------------------------------------------------------
-- F. channel_routing_rule（路由规则）—— 设计文档 §8.2 + §8.3
-- ----------------------------------------------------------------------------
CREATE TABLE channel_routing_rule (
    id                  bigserial   NOT NULL,
    -- NULL = 全局默认规则；非 NULL = 该业务账户的覆盖规则
    business_account_id text        NULL,
    -- 同 (business_account_id) 下规则的求值优先级，**数值大优先**
    priority            int         NOT NULL DEFAULT 100,
    -- expr-lang/expr 表达式文本（如 `param("is_real_person") == true`）
    -- 求值上下文走 normalized routing context（CONTEXT.md），禁止访问大字段
    condition_expr      text        NOT NULL,
    -- 候选 channel ID 列表；命中后从这些 channel 按 priority + weight 选
    target_channel_ids  bigint[]    NOT NULL,
    -- 降级策略（CONTEXT.md fallback_policy）；R11 默认 strict（fail-closed）
    fallback_policy     fallback_policy NOT NULL DEFAULT 'strict',
    enabled             boolean     NOT NULL DEFAULT true,
    created_at          timestamptz NOT NULL DEFAULT NOW(),
    updated_at          timestamptz NOT NULL DEFAULT NOW(),
    CONSTRAINT pk_channel_routing_rule PRIMARY KEY (id),
    CONSTRAINT fk_channel_routing_rule_account
        FOREIGN KEY (business_account_id)
        REFERENCES business_account (id)
        ON DELETE CASCADE,
    -- target_channel_ids 不能是空数组（命中后必须有候选）
    CONSTRAINT chk_channel_routing_rule_targets_non_empty
        CHECK (array_length(target_channel_ids, 1) >= 1)
);

-- 路由查询热路径：(business_account_id, priority DESC) 取最高优先级规则
CREATE INDEX idx_channel_routing_rule_account_priority
    ON channel_routing_rule (business_account_id, priority DESC);

-- 仅启用的规则
CREATE INDEX idx_channel_routing_rule_enabled
    ON channel_routing_rule (enabled)
    WHERE enabled = true;

COMMENT ON TABLE  channel_routing_rule                  IS '路由规则：将 (账户 + 请求参数) 映射到候选 channel（CONTEXT.md channel_routing_rule）';
COMMENT ON COLUMN channel_routing_rule.business_account_id IS 'NULL = 全局默认规则；非 NULL = 企业覆盖规则';
COMMENT ON COLUMN channel_routing_rule.fallback_policy  IS '默认 strict（fail-closed）；isolation_required 企业禁用 global_pool / legacy_distributor';


-- ----------------------------------------------------------------------------
-- G. webhook_event_outbox（事件出箱）—— 设计文档 §9bis.4.1
-- ----------------------------------------------------------------------------
-- v1.2 硬约束：**必须**部署在主库（与 ledger 同库），保证同事务发布
-- v1.2.1 claim/lease 模式：locked_by + locked_until 支持多节点 SKIP LOCKED 并发扫描
CREATE TABLE webhook_event_outbox (
    -- 单调递增的 event_id 是业务系统拉取游标
    event_id                 bigserial             NOT NULL,
    -- NULL = 全局事件（非账户绑定）；非 NULL = 关联业务账户
    business_account_id      text                  NULL,
    -- 事件类型（account.created / account.recharged / task.completed 等）
    event_type               text                  NOT NULL,
    -- 事件 payload（JSON），结构由 event_type 决定
    payload                  jsonb                 NOT NULL,
    -- 财务事件标志：true → 保留期 ≥ 1 年；false → 保留期 ≥ 30 天
    is_financial             boolean               NOT NULL DEFAULT false,
    -- 事件保留截止时间（>= 创建时刻 + 财务/非财务对应保留期）
    retention_until          timestamptz           NOT NULL,
    -- 投递状态（CONTEXT.md outbox claim/lease）
    delivery_status          outbox_delivery_status NOT NULL DEFAULT 'pending',
    delivery_attempts        int                   NOT NULL DEFAULT 0,
    -- worker 抢占信息（lease 模式）
    locked_by                text                  NULL,
    locked_until             timestamptz           NULL,
    -- 业务侧去重键（默认 = event_id 字符串，财务事件长期去重 ≥ 1 年）
    delivery_idempotency_key text                  NOT NULL,
    last_pushed_at           timestamptz           NULL,
    created_at               timestamptz           NOT NULL DEFAULT NOW(),
    CONSTRAINT pk_webhook_event_outbox PRIMARY KEY (event_id),
    CONSTRAINT fk_webhook_event_outbox_account
        FOREIGN KEY (business_account_id)
        REFERENCES business_account (id)
        ON DELETE SET NULL,
    -- 投递幂等键全局唯一
    CONSTRAINT uq_webhook_event_outbox_idempotency UNIQUE (delivery_idempotency_key),
    -- 重试次数非负
    CONSTRAINT chk_webhook_event_outbox_attempts_non_negative CHECK (delivery_attempts >= 0)
);

-- 扫描热路径：dispatcher 每 5 秒查 pending + 超时的 delivering
-- 部分索引仅覆盖未投递完成的行，PG 扫描代价 O(待投递事件数) 而非 O(总事件数)
CREATE INDEX idx_webhook_event_outbox_pending_event_id
    ON webhook_event_outbox (delivery_status, event_id)
    WHERE delivery_status IN ('pending', 'delivering');

-- 保留期清理 job（每天扫一次过期事件）
CREATE INDEX idx_webhook_event_outbox_retention
    ON webhook_event_outbox (retention_until);

COMMENT ON TABLE  webhook_event_outbox                          IS '事件出箱：与 ledger 同事务写入，业务系统拉取/重放游标（CONTEXT.md outbox）';
COMMENT ON COLUMN webhook_event_outbox.is_financial             IS 'true = 财务事件（保留 ≥ 1 年）；false = 非财务（保留 ≥ 30 天）';
COMMENT ON COLUMN webhook_event_outbox.locked_by                IS 'claim/lease：当前抢占该事件的 worker ID（hostname_pid）';
COMMENT ON COLUMN webhook_event_outbox.locked_until             IS 'lease 截止时间；NOW() > locked_until 时其他 worker 可抢占';
COMMENT ON COLUMN webhook_event_outbox.delivery_idempotency_key IS '业务侧去重键；财务事件长期去重 ≥ 1 年';


-- ----------------------------------------------------------------------------
-- H. gateway_admin_token（管理 Token）—— 设计文档 §9bis.6 + 基线决策 #16
-- ----------------------------------------------------------------------------
CREATE TABLE gateway_admin_token (
    id                          bigserial    NOT NULL,
    -- bcrypt / argon2id 哈希（不存明文）
    token_hash                  text         NOT NULL,
    description                 text         NOT NULL,
    -- 细粒度权限范围（business_account:read / recharge / token:write 等）
    scopes                      text[]       NOT NULL DEFAULT '{}'::text[],
    -- 源 IP 白名单（CIDR 数组）；用 PG cidr 类型，支持 inet_contains 索引查询
    ip_allowlist                cidr[]       NOT NULL DEFAULT '{}'::cidr[],
    -- 阀门字段（v1.1 新增，blast radius 控制）
    daily_recharge_quota_limit  bigint       NULL,
    daily_account_create_limit  int          NULL,
    single_recharge_max         bigint       NULL,
    requests_per_minute         int          NULL,
    circuit_breaker_enabled     boolean      NOT NULL DEFAULT false,
    -- 审计字段
    created_by                  text         NOT NULL,
    created_at                  timestamptz  NOT NULL DEFAULT NOW(),
    expires_at                  timestamptz  NULL,
    revoked_at                  timestamptz  NULL,
    CONSTRAINT pk_gateway_admin_token PRIMARY KEY (id),
    CONSTRAINT uq_gateway_admin_token_hash UNIQUE (token_hash),
    -- 阀门非负
    CONSTRAINT chk_gateway_admin_token_daily_recharge_non_negative
        CHECK (daily_recharge_quota_limit IS NULL OR daily_recharge_quota_limit >= 0),
    CONSTRAINT chk_gateway_admin_token_daily_create_non_negative
        CHECK (daily_account_create_limit IS NULL OR daily_account_create_limit >= 0),
    CONSTRAINT chk_gateway_admin_token_single_recharge_non_negative
        CHECK (single_recharge_max IS NULL OR single_recharge_max >= 0),
    CONSTRAINT chk_gateway_admin_token_rpm_non_negative
        CHECK (requests_per_minute IS NULL OR requests_per_minute >= 0)
);

-- 活跃 Token 查询（鉴权中间件热路径）
CREATE INDEX idx_gateway_admin_token_active
    ON gateway_admin_token (revoked_at)
    WHERE revoked_at IS NULL;

COMMENT ON TABLE  gateway_admin_token                            IS 'Admin Token：业务系统调网关的认证凭据（CONTEXT.md Admin Token）';
COMMENT ON COLUMN gateway_admin_token.token_hash                 IS 'bcrypt/argon2id 哈希；明文绝不入库';
COMMENT ON COLUMN gateway_admin_token.scopes                     IS '细粒度权限范围；推荐生产环境只授权必要 scope';
COMMENT ON COLUMN gateway_admin_token.ip_allowlist               IS 'CIDR 源 IP 白名单；未匹配请求直接 401，不消耗限流';
COMMENT ON COLUMN gateway_admin_token.daily_recharge_quota_limit IS 'NULL = 无限；非 NULL = 单日充值上限（阀门）';


-- ----------------------------------------------------------------------------
-- I. webhook_subscription（Webhook 订阅）—— 设计文档 §9bis
-- ----------------------------------------------------------------------------
CREATE TABLE webhook_subscription (
    id                      bigserial    NOT NULL,
    business_account_id     text         NOT NULL,
    endpoint_url            text         NOT NULL,
    -- HMAC 签名密钥密文（envelope encryption；明文绝不入库）
    hmac_secret_encrypted   bytea        NOT NULL,
    key_version             int          NOT NULL DEFAULT 1,
    -- 订阅的事件类型列表；空数组 = 订阅全部
    event_types             text[]       NOT NULL DEFAULT '{}'::text[],
    enabled                 boolean      NOT NULL DEFAULT true,
    created_at              timestamptz  NOT NULL DEFAULT NOW(),
    updated_at              timestamptz  NOT NULL DEFAULT NOW(),
    CONSTRAINT pk_webhook_subscription PRIMARY KEY (id),
    CONSTRAINT fk_webhook_subscription_account
        FOREIGN KEY (business_account_id)
        REFERENCES business_account (id)
        ON DELETE CASCADE,
    CONSTRAINT chk_webhook_subscription_key_version_positive
        CHECK (key_version >= 1)
);

-- 派发 webhook 时按 business_account_id 取所有 enabled 订阅
CREATE INDEX idx_webhook_subscription_account_enabled
    ON webhook_subscription (business_account_id, enabled);

COMMENT ON TABLE  webhook_subscription                       IS 'Webhook 订阅：业务账户对网关事件的回调注册';
COMMENT ON COLUMN webhook_subscription.event_types           IS '订阅的事件类型；空数组 = 订阅全部';
COMMENT ON COLUMN webhook_subscription.hmac_secret_encrypted IS 'HMAC 签名密钥密文；明文绝不入库';


-- ----------------------------------------------------------------------------
-- J. task（异步任务）—— 设计文档 §9 + §9ter + v1.2.2 / v1.2.3 修正
-- ----------------------------------------------------------------------------
-- 任务表 + 8 状态机 + UPSTREAM_SUBMITTING 中间态防孤儿（v1.2.2）+ submit_locked_* 崩溃恢复（v1.2.3）
CREATE TABLE task (
    -- 应用层生成的雪花 / ulid（CONTEXT.md task）
    id                    text         NOT NULL,
    business_account_id   text         NOT NULL,
    token_id              bigint       NULL,
    -- 选中的 channel；NULL 表示提交时尚未路由（应避免，但保留容错）
    channel_id            bigint       NULL,
    provider_type         text         NOT NULL,
    model                 text         NOT NULL,
    status                task_status  NOT NULL DEFAULT 'SUBMITTED',
    upstream_task_id      text         NULL,
    -- v1.2.3 崩溃恢复字段：worker 抢占本地提交权时写入
    submit_locked_until   timestamptz  NULL,
    submit_locked_by      text         NULL,
    -- 崩溃恢复重试计数（达到 3 次后 cron 转 FAILED，避免无限循环）
    submit_recover_count  int          NOT NULL DEFAULT 0,
    -- TaskFinancialSnapshot（授权快照 + 价格快照 + 预扣 ledger ID）
    financial_snapshot    jsonb        NOT NULL DEFAULT '{}'::jsonb,
    -- 跨月归属：固定为 submitted_at 的月份（YYYY-MM），永不切分（§9ter.6）
    accounting_month      text         NOT NULL,
    submitted_at          timestamptz  NOT NULL DEFAULT NOW(),
    terminal_at           timestamptz  NULL,
    error_code            text         NULL,
    error_message         text         NULL,
    updated_at            timestamptz  NOT NULL DEFAULT NOW(),
    CONSTRAINT pk_task PRIMARY KEY (id),
    CONSTRAINT fk_task_account
        FOREIGN KEY (business_account_id)
        REFERENCES business_account (id)
        ON DELETE RESTRICT,
    CONSTRAINT fk_task_channel
        FOREIGN KEY (channel_id)
        REFERENCES channel (id)
        ON DELETE SET NULL,
    CONSTRAINT chk_task_submit_recover_count_non_negative
        CHECK (submit_recover_count >= 0),
    -- accounting_month 格式：YYYY-MM（如 2026-05）
    CONSTRAINT chk_task_accounting_month_format
        CHECK (accounting_month ~ '^[0-9]{4}-(0[1-9]|1[0-2])$')
);

-- inflight 任务查询（按账户、按状态聚合 metrics 用）
-- 部分索引仅覆盖非终态行，扫描代价 O(inflight) 而非 O(全表)
CREATE INDEX idx_task_inflight
    ON task (business_account_id, status)
    WHERE status NOT IN ('COMPLETED', 'FAILED', 'CANCELLED', 'EXPIRED', 'SETTLED');

-- 崩溃恢复 cron：扫描 UPSTREAM_SUBMITTING + submit_locked_until 过期的任务
CREATE INDEX idx_task_submit_recover
    ON task (status, submit_locked_until)
    WHERE status = 'UPSTREAM_SUBMITTING';

-- 月结查询（按账期 + 状态聚合）
CREATE INDEX idx_task_accounting_month
    ON task (accounting_month, status);

-- channel 关联查询（按渠道反查任务，运营排查用）
CREATE INDEX idx_task_channel_id
    ON task (channel_id)
    WHERE channel_id IS NOT NULL;

COMMENT ON TABLE  task                       IS '异步任务：长耗时上游调用的本地记录（CONTEXT.md task）';
COMMENT ON COLUMN task.status                IS '任务状态机；状态转移仅允许 §9ter.2 状态转移表方向（CAS）';
COMMENT ON COLUMN task.submit_locked_until   IS 'v1.2.3：worker 抢占 lease 截止；超时 cron 回退到 SUBMITTED';
COMMENT ON COLUMN task.submit_recover_count  IS 'v1.2.3：UPSTREAM_SUBMITTING 崩溃恢复次数；>=3 转 FAILED';
COMMENT ON COLUMN task.financial_snapshot    IS 'TaskFinancialSnapshot（AuthSnapshot + PricingSnapshot + ReservationLedgerID）';
COMMENT ON COLUMN task.accounting_month      IS '跨月归属：固定为提交时刻的 YYYY-MM，永不切分';

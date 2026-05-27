-- ============================================================================
-- Migration 0004: 业务侧 API Key 表 + actor_type 枚举扩展（F-min Unit 1）
-- ----------------------------------------------------------------------------
-- 设计文档：docs/multimedia-gateway-design.md §9
-- 实施计划：docs/plans/2026-05-27-004-feat-workflow-f-min-openai-compat-relay-plan.md Unit 1
-- 评审依据：F-min plan §决策 D4 + D5（HMAC pepper 复用 + business_account_api_key schema）
--
-- 本次变更总览：
--   1. ALTER TYPE actor_type ADD VALUE 'business_key'（plan §Unit 7 决策点提前到 Unit 1
--      —— 避免 Unit 2-7 期间 schema 半截状态；ledger 写入 business_key actor 即时可用）
--   2. CREATE TABLE business_account_api_key：业务系统对外身份凭据表
--   3. UNIQUE on key_hash + 索引 + FK CASCADE
--   4. COMMENT 说明 pepper 复用 GATEWAY_TOKEN_PEPPER（与 admin token 同源）
--
-- 关键 PG 语义说明：
--   - ALTER TYPE ADD VALUE 不能在同事务里被使用，必须独立提交（migrate 自动每文件 1 事务，OK）
--   - PG 不支持 DROP VALUE 反向；down.sql 仅删表，actor_type 留 business_key
--     枚举值（运维 SOP 可手工迁移 DDL 后清理；MVP 接受）
-- ============================================================================


-- ----------------------------------------------------------------------------
-- 1. ALTER TYPE actor_type ADD VALUE 'business_key'
-- ----------------------------------------------------------------------------
-- 业务系统通过 /v1/chat/completions 走 relay 写 ledger entry 时，actor 标识为
-- business_key:{api_key_id} —— 与 admin_token:{token_id} 同 pattern，便于审计反查。

ALTER TYPE actor_type ADD VALUE IF NOT EXISTS 'business_key';

COMMENT ON TYPE actor_type IS '操作来源类型；与 actor_id 配合构成结构化审计两列。值：admin_token / cli / system / task / business_key';


-- ----------------------------------------------------------------------------
-- 2. business_account_api_key 表
-- ----------------------------------------------------------------------------
-- 业务系统接入网关的对外凭据；与 admin token 共享 HMAC pepper（F-min 决策 D4）。
-- 一个 business_account 可有多个 active key（如 prod / staging / 子部门）；
-- 删除账户时所有 key 通过 FK CASCADE 自动失效（防"账户没了但 key 还能 auth"）。

CREATE TABLE business_account_api_key (
    id                    bigserial   NOT NULL,
    -- 关联业务账户；与 business_account.id 外键 CASCADE
    business_account_id   text        NOT NULL,
    -- 运营标签（如 "creator-platform-prod-key-1"）；运维 list 时区分用途
    description           text        NOT NULL,
    -- HMAC-SHA-256(GATEWAY_TOKEN_PEPPER, plaintext) hex；与 admin token 同源
    -- 鉴权热路径：单 row UNIQUE 查询；明文绝不入库
    key_hash              text        NOT NULL,
    -- RPM 限速；NULL = 不限速；业务侧 InProcessRPM 按 key.id 维度计数
    requests_per_minute   int         NULL,
    -- 创建者标识；MVP admin-cli 硬编码 "cli:bootstrap"，不接受 flag 注入
    created_by            text        NOT NULL,
    created_at            timestamptz NOT NULL DEFAULT NOW(),
    -- revoke 时写入；查询用 WHERE revoked_at IS NULL 过滤
    revoked_at            timestamptz NULL,
    -- 鉴权命中时 best-effort 异步更新（5min 批量 flush）；运维查"长期未用 key"
    last_used_at          timestamptz NULL,
    updated_at            timestamptz NOT NULL DEFAULT NOW(),

    CONSTRAINT pk_business_account_api_key PRIMARY KEY (id),
    -- 鉴权热路径：HMAC hash 单 row 查询；UNIQUE 防 hash 碰撞（理论上 256 bit 不会撞）
    CONSTRAINT uq_business_account_api_key_hash UNIQUE (key_hash),
    -- FK CASCADE：删账户时 key 一并失效（与设计文档 business_account 生命周期一致）
    CONSTRAINT fk_business_account_api_key_account
        FOREIGN KEY (business_account_id) REFERENCES business_account (id) ON DELETE CASCADE,
    -- RPM 非负（防御编程；admin-cli 入参校验已保证，DB 层兜底）
    CONSTRAINT chk_business_account_api_key_rpm_non_negative
        CHECK (requests_per_minute IS NULL OR requests_per_minute > 0)
);

-- 运维查"账户 X 有几个 active key"：按 account_id 过滤 + 未 revoked
CREATE INDEX idx_business_account_api_key_account_active
    ON business_account_api_key (business_account_id)
    WHERE revoked_at IS NULL;

COMMENT ON TABLE  business_account_api_key                     IS '业务系统对外 API Key；与 admin token 共享 HMAC pepper（F-min 决策 D4）';
COMMENT ON COLUMN business_account_api_key.business_account_id IS '关联 business_account.id；FK CASCADE 保证账户删除时 key 一并失效';
COMMENT ON COLUMN business_account_api_key.key_hash            IS 'HMAC-SHA-256(GATEWAY_TOKEN_PEPPER, plaintext) 的 hex 字符串（64 char）；与 admin token 同 pepper 同算法；明文绝不入库';
COMMENT ON COLUMN business_account_api_key.requests_per_minute IS 'RPM 上限；NULL = 不限速；按 key.id 维度计数（InProcessRPM）';
COMMENT ON COLUMN business_account_api_key.created_by          IS '创建者标识；MVP admin-cli 硬编码 cli:bootstrap';
COMMENT ON COLUMN business_account_api_key.revoked_at          IS '吊销时间；NULL = 未吊销；查询 WHERE revoked_at IS NULL 过滤';
COMMENT ON COLUMN business_account_api_key.last_used_at        IS '最近鉴权命中时刻；best-effort 异步批量更新（不阻塞鉴权热路径）';

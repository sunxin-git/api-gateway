-- ============================================================================
-- Migration 0002: ledger / balance 字段扩展 + 不可变 trigger + 复合 UNIQUE + 守门 CHECK
-- ----------------------------------------------------------------------------
-- 设计文档：docs/multimedia-gateway-design.md §3ter.2
-- 实施计划：docs/plans/2026-05-26-002-feat-workflow-e-ledger-infrastructure-plan.md Unit 1
-- 评审依据：document-review pass 1/2 多 persona 共识（pgx native / CTE 原子性 / 不可变 trigger
--           / 复合 UNIQUE 幂等 / actor_type+actor_id 结构化审计 / canonical body sha256 / 等）
--
-- 关键 PG 语义说明：
--   1. ADD COLUMN ... NOT NULL DEFAULT <const_literal> 在 PG 11+ 不重写表，秒级完成
--   2. 行级 trigger 不覆盖 TRUNCATE，所以需单独 statement-level trigger
--   3. plpgsql 异常会让外层 tx 全部回滚（外加 caller 看到明确错误消息）
-- ============================================================================


-- ----------------------------------------------------------------------------
-- 1. 新增枚举：actor_type（结构化审计两列：actor_type + actor_id）
-- ----------------------------------------------------------------------------

CREATE TYPE actor_type AS ENUM (
    'admin_token',  -- Admin Token 调用（D-min HTTP 路径）
    'cli',          -- admin-cli 调用（P0 写死 cli:bootstrap）
    'system',       -- 系统组件（reconciler / rebuild / migration bootstrap）
    'task'          -- 异步任务路径（工作流 F task service）
);

COMMENT ON TYPE actor_type IS '操作来源类型；与 actor_id 配合构成结构化审计两列';


-- ----------------------------------------------------------------------------
-- 2. business_account_ledger 字段扩展
-- ----------------------------------------------------------------------------

ALTER TABLE business_account_ledger
    -- delta 三件套：reconciler 重算 balance 时按 entry_type 无关的求和直接得 expected 值
    ADD COLUMN available_delta        bigint     NOT NULL DEFAULT 0,
    ADD COLUMN reserved_delta         bigint     NOT NULL DEFAULT 0,
    ADD COLUMN used_delta             bigint     NOT NULL DEFAULT 0,
    -- reference 反查：哪个 task / topup_order / manual_adjust 触发了本笔
    ADD COLUMN reference_type         text       NULL,
    ADD COLUMN reference_id           text       NULL,
    -- 业务侧附加标签（运营查询 / 月结分组 / 等）
    ADD COLUMN metadata               jsonb      NOT NULL DEFAULT '{}'::jsonb,
    -- 结构化审计两列（防 created_by 自由字符串伪造）
    ADD COLUMN actor_type             actor_type NOT NULL DEFAULT 'system',
    ADD COLUMN actor_id               text       NOT NULL DEFAULT 'bootstrap',
    -- 充值幂等命中时 service 比对的 canonical body sha256（32 字节）
    ADD COLUMN canonical_body_sha256  bytea      NULL,
    ADD CONSTRAINT chk_ledger_canonical_body_hash_len
        CHECK (canonical_body_sha256 IS NULL OR octet_length(canonical_body_sha256) = 32);

COMMENT ON COLUMN business_account_ledger.available_delta        IS '此 entry 对 available 余额的影响（reconciler SUM 用）';
COMMENT ON COLUMN business_account_ledger.reserved_delta         IS '此 entry 对 reserved 余额的影响';
COMMENT ON COLUMN business_account_ledger.used_delta             IS '此 entry 对 used_total 累计的影响';
COMMENT ON COLUMN business_account_ledger.reference_type         IS '关联实体类型：task / topup_order / manual_adjust / monthly_settle';
COMMENT ON COLUMN business_account_ledger.reference_id           IS '关联实体 ID（如 task_id）';
COMMENT ON COLUMN business_account_ledger.metadata               IS '业务侧附加标签（jsonb；不含 PII 原值）';
COMMENT ON COLUMN business_account_ledger.actor_type             IS '操作来源（结构化审计两列）';
COMMENT ON COLUMN business_account_ledger.actor_id               IS '操作者 ID（admin_token id / cli username / system component / task id）';
COMMENT ON COLUMN business_account_ledger.canonical_body_sha256  IS '充值幂等命中时比对的 canonical body sha256；防沉默篡改';


-- ----------------------------------------------------------------------------
-- 3. business_account_balance 字段扩展：last_ledger_id 投影游标
-- ----------------------------------------------------------------------------

ALTER TABLE business_account_balance
    ADD COLUMN last_ledger_id bigint NOT NULL DEFAULT 0;

COMMENT ON COLUMN business_account_balance.last_ledger_id IS '已聚合到的最大 ledger.id（投影游标 + rebuild CAS 锚点）';


-- ----------------------------------------------------------------------------
-- 4. UNIQUE 索引重建：跨 entry_type 隔离的 idempotency_key
--    旧索引 uq_ledger_idempotency_key 是全表唯一，不同 entry_type 共用 key 会撞索引
-- ----------------------------------------------------------------------------

DROP INDEX IF EXISTS uq_ledger_idempotency_key;

CREATE UNIQUE INDEX uq_ledger_idempotency_key_per_type
    ON business_account_ledger (entry_type, idempotency_key)
    WHERE idempotency_key IS NOT NULL;

COMMENT ON INDEX uq_ledger_idempotency_key_per_type IS '充值幂等：(entry_type, idempotency_key) 复合唯一；跨 entry_type 复用同字符串不会撞索引';


-- ----------------------------------------------------------------------------
-- 5. correlation_id 复合 UNIQUE：Reserve/Commit/Release/Refund 重试幂等
-- ----------------------------------------------------------------------------

CREATE UNIQUE INDEX uq_ledger_correlation_per_type
    ON business_account_ledger (business_account_id, correlation_id, entry_type)
    WHERE correlation_id <> '';

COMMENT ON INDEX uq_ledger_correlation_per_type IS '业务流幂等：(account, correlation_id, entry_type) 三元组；空字符串 correlation_id 不约束';


-- ----------------------------------------------------------------------------
-- 6. reference 反查索引
-- ----------------------------------------------------------------------------

CREATE INDEX idx_ledger_reference
    ON business_account_ledger (reference_type, reference_id)
    WHERE reference_type IS NOT NULL;


-- ----------------------------------------------------------------------------
-- 7. Entry-level CHECK：按 entry_type 校验 delta 组合守恒
--    防 service bug 写出非守恒 entry（一旦写入因 trigger 不可改）
-- ----------------------------------------------------------------------------

ALTER TABLE business_account_ledger
    ADD CONSTRAINT chk_ledger_delta_by_type CHECK (
        CASE entry_type
            WHEN 'recharge' THEN
                -- 充值：仅增 available + 增 recharge_total（balance 表维护，不进 delta）
                available_delta >= 0
                AND reserved_delta = 0
                AND used_delta = 0
            WHEN 'reserve' THEN
                -- 预占：available 减 = reserved 增
                available_delta <= 0
                AND reserved_delta >= 0
                AND used_delta = 0
                AND available_delta + reserved_delta = 0
            WHEN 'commit' THEN
                -- 结算：reserved 减 = used 增
                available_delta = 0
                AND reserved_delta <= 0
                AND used_delta >= 0
                AND reserved_delta + used_delta = 0
            WHEN 'release' THEN
                -- 释放：reserved 减 = available 增
                available_delta >= 0
                AND reserved_delta <= 0
                AND used_delta = 0
                AND available_delta + reserved_delta = 0
            WHEN 'refund' THEN
                -- 返还额度：used 减 = available 增
                available_delta >= 0
                AND reserved_delta = 0
                AND used_delta <= 0
                AND available_delta + used_delta = 0
            ELSE TRUE  -- cashout / recharge_reversal / adjust / expire 留 enum 占位，P0 service 不写
        END
    );

COMMENT ON CONSTRAINT chk_ledger_delta_by_type ON business_account_ledger IS 'entry-level 守恒校验；防 service bug 写非守恒 entry 污染 ledger（不可改）';


-- ----------------------------------------------------------------------------
-- 8. ledger 不可变 trigger：DB 层强制阻 UPDATE / DELETE / TRUNCATE
--    应用层约定不够（即便 service 不写 UPDATE，DBA 手工脚本 / 攻击者 / 误操作仍可破坏）
-- ----------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION ledger_immutable_violation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'business_account_ledger 不可变：禁止 % 操作（设计文档 §3ter.1 #5）', TG_OP;
END;
$$ LANGUAGE plpgsql;

COMMENT ON FUNCTION ledger_immutable_violation IS 'ledger 表不可变守门：所有 UPDATE/DELETE/TRUNCATE 都抛异常';

-- 行级：UPDATE / DELETE
CREATE TRIGGER ledger_prevent_update_or_delete
    BEFORE UPDATE OR DELETE ON business_account_ledger
    FOR EACH ROW EXECUTE FUNCTION ledger_immutable_violation();

-- 语句级：TRUNCATE（行 trigger 不覆盖此操作 / pass-2 sec sec-001）
CREATE TRIGGER ledger_prevent_truncate
    BEFORE TRUNCATE ON business_account_ledger
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_immutable_violation();


-- ----------------------------------------------------------------------------
-- 9. 双兜底：P0 阶段单一 connection 用户也明确 REVOKE 高危权限
--    P1 部署阶段会进一步做 DB role 分级（app vs reconciler vs migration）
-- ----------------------------------------------------------------------------

REVOKE TRUNCATE, DELETE, UPDATE ON business_account_ledger FROM PUBLIC;

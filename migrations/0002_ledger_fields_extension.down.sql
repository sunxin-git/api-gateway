-- ============================================================================
-- Migration 0002 down.sql：撤销 ledger / balance 字段扩展
-- ============================================================================
-- ⚠️ 警告（首行 DO 块硬守门 / pass-2 adv F2-08）：
--   生产环境 ledger 表有数据时**禁止**跑本 down 脚本。
--   理由：(a) DROP COLUMN 会丢失审计字段（actor_type/actor_id/delta/canonical_body_sha256）
--         (b) DROP CONSTRAINT 会让历史 entry 失去 守恒约束保护
--         (c) DROP TRIGGER 会让历史 entry 失去不可变保护
--   生产环境出问题应优先走「代码热修」而非 schema 回退。
--   本文件用 PG plpgsql DO 块在执行前检查 ledger 是否为空，非空直接 RAISE 拒绝。
-- ============================================================================


-- ----------------------------------------------------------------------------
-- 0. 守门：生产 ledger 有数据时拒绝执行
-- ----------------------------------------------------------------------------

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM business_account_ledger LIMIT 1) THEN
        RAISE EXCEPTION 'Migration 0002 down 禁止：business_account_ledger 表已有数据。生产 rollback 走代码热修，不要回退 schema（详见 docs/plans/2026-05-26-002-...md Operational Notes）';
    END IF;
END $$;


-- ----------------------------------------------------------------------------
-- 1. 撤销权限 REVOKE（恢复 default）
-- ----------------------------------------------------------------------------

GRANT TRUNCATE, DELETE, UPDATE ON business_account_ledger TO PUBLIC;


-- ----------------------------------------------------------------------------
-- 2. 撤销 trigger
-- ----------------------------------------------------------------------------

DROP TRIGGER IF EXISTS ledger_prevent_truncate          ON business_account_ledger;
DROP TRIGGER IF EXISTS ledger_prevent_update_or_delete  ON business_account_ledger;
DROP FUNCTION IF EXISTS ledger_immutable_violation;


-- ----------------------------------------------------------------------------
-- 3. 撤销 CHECK 约束
-- ----------------------------------------------------------------------------

ALTER TABLE business_account_ledger
    DROP CONSTRAINT IF EXISTS chk_ledger_delta_by_type,
    DROP CONSTRAINT IF EXISTS chk_ledger_canonical_body_hash_len;


-- ----------------------------------------------------------------------------
-- 4. 撤销新增索引
-- ----------------------------------------------------------------------------

DROP INDEX IF EXISTS idx_ledger_reference;
DROP INDEX IF EXISTS uq_ledger_correlation_per_type;
DROP INDEX IF EXISTS uq_ledger_idempotency_key_per_type;


-- ----------------------------------------------------------------------------
-- 5. 恢复旧的全表 idempotency_key UNIQUE（与 0001 init 一致）
-- ----------------------------------------------------------------------------

CREATE UNIQUE INDEX uq_ledger_idempotency_key
    ON business_account_ledger (idempotency_key)
    WHERE idempotency_key IS NOT NULL;


-- ----------------------------------------------------------------------------
-- 6. 撤销 balance 字段
-- ----------------------------------------------------------------------------

ALTER TABLE business_account_balance
    DROP COLUMN IF EXISTS last_ledger_id;


-- ----------------------------------------------------------------------------
-- 7. 撤销 ledger 字段（反向依赖顺序）
-- ----------------------------------------------------------------------------

ALTER TABLE business_account_ledger
    DROP COLUMN IF EXISTS canonical_body_sha256,
    DROP COLUMN IF EXISTS actor_id,
    DROP COLUMN IF EXISTS actor_type,
    DROP COLUMN IF EXISTS metadata,
    DROP COLUMN IF EXISTS reference_id,
    DROP COLUMN IF EXISTS reference_type,
    DROP COLUMN IF EXISTS used_delta,
    DROP COLUMN IF EXISTS reserved_delta,
    DROP COLUMN IF EXISTS available_delta;


-- ----------------------------------------------------------------------------
-- 8. 撤销枚举类型
-- ----------------------------------------------------------------------------

DROP TYPE IF EXISTS actor_type;

-- ============================================================================
-- Migration 0003 DOWN: 回滚 Admin Token 阀门基础设施
-- ----------------------------------------------------------------------------
-- 顺序与 up 相反：先 DROP 子表 → 再 DROP ALTER 加的列
-- ============================================================================

-- 删除独立表（无依赖，可直接 DROP）
DROP TABLE IF EXISTS gateway_admin_token_circuit;
DROP TABLE IF EXISTS gateway_admin_token_usage;

-- 删 ALTER 加的列与约束（PG 自动级联删除关联的 CHECK）
ALTER TABLE gateway_admin_token
    DROP COLUMN IF EXISTS daily_refund_quota_limit,
    DROP COLUMN IF EXISTS single_refund_max;

-- token_hash COMMENT 不回滚（修订是 Phase 1 笔误，回滚到笔误版本没意义）

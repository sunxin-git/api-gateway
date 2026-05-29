-- ============================================================================
-- Migration 0005 DOWN: 回滚异步视频中继数据层
-- ----------------------------------------------------------------------------
-- 删两张新表 + task 的两列。task_status 枚举不在本文件改动（见 0006/0007 的 down）。
-- ============================================================================

DROP TABLE IF EXISTS business_account_model_entitlement;
DROP TABLE IF EXISTS account_model_concurrency;

ALTER TABLE task
    DROP COLUMN IF EXISTS upstream_submitted_at,
    DROP COLUMN IF EXISTS callback_token;

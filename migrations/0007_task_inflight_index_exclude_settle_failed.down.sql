-- ============================================================================
-- Migration 0007 DOWN: 还原 idx_task_inflight 至 0001 谓词（不含 SETTLE_FAILED）
-- ----------------------------------------------------------------------------
-- SETTLE_FAILED 枚举值由 0006 保留（PG 不可删值），但本索引谓词还原为原 5 终态排除。
-- ============================================================================

DROP INDEX IF EXISTS idx_task_inflight;

CREATE INDEX idx_task_inflight
    ON task (business_account_id, status)
    WHERE status NOT IN ('COMPLETED', 'FAILED', 'CANCELLED', 'EXPIRED', 'SETTLED');

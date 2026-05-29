-- ============================================================================
-- Migration 0007: 重建 idx_task_inflight，把 SETTLE_FAILED 纳入终态排除谓词
-- ----------------------------------------------------------------------------
-- 计划：docs/plans/2026-05-28-001-feat-async-video-relay-mvp-plan.md Unit 2
--
-- 必须在 0006（ADD VALUE）之后的独立文件：PG 不能在加值的同事务里使用新枚举值，
-- 而本索引的部分谓词 WHERE ... SETTLE_FAILED 会"使用"它。
--
-- idx_task_inflight 仅供 reconciler 扫卡住任务 / metrics 聚合（非 R15 cap 计数——cap 走
-- account_model_concurrency，见 ADR-0006 决策 2）。SETTLE_FAILED 是终态，须排除在"在途"之外。
-- ============================================================================

DROP INDEX IF EXISTS idx_task_inflight;

CREATE INDEX idx_task_inflight
    ON task (business_account_id, status)
    WHERE status NOT IN ('COMPLETED', 'FAILED', 'CANCELLED', 'EXPIRED', 'SETTLED', 'SETTLE_FAILED');

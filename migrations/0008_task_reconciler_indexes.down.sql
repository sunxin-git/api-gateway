-- ============================================================================
-- Migration 0008 DOWN: 删 task reconciler / 回调热路径索引
-- ============================================================================

DROP INDEX IF EXISTS idx_task_expirable;
DROP INDEX IF EXISTS idx_task_submitted_no_job;
DROP INDEX IF EXISTS idx_task_stuck_upstream_submitted;
DROP INDEX IF EXISTS idx_task_upstream_task_id;

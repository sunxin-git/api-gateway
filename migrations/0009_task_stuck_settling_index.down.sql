-- ============================================================================
-- Migration 0009 DOWN: 删 task stuck-SETTLING reconciler 索引
-- ============================================================================

DROP INDEX IF EXISTS idx_task_stuck_settling;

-- ============================================================================
-- Migration 0009: task stuck-SETTLING reconciler 索引（Phase 2 Unit 6b）
-- ----------------------------------------------------------------------------
-- 计划：docs/plans/2026-05-28-001-feat-async-video-relay-mvp-plan.md Unit 6
-- 来源：Unit 6b——fetch reconciler 新增 ScanStuckSettling 扫描需配套索引。
--
-- 现有 task reconciler 索引（0007/0008）：
--   idx_task_inflight (business_account_id, status) WHERE 非终态（不含 updated_at，扫 SETTLING 不优）
--   idx_task_stuck_upstream_submitted / idx_task_submitted_no_job / idx_task_expirable
-- 本次补 1 个：扫 SETTLING 且 updated_at 超时（硬崩溃于结算落账后、终态 CAS 前的卡住任务）。
--
-- 锁说明：CREATE INDEX（非 CONCURRENTLY）构建期持 task 表 ShareLock 阻塞写；golang-migrate
--   每文件单事务，事务内不可用 CONCURRENTLY。task 表当前行少，开发/首次部署无忧；
--   未来大表重建须用 migrate -x（去事务）+ CREATE INDEX CONCURRENTLY（见 schema.md 操作纪律）。
-- ============================================================================

CREATE INDEX idx_task_stuck_settling
    ON task (updated_at)
    WHERE status = 'SETTLING';

COMMENT ON INDEX idx_task_stuck_settling IS 'fetch reconciler 扫 SETTLING 超时（硬崩溃于结算落账后、终态 CAS 前的卡住任务）';

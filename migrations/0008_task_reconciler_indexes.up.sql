-- ============================================================================
-- Migration 0008: task 异步 reconciler / 回调热路径索引（Phase 2 Unit 3 评审补充）
-- ----------------------------------------------------------------------------
-- 计划：docs/plans/2026-05-28-001-feat-async-video-relay-mvp-plan.md Unit 2/6
-- 来源：ce-review（performance + data-migrations 一致发现）—— Unit 2 新增的查询缺索引。
--
-- 现有索引（0001/0007）：
--   idx_task_inflight (business_account_id, status) WHERE 非终态（含 SETTLE_FAILED 排除）
--   idx_task_submit_recover (status, submit_locked_until) WHERE status='UPSTREAM_SUBMITTING'
-- 本次补 4 个，覆盖 task.sql 的回调反查与各 reconciler scan，避免任务表增长后全表扫描：
--   1. GetTaskByUpstreamTaskID  —— 回调入口 + fetch reconciler 按上游 id 反查（同步热路径）
--   2. ScanStuckUpstreamSubmitted —— fetch reconciler 扫 UPSTREAM_SUBMITTED 超时
--   3. ScanSubmittedNoJob       —— reconciler 扫 SUBMITTED 滞留（入队丢失）
--   4. ScanExpirableTasks       —— expire worker 扫三态超最长执行期
--
-- 锁说明：CREATE INDEX（非 CONCURRENTLY）在构建期持 task 表 ShareLock，阻塞写。
--   golang-migrate 每文件单事务，事务内不可用 CONCURRENTLY。task 表当前为新表（行少），
--   开发/首次部署无忧；**未来大表重建须用 migrate -x（去事务）+ CREATE INDEX CONCURRENTLY**
--   （见 docs/db/schema.md migration 操作纪律）。
-- ============================================================================

-- 1. 回调入口 / fetch reconciler：按上游 task_id 反查（部分索引，仅非空）
CREATE INDEX idx_task_upstream_task_id
    ON task (upstream_task_id)
    WHERE upstream_task_id IS NOT NULL;

-- 2. fetch reconciler：扫 UPSTREAM_SUBMITTED 且 upstream_submitted_at 超时
CREATE INDEX idx_task_stuck_upstream_submitted
    ON task (upstream_submitted_at)
    WHERE status = 'UPSTREAM_SUBMITTED';

-- 3. reconciler：扫 SUBMITTED 滞留（无 Asynq job）
CREATE INDEX idx_task_submitted_no_job
    ON task (submitted_at)
    WHERE status = 'SUBMITTED';

-- 4. expire worker：扫三态超最长执行期（部分索引谓词内联 OR）
CREATE INDEX idx_task_expirable
    ON task (submitted_at)
    WHERE status IN ('SUBMITTED', 'UPSTREAM_SUBMITTING', 'UPSTREAM_SUBMITTED');

COMMENT ON INDEX idx_task_upstream_task_id          IS '回调入口 + fetch reconciler 按上游 task_id 反查';
COMMENT ON INDEX idx_task_stuck_upstream_submitted  IS 'fetch reconciler 扫 UPSTREAM_SUBMITTED 超时';
COMMENT ON INDEX idx_task_submitted_no_job          IS 'reconciler 扫 SUBMITTED 滞留（入队丢失）';
COMMENT ON INDEX idx_task_expirable                 IS 'expire worker 扫三态超最长执行期';

-- task.sql —— 异步任务 CRUD + 状态机 CAS + recover/reconciler 扫描 + R15 并发计数行
--
-- 设计文档：docs/multimedia-gateway-design.md §9 / §9ter / §9.5
-- 实施计划：docs/plans/2026-05-28-001-feat-async-video-relay-mvp-plan.md Unit 2（供 Unit 6/8）
-- 决策：ADR-0006（状态机 CAS 必带 from 条件；R15 走 account_model_concurrency 原子 claim）
--
-- 硬约束（CLAUDE.md 状态机模式）：所有状态变更**只**走带 from 条件的 CAS（:execrows，
--   受影响 0 行 = CAS 失败 = 状态已被他人推进）；**禁止**裸 UPDATE ... WHERE id 不带 status。


-- ============================================================================
-- 1. CRUD
-- ============================================================================


-- name: InsertTask :one
-- 提交流程落库（status 默认 SUBMITTED）；token_id / channel_id / callback_token 可空。
INSERT INTO task (
    id, business_account_id, token_id, channel_id,
    provider_type, model, financial_snapshot, accounting_month,
    callback_token
)
VALUES (
    @id, @business_account_id, sqlc.narg('token_id'), sqlc.narg('channel_id'),
    @provider_type, @model, @financial_snapshot, @accounting_month,
    sqlc.narg('callback_token')
)
RETURNING *;


-- name: GetTaskByID :one
-- 内部按 id 查任务（worker / reconciler 用，不做归属过滤）。
SELECT * FROM task WHERE id = @id;


-- name: GetTaskForAccount :one
-- 业务侧只读查询：强制归属校验（id + business_account_id）。
-- 不匹配返 0 rows → handler 返 404（跨租户不可枚举，符合 CLAUDE.md ISP / 失败优先）。
SELECT * FROM task
WHERE id = @id
  AND business_account_id = @business_account_id;


-- name: GetTaskByUpstreamTaskID :one
-- 按上游 task_id 反查本地任务（回调入口 / fetch reconciler 用）。
SELECT * FROM task WHERE upstream_task_id = @upstream_task_id;


-- ============================================================================
-- 2. 状态机 CAS（显式 from → to；:execrows 判成败）
-- ============================================================================


-- name: CompareAndSwapTaskStatus :execrows
-- 通用状态 CAS：仅当当前 status = @from_status 时推进到 @to_status。
-- 用于无附带字段的转移（如 UPSTREAM_SUBMITTED→SETTLING、SETTLING→SETTLED/SETTLE_FAILED）。
UPDATE task
SET status     = @to_status,
    updated_at = NOW()
WHERE id = @id
  AND status = @from_status;


-- name: MarkTaskSubmitting :execrows
-- CAS SUBMITTED → UPSTREAM_SUBMITTING，并写入 worker 抢占 lease（submit_locked_*）。
-- 仅一个 worker 能 CAS 成功；其余受影响 0 行放弃。
UPDATE task
SET status              = 'UPSTREAM_SUBMITTING',
    submit_locked_until = @submit_locked_until,
    submit_locked_by    = @submit_locked_by,
    updated_at          = NOW()
WHERE id = @id
  AND status = 'SUBMITTED';


-- name: MarkTaskUpstreamSubmitted :execrows
-- CAS UPSTREAM_SUBMITTING → UPSTREAM_SUBMITTED，同步持久化 upstream_task_id + 提交时刻。
-- 这是「上游 Submit 成功 → 存 upstream_task_id」的原子落点（ADR-0006 决策 5 双提交防护核心）。
UPDATE task
SET status                = 'UPSTREAM_SUBMITTED',
    upstream_task_id      = @upstream_task_id,
    upstream_submitted_at = NOW(),
    updated_at            = NOW()
WHERE id = @id
  AND status = 'UPSTREAM_SUBMITTING';


-- name: MarkTaskUpstreamTerminal :execrows
-- CAS 进入上游终态（COMPLETED/FAILED/CANCELLED/EXPIRED），写 terminal_at + 错误信息，
-- 并**置空 callback_token**（终态后不再接受回调，防 token 泄露后被滥用，ADR-0006 决策 5）。
-- 调用方负责在同事务内释放并发 claim（ReleaseConcurrencySlot）。
UPDATE task
SET status        = @to_status,
    terminal_at   = NOW(),
    error_code    = sqlc.narg('error_code'),
    error_message = sqlc.narg('error_message'),
    callback_token = NULL,
    updated_at    = NOW()
WHERE id = @id
  AND status = @from_status;


-- ============================================================================
-- 3. recover / reconciler 扫描 + 在途计数（metrics / 兜底，非 R15 cap）
-- ============================================================================


-- name: CountInflightByAccountModel :one
-- 统计某 (account, model) 当前在途任务数（reconciler 对账 / metrics 用）。
-- **非** R15 cap 计数——cap 由 account_model_concurrency 原子 claim 承载（ADR-0006 决策 2）。
SELECT count(*) FROM task
WHERE business_account_id = @business_account_id
  AND model = @model
  AND status IN ('SUBMITTED', 'UPSTREAM_SUBMITTING', 'UPSTREAM_SUBMITTED');


-- name: ScanRecoverableTasks :many
-- recover worker：扫 UPSTREAM_SUBMITTING 且 lease 过期的任务（崩溃在提交窗口）。
-- ADR-0006 决策 5：上游无幂等键/不可反查 → 这些任务 fail-closed（不自动重投），
-- 调用方据此 CAS→FAILED + release + 告警。用 idx_task_submit_recover。
SELECT * FROM task
WHERE status = 'UPSTREAM_SUBMITTING'
  AND submit_locked_until < @now
ORDER BY submit_locked_until
LIMIT @batch_size;


-- name: ScanStuckUpstreamSubmitted :many
-- fetch reconciler：扫 UPSTREAM_SUBMITTED 超时未终态的任务 → 主动 Poll 上游兜底。
SELECT * FROM task
WHERE status = 'UPSTREAM_SUBMITTED'
  AND upstream_submitted_at < @threshold
ORDER BY upstream_submitted_at
LIMIT @batch_size;


-- name: ScanSubmittedNoJob :many
-- reconciler：扫 SUBMITTED 滞留超阈值（入队丢失 / Redis 抖动导致无 Asynq job）→ 幂等重投。
SELECT * FROM task
WHERE status = 'SUBMITTED'
  AND submitted_at < @threshold
ORDER BY submitted_at
LIMIT @batch_size;


-- name: ScanExpirableTasks :many
-- expire worker：扫上游侧仍在途但已超最长执行期的任务 → CAS→EXPIRED + release 兜底。
SELECT * FROM task
WHERE status IN ('SUBMITTED', 'UPSTREAM_SUBMITTING', 'UPSTREAM_SUBMITTED')
  AND submitted_at < @threshold
ORDER BY submitted_at
LIMIT @batch_size;


-- ============================================================================
-- 4. account_model_concurrency（R15 并发硬上限：原子 claim / release）
-- ============================================================================


-- name: ClaimConcurrencySlot :one
-- R15 原子占位（提交前，与 task 落库同事务）：每 (account, model) 计数行 +1，
-- 仅当 inflight < @cap_limit。首次用 ON CONFLICT lazy upsert。
-- 返回新 inflight；**返 0 行（pgx.ErrNoRows）= 占不到 = 调用方返 429**。
-- 用条件 UPSERT（单语句原子，PG 行锁串行化），杜绝 count-then-act 的幻读 TOCTOU。
-- **cap=0 守卫**：INSERT...SELECT 的 `WHERE @cap_limit >= 1` 让首次插入路径也受 cap 约束——
--   否则「行不存在 + cap=0」会绕过 ON CONFLICT 的 WHERE（仅作用于 DO UPDATE 分支）
--   直接插入 inflight=1，使「禁用模型(cap=0)」失效（评审 #1）。
INSERT INTO account_model_concurrency AS amc (business_account_id, model, inflight, updated_at)
SELECT @business_account_id, @model, 1, NOW()
WHERE @cap_limit::int >= 1
ON CONFLICT (business_account_id, model) DO UPDATE
    SET inflight   = amc.inflight + 1,
        updated_at = NOW()
    WHERE amc.inflight < @cap_limit
RETURNING inflight;


-- name: ReleaseConcurrencySlot :execrows
-- 释放并发槽（进上游终态 CAS 赢家同事务内调用）：inflight -1，永不为负（防 double-release）。
UPDATE account_model_concurrency
SET inflight   = inflight - 1,
    updated_at = NOW()
WHERE business_account_id = @business_account_id
  AND model = @model
  AND inflight > 0;


-- name: GetConcurrency :one
-- 读某 (account, model) 当前 inflight（admin / metrics）。
SELECT * FROM account_model_concurrency
WHERE business_account_id = @business_account_id
  AND model = @model;

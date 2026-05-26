-- balance.sql —— 账户余额投影读取
--
-- 写路径由 ledger.sql 的 CTE 单语句完成（INSERT ledger + UPDATE balance）；
-- 本文件仅承载读路径 + rebuild 专用的 ReplaceBalance（绕过 CAS 的写）。


-- name: GetBalance :one
-- 标准读取：用于 service.GetBalance + admin-cli 反查 + 测试断言。
SELECT business_account_id, available, reserved, used_total, recharge_total,
       refund_total, version, frozen, frozen_reason, frozen_at, updated_at, last_ledger_id
FROM business_account_balance
WHERE business_account_id = @business_account_id;


-- name: GetBalanceInTx :one
-- 事务内读取：与 GetBalance 等价，但显式由 service 在 CAS 失败后的同一 tx 调用。
-- 保留独立 query 命名，便于 grep 出「事务内读」语义。
SELECT business_account_id, available, reserved, used_total, recharge_total,
       refund_total, version, frozen, frozen_reason, frozen_at, updated_at, last_ledger_id
FROM business_account_balance
WHERE business_account_id = @business_account_id;


-- name: GetBalanceForUpdate :one
-- 行级 FOR UPDATE 读：rebuild TX3 用，防并发 Commit/Release 写入。
SELECT business_account_id, available, reserved, used_total, recharge_total,
       refund_total, version, frozen, frozen_reason, frozen_at, updated_at, last_ledger_id
FROM business_account_balance
WHERE business_account_id = @business_account_id
FOR UPDATE;


-- name: ListAllUnfrozenAccountsForReconciler :many
-- reconciler 全表扫起点：仅取 ID + 当前 version；后续读 SUM 时再单只读 tx 中拉一致快照。
SELECT business_account_id, version
FROM business_account_balance
WHERE frozen = false
ORDER BY business_account_id ASC;


-- name: ListStuckRebuildAccounts :many
-- reconciler 收尾 watchdog：扫出处于 rebuild_in_progress 状态且 frozen 时间过长的账户。
--
-- 用途：U8 RebuildBalance 三事务流程中第二阶段崩溃 / 进程被 kill 时，账户会停留在
-- frozen=true + reason 含 'rebuild_in_progress' 状态；本查询给 reconciler 暴露 metric
-- 让运维及时发现并人工恢复（pass-2 sec sec-pass2-008）。
--
-- @threshold 参数取自 reconciler.rebuildStuckThreshold（默认 10 分钟）。
SELECT business_account_id, frozen_at
FROM business_account_balance
WHERE frozen = true
  AND frozen_reason LIKE '%rebuild_in_progress%'
  AND frozen_at IS NOT NULL
  AND frozen_at < (NOW() - @threshold::interval);


-- name: ReplaceBalance :execrows
-- rebuild TX3 专用：用 last_ledger_id CAS 校验「TX2 读快照后没有新 entry 写入」；
-- 命中行数 = 0 表示并发漂移，调用方应回到 TX2 重读快照。
-- 不参与日常写路径，绕开 version CAS（rebuild 期间 frozen=true 已阻新业务写）。
UPDATE business_account_balance
SET available      = @available,
    reserved       = @reserved,
    used_total     = @used_total,
    recharge_total = @recharge_total,
    refund_total   = @refund_total,
    version        = version + 1,
    updated_at     = NOW()
WHERE business_account_id = @business_account_id
  AND last_ledger_id     = @expected_last_ledger_id;

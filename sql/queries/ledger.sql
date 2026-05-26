-- ledger.sql —— 账本流水写入 + 幂等查询 + drift 聚合 + rebuild 全量读
--
-- 设计文档：docs/multimedia-gateway-design.md §3ter
-- 实施计划：docs/plans/2026-05-26-002-...-plan.md Unit 2
--
-- 写操作统一走 PG CTE 单语句模式：
--   WITH new_entry AS (INSERT INTO business_account_ledger ... RETURNING ...)
--   UPDATE business_account_balance SET ... WHERE CAS 条件 RETURNING ...
--
-- 关键 PG 语义（务必牢记）：
--   data-modifying CTE 即使最终 SELECT 0 行，sibling INSERT 仍**执行到完成**。
--   原子性靠 tx 级 ROLLBACK 保证 —— service 在 pgx.ErrNoRows 时**必须**显式 tx.Rollback。
--
-- sqlc 命名参数注意：
--   sqlc v1.30.0 对 `-@param`（紧贴一元负号）解析有 bug，会把 `@param` 留在生成 SQL 中。
--   解决：所有负值表达式写成 `(0 - @param)::bigint`，sqlc 能正确替换为 `(0 - $N)::bigint`。
--
-- CAS 字段策略：
--   - Recharge / Reserve   含 frozen = false（拒绝冻结账户的新入金 / 预占）
--   - Commit / Release / Refund / Freeze / Unfreeze   不查 frozen（允许 inflight 完成 + 管理动作）
--   - 所有写都含 version = @expected_version 防丢失更新


-- ============================================================================
-- 1. 写操作：CTE 单语句（INSERT ledger + UPDATE balance）
-- ============================================================================


-- name: RechargeAtomic :one
-- 充值：available 增 + recharge_total 增；CAS WHERE frozen=false AND version=?
-- 0 行 = CAS 失败（frozen / version 冲突）；service 同 tx 内 fresh-read 判错。
WITH new_entry AS (
    INSERT INTO business_account_ledger (
        business_account_id, entry_type, amount,
        available_delta, reserved_delta, used_delta,
        correlation_id, idempotency_key, snapshot,
        reference_type, reference_id, metadata,
        actor_type, actor_id, canonical_body_sha256
    )
    VALUES (
        @business_account_id, 'recharge', @amount,
        @amount, 0, 0,
        @correlation_id, sqlc.narg('idempotency_key')::text, @snapshot,
        sqlc.narg('reference_type')::text, sqlc.narg('reference_id')::text, @metadata,
        @actor_type, @actor_id, sqlc.narg('canonical_body_sha256')::bytea
    )
    RETURNING id, created_at
),
updated_balance AS (
    UPDATE business_account_balance
    SET available      = available + @amount,
        recharge_total = recharge_total + @amount,
        last_ledger_id = (SELECT id FROM new_entry),
        version        = version + 1,
        updated_at     = NOW()
    WHERE business_account_id = @business_account_id
      AND version             = @expected_version
      AND frozen              = false
    RETURNING business_account_id, available, reserved, used_total, recharge_total,
              refund_total, version, frozen, frozen_reason, frozen_at, updated_at, last_ledger_id
)
SELECT
    (SELECT id FROM new_entry)                AS new_ledger_id,
    (SELECT created_at FROM new_entry)        AS new_created_at,
    ub.business_account_id,
    ub.available,
    ub.reserved,
    ub.used_total,
    ub.recharge_total,
    ub.refund_total,
    ub.version,
    ub.frozen,
    ub.frozen_reason,
    ub.frozen_at,
    ub.updated_at,
    ub.last_ledger_id
FROM updated_balance ub;


-- name: ReserveAtomic :one
-- 预占：available 减 + reserved 增；CAS 含 available >= @amount AND frozen=false AND version=?
-- available_delta 用 (0::bigint - @amount)::bigint 表达负值（sqlc 解析必须）。
WITH new_entry AS (
    INSERT INTO business_account_ledger (
        business_account_id, entry_type, amount,
        available_delta, reserved_delta, used_delta,
        correlation_id, snapshot,
        reference_type, reference_id, metadata,
        actor_type, actor_id
    )
    VALUES (
        @business_account_id, 'reserve', @amount,
        (0::bigint - @amount)::bigint, @amount, 0,
        @correlation_id, @snapshot,
        sqlc.narg('reference_type')::text, sqlc.narg('reference_id')::text, @metadata,
        @actor_type, @actor_id
    )
    RETURNING id, created_at
),
updated_balance AS (
    UPDATE business_account_balance
    SET available  = available - @amount,
        reserved   = reserved + @amount,
        last_ledger_id = (SELECT id FROM new_entry),
        version    = version + 1,
        updated_at = NOW()
    WHERE business_account_id = @business_account_id
      AND version             = @expected_version
      AND frozen              = false
      AND available           >= @amount
    RETURNING business_account_id, available, reserved, used_total, recharge_total,
              refund_total, version, frozen, frozen_reason, frozen_at, updated_at, last_ledger_id
)
SELECT
    (SELECT id FROM new_entry)                AS new_ledger_id,
    (SELECT created_at FROM new_entry)        AS new_created_at,
    ub.business_account_id,
    ub.available,
    ub.reserved,
    ub.used_total,
    ub.recharge_total,
    ub.refund_total,
    ub.version,
    ub.frozen,
    ub.frozen_reason,
    ub.frozen_at,
    ub.updated_at,
    ub.last_ledger_id
FROM updated_balance ub;


-- name: CommitWithReleaseAtomic :one
-- 结算（含部分释放残余）：actualCost < reserveAmount 时用本 query。
-- 一次性 INSERT commit + release 两条 entry + UPDATE balance。
-- CAS：不查 frozen；含 reserved >= @reserve_amount AND version=?
--
-- 参数：
--   @reserve_amount, @actual_cost, @release_amount = reserve_amount - actual_cost
--   @release_correlation_id = correlation_id + ":release"
--
-- 返回 commit + release 两条 ledger 信息 + 新 balance。
WITH commit_entry AS (
    INSERT INTO business_account_ledger (
        business_account_id, entry_type, amount,
        available_delta, reserved_delta, used_delta,
        correlation_id, snapshot,
        reference_type, reference_id, metadata,
        actor_type, actor_id
    )
    VALUES (
        @business_account_id, 'commit', @actual_cost,
        0, (0::bigint - @actual_cost)::bigint, @actual_cost,
        @correlation_id, @snapshot,
        sqlc.narg('reference_type')::text, sqlc.narg('reference_id')::text, @metadata,
        @actor_type, @actor_id
    )
    RETURNING id, created_at
),
release_entry AS (
    INSERT INTO business_account_ledger (
        business_account_id, entry_type, amount,
        available_delta, reserved_delta, used_delta,
        correlation_id, snapshot,
        reference_type, reference_id, metadata,
        actor_type, actor_id
    )
    VALUES (
        @business_account_id, 'release', @release_amount,
        @release_amount, (0::bigint - @release_amount)::bigint, 0,
        @release_correlation_id, @snapshot,
        sqlc.narg('reference_type')::text, sqlc.narg('reference_id')::text, @metadata,
        @actor_type, @actor_id
    )
    RETURNING id, created_at
),
updated_balance AS (
    UPDATE business_account_balance
    SET available      = available + @release_amount,
        reserved       = reserved - @reserve_amount,
        used_total     = used_total + @actual_cost,
        last_ledger_id = (SELECT id FROM release_entry),
        version        = version + 1,
        updated_at     = NOW()
    WHERE business_account_id = @business_account_id
      AND version             = @expected_version
      AND reserved            >= @reserve_amount
    RETURNING business_account_id, available, reserved, used_total, recharge_total,
              refund_total, version, frozen, frozen_reason, frozen_at, updated_at, last_ledger_id
)
SELECT
    (SELECT id FROM commit_entry)          AS commit_ledger_id,
    (SELECT created_at FROM commit_entry)  AS commit_created_at,
    (SELECT id FROM release_entry)         AS release_ledger_id,
    (SELECT created_at FROM release_entry) AS release_created_at,
    ub.business_account_id,
    ub.available,
    ub.reserved,
    ub.used_total,
    ub.recharge_total,
    ub.refund_total,
    ub.version,
    ub.frozen,
    ub.frozen_reason,
    ub.frozen_at,
    ub.updated_at,
    ub.last_ledger_id
FROM updated_balance ub;


-- name: CommitAtomic :one
-- 结算（无残余，actualCost == reserveAmount）：只 INSERT commit + UPDATE balance。
-- 与 CommitWithReleaseAtomic 同语义，但少一条 release entry，sqlc 类型推导更干净。
WITH new_entry AS (
    INSERT INTO business_account_ledger (
        business_account_id, entry_type, amount,
        available_delta, reserved_delta, used_delta,
        correlation_id, snapshot,
        reference_type, reference_id, metadata,
        actor_type, actor_id
    )
    VALUES (
        @business_account_id, 'commit', @actual_cost,
        0, (0::bigint - @actual_cost)::bigint, @actual_cost,
        @correlation_id, @snapshot,
        sqlc.narg('reference_type')::text, sqlc.narg('reference_id')::text, @metadata,
        @actor_type, @actor_id
    )
    RETURNING id, created_at
),
updated_balance AS (
    UPDATE business_account_balance
    SET reserved       = reserved - @reserve_amount,
        used_total     = used_total + @actual_cost,
        last_ledger_id = (SELECT id FROM new_entry),
        version        = version + 1,
        updated_at     = NOW()
    WHERE business_account_id = @business_account_id
      AND version             = @expected_version
      AND reserved            >= @reserve_amount
    RETURNING business_account_id, available, reserved, used_total, recharge_total,
              refund_total, version, frozen, frozen_reason, frozen_at, updated_at, last_ledger_id
)
SELECT
    (SELECT id FROM new_entry)         AS new_ledger_id,
    (SELECT created_at FROM new_entry) AS new_created_at,
    ub.business_account_id,
    ub.available,
    ub.reserved,
    ub.used_total,
    ub.recharge_total,
    ub.refund_total,
    ub.version,
    ub.frozen,
    ub.frozen_reason,
    ub.frozen_at,
    ub.updated_at,
    ub.last_ledger_id
FROM updated_balance ub;


-- name: ReleaseAtomic :one
-- 释放预占：reserved 减 + available 增；CAS 含 reserved >= @amount AND version=?
-- 不查 frozen（允许 inflight 完成）。
WITH new_entry AS (
    INSERT INTO business_account_ledger (
        business_account_id, entry_type, amount,
        available_delta, reserved_delta, used_delta,
        correlation_id, snapshot,
        reference_type, reference_id, metadata,
        actor_type, actor_id
    )
    VALUES (
        @business_account_id, 'release', @amount,
        @amount, (0::bigint - @amount)::bigint, 0,
        @correlation_id, @snapshot,
        sqlc.narg('reference_type')::text, sqlc.narg('reference_id')::text, @metadata,
        @actor_type, @actor_id
    )
    RETURNING id, created_at
),
updated_balance AS (
    UPDATE business_account_balance
    SET available  = available + @amount,
        reserved   = reserved - @amount,
        last_ledger_id = (SELECT id FROM new_entry),
        version    = version + 1,
        updated_at = NOW()
    WHERE business_account_id = @business_account_id
      AND version             = @expected_version
      AND reserved            >= @amount
    RETURNING business_account_id, available, reserved, used_total, recharge_total,
              refund_total, version, frozen, frozen_reason, frozen_at, updated_at, last_ledger_id
)
SELECT
    (SELECT id FROM new_entry)                AS new_ledger_id,
    (SELECT created_at FROM new_entry)        AS new_created_at,
    ub.business_account_id,
    ub.available,
    ub.reserved,
    ub.used_total,
    ub.recharge_total,
    ub.refund_total,
    ub.version,
    ub.frozen,
    ub.frozen_reason,
    ub.frozen_at,
    ub.updated_at,
    ub.last_ledger_id
FROM updated_balance ub;


-- name: RefundAtomic :one
-- 退款：used_total 减 + available 增 + refund_total 增；CAS 含 used_total >= @amount AND version=?
-- 不查 frozen（管理动作允许）。refund_total 不进不变量等式，单独自增。
WITH new_entry AS (
    INSERT INTO business_account_ledger (
        business_account_id, entry_type, amount,
        available_delta, reserved_delta, used_delta,
        correlation_id, snapshot,
        reference_type, reference_id, metadata,
        actor_type, actor_id
    )
    VALUES (
        @business_account_id, 'refund', @amount,
        @amount, 0, (0::bigint - @amount)::bigint,
        @correlation_id, @snapshot,
        sqlc.narg('reference_type')::text, sqlc.narg('reference_id')::text, @metadata,
        @actor_type, @actor_id
    )
    RETURNING id, created_at
),
updated_balance AS (
    UPDATE business_account_balance
    SET available    = available + @amount,
        used_total   = used_total - @amount,
        refund_total = refund_total + @amount,
        last_ledger_id = (SELECT id FROM new_entry),
        version      = version + 1,
        updated_at   = NOW()
    WHERE business_account_id = @business_account_id
      AND version             = @expected_version
      AND used_total          >= @amount
    RETURNING business_account_id, available, reserved, used_total, recharge_total,
              refund_total, version, frozen, frozen_reason, frozen_at, updated_at, last_ledger_id
)
SELECT
    (SELECT id FROM new_entry)                AS new_ledger_id,
    (SELECT created_at FROM new_entry)        AS new_created_at,
    ub.business_account_id,
    ub.available,
    ub.reserved,
    ub.used_total,
    ub.recharge_total,
    ub.refund_total,
    ub.version,
    ub.frozen,
    ub.frozen_reason,
    ub.frozen_at,
    ub.updated_at,
    ub.last_ledger_id
FROM updated_balance ub;


-- name: FreezeAtomic :one
-- 冻结账户余额：CAS 含 frozen=false AND version=?
-- 已 frozen 返回 0 行；service 判幂等成功（不视为错误）。
-- 不写 ledger entry（frozen 是 balance 字段，不涉及金额流水）；调用方自行写 outbox。
UPDATE business_account_balance
SET frozen        = true,
    frozen_reason = @frozen_reason,
    frozen_at     = NOW(),
    version       = version + 1,
    updated_at    = NOW()
WHERE business_account_id = @business_account_id
  AND version             = @expected_version
  AND frozen              = false
RETURNING business_account_id, available, reserved, used_total, recharge_total,
          refund_total, version, frozen, frozen_reason, frozen_at, updated_at, last_ledger_id;


-- name: UnfreezeAtomic :one
-- 解冻：CAS 含 frozen=true AND version=?；frozen_reason 重写为新值（rebuild_completed / manual_unfreeze）。
-- 0 行 = 已 unfrozen（幂等成功）或 version 冲突。
UPDATE business_account_balance
SET frozen        = false,
    frozen_reason = @frozen_reason,
    frozen_at     = NULL,
    version       = version + 1,
    updated_at    = NOW()
WHERE business_account_id = @business_account_id
  AND version             = @expected_version
  AND frozen              = true
RETURNING business_account_id, available, reserved, used_total, recharge_total,
          refund_total, version, frozen, frozen_reason, frozen_at, updated_at, last_ledger_id;


-- ============================================================================
-- 2. 幂等查询
-- ============================================================================


-- name: FindLedgerEntryByIdempotencyKey :one
-- 充值幂等命中查询：service 命中后比对 canonical_body_sha256，相同返原 entry，不同 ErrIdempotencyConflict。
SELECT id, business_account_id, entry_type, amount,
       available_delta, reserved_delta, used_delta,
       correlation_id, idempotency_key, snapshot,
       reference_type, reference_id, metadata,
       actor_type, actor_id, canonical_body_sha256, created_at
FROM business_account_ledger
WHERE entry_type      = @entry_type
  AND idempotency_key = @idempotency_key;


-- name: FindLedgerEntryByCorrelationAndType :one
-- 通用反查：Reserve/Commit/Release/Refund 幂等用此 query 检查同 correlation_id+entry_type 是否已存在。
SELECT id, business_account_id, entry_type, amount,
       available_delta, reserved_delta, used_delta,
       correlation_id, idempotency_key, snapshot,
       reference_type, reference_id, metadata,
       actor_type, actor_id, canonical_body_sha256, created_at
FROM business_account_ledger
WHERE business_account_id = @business_account_id
  AND correlation_id      = @correlation_id
  AND entry_type          = @entry_type;


-- name: FindActiveReserveByCorrelation :one
-- Commit/Release 前置校验：找同 correlation_id 的 active reserve（未 commit / release）。
-- NOT EXISTS 子查询确认同 correlation_id 没有 commit/release entry。
SELECT id, business_account_id, entry_type, amount,
       available_delta, reserved_delta, used_delta,
       correlation_id, idempotency_key, snapshot,
       reference_type, reference_id, metadata,
       actor_type, actor_id, canonical_body_sha256, created_at
FROM business_account_ledger l
WHERE l.business_account_id = @business_account_id
  AND l.correlation_id      = @correlation_id
  AND l.entry_type          = 'reserve'
  AND NOT EXISTS (
      SELECT 1 FROM business_account_ledger s
      WHERE s.business_account_id = l.business_account_id
        AND s.correlation_id      = l.correlation_id
        AND s.entry_type IN ('commit', 'release')
  );


-- name: FindAnyReserveByCorrelation :one
-- 用于区分「未找到 reserve」与「reserve 已 commit/release」（service 用以返回不同 sentinel）。
SELECT id, business_account_id, entry_type, amount,
       available_delta, reserved_delta, used_delta,
       correlation_id, idempotency_key, snapshot,
       reference_type, reference_id, metadata,
       actor_type, actor_id, canonical_body_sha256, created_at
FROM business_account_ledger
WHERE business_account_id = @business_account_id
  AND correlation_id      = @correlation_id
  AND entry_type          = 'reserve';


-- ============================================================================
-- 3. drift reconciler 聚合
-- ============================================================================


-- name: SumLedgerDeltasByAccount :one
-- reconciler 用：对单个账户的全部 ledger entry SUM 各 delta，
-- 与 balance 表比对得 drift。recharge_sum / refund_sum 用 FILTER 仅累加对应 entry_type。
-- COALESCE 兜底 NULL（账户 0 条 ledger 时 SUM 返回 NULL）。
SELECT
    COALESCE(SUM(available_delta), 0)::bigint AS available_sum,
    COALESCE(SUM(reserved_delta),  0)::bigint AS reserved_sum,
    COALESCE(SUM(used_delta),      0)::bigint AS used_sum,
    COALESCE(SUM(amount) FILTER (WHERE entry_type = 'recharge'), 0)::bigint AS recharge_sum,
    COALESCE(SUM(amount) FILTER (WHERE entry_type = 'refund'),   0)::bigint AS refund_sum,
    COALESCE(MAX(id), 0)::bigint AS max_ledger_id
FROM business_account_ledger
WHERE business_account_id = @business_account_id;


-- ============================================================================
-- 4. rebuild 全量读
-- ============================================================================


-- name: GetLedgerEntriesForRebuild :many
-- rebuild TX2 用：按 id ASC 全量取 ledger，应用层累加得 expected balance。
-- ORDER BY id 保证回放顺序与原写入顺序一致（虽然累加是可交换的，方便审计）。
SELECT id, business_account_id, entry_type, amount,
       available_delta, reserved_delta, used_delta,
       correlation_id, idempotency_key, snapshot,
       reference_type, reference_id, metadata,
       actor_type, actor_id, canonical_body_sha256, created_at
FROM business_account_ledger
WHERE business_account_id = @business_account_id
ORDER BY id ASC;

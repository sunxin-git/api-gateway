-- business_account.sql —— 业务账户 CRUD
--
-- 适用范围：账户创建（同事务建 balance 行）/ 读取 / 入口 active 校验。
-- 实现层：internal/ledger/postgres.go CreateAccount + 通用查询。
--
-- 说明：CreateBusinessAccount 仅插入 business_account 主表；
--       同事务的 business_account_balance(zeros) 由调用方在同一 pgx.Tx 中
--       另起一条 INSERT（详见 ledger.sql 与 service 实现），便于复用 BalanceRow。


-- name: CreateBusinessAccount :one
-- 创建业务账户主记录；调用方负责在同一事务内插入 balance(zeros) 与 outbox 事件。
INSERT INTO business_account (
    id, status, isolation_required, metadata
) VALUES (
    @id, 'active', @isolation_required, @metadata
)
RETURNING id, status, isolation_required, break_glass_until, metadata, created_at, updated_at;


-- name: CreateBusinessAccountBalanceZero :one
-- 创建账户的零值 balance 行（与 CreateBusinessAccount 同事务）。
INSERT INTO business_account_balance (
    business_account_id,
    available, reserved, used_total, recharge_total, refund_total,
    version, frozen, last_ledger_id
) VALUES (
    @business_account_id,
    0, 0, 0, 0, 0,
    0, false, 0
)
RETURNING business_account_id, available, reserved, used_total, recharge_total,
          refund_total, version, frozen, frozen_reason, frozen_at, updated_at, last_ledger_id;


-- name: GetBusinessAccount :one
-- 读取业务账户主记录（用于反查 / 管理后台展示）。
SELECT id, status, isolation_required, break_glass_until, metadata, created_at, updated_at
FROM business_account
WHERE id = @id;


-- name: GetBusinessAccountForActiveCheck :one
-- 入口校验：仅返回 status + isolation_required，避免拉全字段；
-- service 层在写操作入口先调用此 query 判 status='active'。
SELECT status, isolation_required
FROM business_account
WHERE id = @id;

-- operator_account.sql —— 运维登录账户 CRUD + 认证热路径
--
-- 决策依据：ADR-0008（管理后台会话认证）
-- 实施计划：docs/plans/2026-05-31-001-feat-admin-console-config-plan.md Unit 2（供 Unit 3 operator service）
--
-- 安全约定：
--   - password_hash 是 bcrypt(口令) hex；明文绝不入库。
--   - **仅** GetOperatorAccountByUsername 返回 password_hash（认证用）；List / 其余视图绝不回显哈希。
--   - 认证由 service 层（Unit 3）用 bcrypt.CompareHashAndPassword 完成；本层只取哈希。


-- name: InsertOperatorAccount :one
-- 创建运维账户；password_hash 由 service 层 bcrypt 后传入。
-- created_by：初始管理员种子为 'seed'，后台开通为 'operator:<id>'。
-- username 唯一冲突 → service 层判 ErrUsernameExists（SQLSTATE 23505）。
INSERT INTO operator_account (username, password_hash, enabled, created_by)
VALUES (@username, @password_hash, @enabled, @created_by)
RETURNING id, username, enabled, created_by, created_at, updated_at;


-- name: GetOperatorAccountByUsername :one
-- 认证热路径：按用户名取账户（**含 password_hash**，仅认证用）。
-- 调用方校验 enabled 再比对口令（禁用账户拒登）。
SELECT id, username, password_hash, enabled, created_by, created_at, updated_at
FROM operator_account
WHERE username = @username;


-- name: GetOperatorAccountByID :one
-- 按 id 查账户（**不含 password_hash**；运维 / 会话归属校验用）。
SELECT id, username, enabled, created_by, created_at, updated_at
FROM operator_account
WHERE id = @id;


-- name: ListOperatorAccounts :many
-- 列出全部运维账户（**不含 password_hash**）；后台账户管理用。按 created_at DESC。
SELECT id, username, enabled, created_by, created_at, updated_at
FROM operator_account
ORDER BY created_at DESC;


-- name: CountOperatorAccounts :one
-- 计数；初始管理员种子幂等判定（表空才种子）。
SELECT COUNT(*) AS total FROM operator_account;


-- name: SetOperatorAccountEnabled :one
-- 启用 / 禁用账户（禁用即时阻断后续登录；已建会话由禁用账户校验失效）。
-- 不存在 id 返 0 rows → service 判 ErrNotFound。
UPDATE operator_account
SET enabled    = @enabled,
    updated_at = NOW()
WHERE id = @id
RETURNING id, username, enabled, created_by, created_at, updated_at;


-- name: UpdateOperatorPassword :execrows
-- 改口令（password_hash 由 service bcrypt 后传入）；返回受影响行数判断是否存在。
UPDATE operator_account
SET password_hash = @password_hash,
    updated_at    = NOW()
WHERE id = @id;

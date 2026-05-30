-- admin_session.sql —— 管理后台 PG 会话 CRUD + 鉴权热路径
--
-- 决策依据：ADR-0008（会话存 PG；存 token 的 HMAC 而非明文）
-- 实施计划：docs/plans/2026-05-31-001-feat-admin-console-config-plan.md Unit 2（供 Unit 4 会话中间件）
--
-- 安全约定：
--   - session_token_hash = HMAC(pepper, 会话明文 token) hex；明文只在 HttpOnly Cookie。
--   - 鉴权按 token_hash 单 row 查 + 校验 expires_at > NOW() + 账户 enabled。


-- name: InsertAdminSession :one
-- 登录成功建会话；session_token_hash / csrf_token / expires_at 由 service 层（Unit 4）算好传入。
INSERT INTO admin_session (session_token_hash, operator_id, csrf_token, expires_at)
VALUES (@session_token_hash, @operator_id, @csrf_token, @expires_at)
RETURNING id, operator_id, csrf_token, expires_at, created_at;


-- name: GetActiveAdminSessionByTokenHash :one
-- 鉴权热路径：按 token_hash 取**未过期 + 账户启用**的会话，并带出运维身份。
-- 任一不满足（无会话 / 已过期 / 账户禁用）→ 0 rows → 中间件 fail-closed 401。
SELECT s.id            AS session_id,
       s.operator_id   AS operator_id,
       s.csrf_token    AS csrf_token,
       s.expires_at    AS expires_at,
       o.username      AS username,
       o.enabled       AS operator_enabled
FROM admin_session s
JOIN operator_account o ON o.id = s.operator_id
WHERE s.session_token_hash = @session_token_hash
  AND s.expires_at > NOW()
  AND o.enabled = true;


-- name: TouchAdminSessionLastSeen :exec
-- 异步 best-effort 更新 last_seen_at（审计 / 可选续期）；失败不影响鉴权主路径。
UPDATE admin_session
SET last_seen_at = NOW()
WHERE id = @id;


-- name: DeleteAdminSessionByTokenHash :execrows
-- 登出：按 token_hash 删会话；返回受影响行数（0 = 会话已不存在，幂等）。
DELETE FROM admin_session
WHERE session_token_hash = @session_token_hash;


-- name: DeleteAdminSessionsByOperator :execrows
-- 禁用账户 / 强制下线：删某运维全部会话。
DELETE FROM admin_session
WHERE operator_id = @operator_id;


-- name: DeleteExpiredAdminSessions :execrows
-- sweep：清理已过期会话（后台定期调用）。
DELETE FROM admin_session
WHERE expires_at <= NOW();

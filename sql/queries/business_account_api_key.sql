-- business_account_api_key.sql —— 业务侧 API Key CRUD + 鉴权热路径
--
-- 设计文档：docs/multimedia-gateway-design.md §9
-- 实施计划：docs/plans/2026-05-27-004-feat-workflow-f-min-openai-compat-relay-plan.md Unit 1
--
-- 命名约定：
--   - InsertBusinessKey / FindActiveBusinessKeyByHash / FindBusinessKeyByID / RevokeBusinessKey：CRUD
--   - ListActiveBusinessKeysByAccount / ListAllActiveBusinessKeys：admin-cli 与运维查询
--   - TouchBusinessKeyLastUsed：异步 best-effort 鉴权命中时间戳
--
-- 关键 PG 语义：
--   - UNIQUE on key_hash：鉴权热路径单 row 查询（O(log n) btree）
--   - COALESCE(revoked_at, NOW())：Revoke 多次同 id 保留首次 timestamp（与 admin token 一致）
--   - WHERE revoked_at IS NULL：所有"active key"查询都过滤已吊销，零额外索引开销


-- ============================================================================
-- 1. CRUD
-- ============================================================================


-- name: InsertBusinessKey :one
-- 插入新业务 key；调用方负责生成 plaintext + HMAC-SHA-256(pepper, plaintext) → key_hash hex。
-- requests_per_minute NULL = 不限速；service 层校验 > 0（DB CHECK 也兜底）。
INSERT INTO business_account_api_key (
    business_account_id, description, key_hash,
    requests_per_minute, created_by
)
VALUES (
    @business_account_id, @description, @key_hash,
    sqlc.narg('requests_per_minute')::int, @created_by
)
RETURNING id, business_account_id, description, key_hash,
          requests_per_minute, created_by,
          created_at, revoked_at, last_used_at, updated_at;


-- name: FindActiveBusinessKeyByHash :one
-- 鉴权热路径：根据 key_hash 单 row 查活跃 key（未 revoke）。
-- service 层调用前先算 hex = hex.EncodeToString(HMAC-SHA-256(pepper, plaintext))。
SELECT id, business_account_id, description, key_hash,
       requests_per_minute, created_by,
       created_at, revoked_at, last_used_at, updated_at
FROM business_account_api_key
WHERE key_hash = @key_hash
  AND revoked_at IS NULL;


-- name: FindBusinessKeyByID :one
-- 按 id 查 key（不限制是否 revoked，运维 / audit 用）。
SELECT id, business_account_id, description, key_hash,
       requests_per_minute, created_by,
       created_at, revoked_at, last_used_at, updated_at
FROM business_account_api_key
WHERE id = @id;


-- name: RevokeBusinessKey :one
-- Revoke 给定 id 的 key。
-- COALESCE 保留首次 revoke 时间戳（与 admin token 一致）；多次 Revoke 同 id 不覆盖。
-- 不存在 id 时返 0 rows（service 层判 ErrKeyNotFound）。
UPDATE business_account_api_key
SET revoked_at = COALESCE(revoked_at, NOW()),
    updated_at = NOW()
WHERE id = @id
RETURNING id, revoked_at;


-- ============================================================================
-- 2. 列表查询（admin-cli list / 运维）
-- ============================================================================


-- name: ListActiveBusinessKeysByAccount :many
-- 列出指定账户的所有活跃 key（未 revoke）；按 created_at DESC 排序便于运维查最新发放。
-- **不**返 key_hash（避免泄漏；明文已无法获取，hash 也只入鉴权 / 排查路径）。
SELECT id, business_account_id, description,
       requests_per_minute, created_by,
       created_at, revoked_at, last_used_at, updated_at
FROM business_account_api_key
WHERE business_account_id = @business_account_id
  AND revoked_at IS NULL
ORDER BY created_at DESC;


-- name: ListAllActiveBusinessKeys :many
-- 列出所有账户的活跃 key；admin-cli `business-key list`（不带 --business-account-id）用。
-- **不**返 key_hash。按 created_at DESC 排序。
SELECT id, business_account_id, description,
       requests_per_minute, created_by,
       created_at, revoked_at, last_used_at, updated_at
FROM business_account_api_key
WHERE revoked_at IS NULL
ORDER BY created_at DESC;


-- ============================================================================
-- 3. 鉴权命中异步更新（best-effort）
-- ============================================================================


-- name: TouchBusinessKeyLastUsed :exec
-- 异步批量更新 last_used_at（service 层 5min 批量 flush）；single id 更新。
-- 运维用途：清理长期未用 key（如 WHERE last_used_at < NOW() - INTERVAL '90 days'）。
-- best-effort：失败不影响鉴权主路径；service 层会捕获并 log 但不返业务错。
UPDATE business_account_api_key
SET last_used_at = NOW(),
    updated_at   = NOW()
WHERE id = @id;

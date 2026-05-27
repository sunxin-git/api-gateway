-- admin_token.sql —— Admin Token CRUD + 阀门用量累计 + 熔断器状态机
--
-- 设计文档：docs/multimedia-gateway-design.md §9bis.6（Admin Token 安全 5 件套）
-- 实施计划：docs/plans/2026-05-27-003-feat-workflow-d-min-admin-api-plan.md Unit 1
--
-- 命名约定：
--   - InsertAdminToken / FindActiveAdminTokenByHash / RevokeAdminToken / ListActiveAdminTokens：token CRUD
--   - IncrementTokenUsage / GetTokenUsage：每日用量累计 + 查询
--   - RecordCircuitError / TripCircuitBreaker / GetCircuitState / ResetCircuitBreaker：熔断器状态机
--
-- 关键 PG 语义：
--   - ON CONFLICT DO UPDATE：用于 UPSERT 同行原子累加，行锁串行化保证 100+ 并发同行无丢失
--   - COALESCE(revoked_at, NOW())：Revoke 同 token 多次时保留首次 revoke 时间戳，幂等成功
--   - CASE WHEN 在 DO UPDATE SET 子句中：实现 circuit 1h 滚动窗口的"超时即重置"


-- ============================================================================
-- 1. Admin Token CRUD
-- ============================================================================


-- name: InsertAdminToken :one
-- 插入新 Admin Token；调用方负责生成 plaintext + HMAC-SHA-256(pepper, plaintext) → token_hash hex。
-- 7 个阀门字段 NULL = 无限制；ip_allowlist 至少 1 个 CIDR（service 层校验，schema 不强制）。
INSERT INTO gateway_admin_token (
    token_hash, description, scopes, ip_allowlist,
    single_recharge_max, daily_recharge_quota_limit,
    single_refund_max, daily_refund_quota_limit,
    daily_account_create_limit, requests_per_minute, circuit_breaker_enabled,
    created_by, expires_at
)
VALUES (
    @token_hash, @description, @scopes, @ip_allowlist,
    sqlc.narg('single_recharge_max')::bigint, sqlc.narg('daily_recharge_quota_limit')::bigint,
    sqlc.narg('single_refund_max')::bigint,   sqlc.narg('daily_refund_quota_limit')::bigint,
    sqlc.narg('daily_account_create_limit')::int, sqlc.narg('requests_per_minute')::int,
    @circuit_breaker_enabled,
    @created_by, sqlc.narg('expires_at')::timestamptz
)
RETURNING id, token_hash, description, scopes, ip_allowlist,
          single_recharge_max, daily_recharge_quota_limit,
          single_refund_max, daily_refund_quota_limit,
          daily_account_create_limit, requests_per_minute, circuit_breaker_enabled,
          created_by, created_at, expires_at, revoked_at;


-- name: FindActiveAdminTokenByHash :one
-- 鉴权热路径：根据 token_hash 单 query 查活跃 token（未 revoke + 未过期）。
-- service 层调用前先算 hex = hex.EncodeToString(HMAC-SHA-256(pepper, plaintext))。
SELECT id, token_hash, description, scopes, ip_allowlist,
       single_recharge_max, daily_recharge_quota_limit,
       single_refund_max, daily_refund_quota_limit,
       daily_account_create_limit, requests_per_minute, circuit_breaker_enabled,
       created_by, created_at, expires_at, revoked_at
FROM gateway_admin_token
WHERE token_hash = @token_hash
  AND revoked_at IS NULL
  AND (expires_at IS NULL OR expires_at > NOW());


-- name: FindAdminTokenByID :one
-- 按 id 查 token（不限制是否 revoked / expired，运维查询用）。
SELECT id, token_hash, description, scopes, ip_allowlist,
       single_recharge_max, daily_recharge_quota_limit,
       single_refund_max, daily_refund_quota_limit,
       daily_account_create_limit, requests_per_minute, circuit_breaker_enabled,
       created_by, created_at, expires_at, revoked_at
FROM gateway_admin_token
WHERE id = @id;


-- name: RevokeAdminToken :one
-- Revoke 给定 id 的 token。
-- 用 COALESCE(revoked_at, NOW()) 保留首次 revoke 时间戳，多次 Revoke 同 id 不覆盖原 timestamp。
-- 返回值（id + 新 revoked_at + 原 revoked_at）让 service 区分"首次 revoke" vs "已 revoked 幂等"。
-- 不存在 id 时返 0 rows（service 层判 ErrTokenNotFound）。
UPDATE gateway_admin_token
SET revoked_at = COALESCE(revoked_at, NOW())
WHERE id = @id
RETURNING id, revoked_at;


-- name: ListActiveAdminTokens :many
-- 列出所有活跃 token（未 revoke）；按 created_at DESC 排序便于运维查最新发放。
-- 返回**不含** token_hash（避免泄漏；明文已无法获取，hash 也只入审计 / 排查路径）。
SELECT id, description, scopes, ip_allowlist,
       single_recharge_max, daily_recharge_quota_limit,
       single_refund_max, daily_refund_quota_limit,
       daily_account_create_limit, requests_per_minute, circuit_breaker_enabled,
       created_by, created_at, expires_at, revoked_at
FROM gateway_admin_token
WHERE revoked_at IS NULL
ORDER BY created_at DESC;


-- ============================================================================
-- 2. Token 每日用量累计
-- ============================================================================


-- name: IncrementTokenUsage :one
-- 单语句原子累加当日用量；不存在则 INSERT，存在则 UPDATE 累加。
-- 三列分别累加（recharge / refund / create）；调用方按场景传非零的那一列，其他传 0。
-- day 用 UTC 当日（决策 D9）。
INSERT INTO gateway_admin_token_usage AS u (
    token_id, day,
    recharge_total_minor, refund_total_minor, account_create_count
)
VALUES (
    @token_id, (NOW() AT TIME ZONE 'UTC')::date,
    @recharge_delta, @refund_delta, @account_create_delta
)
ON CONFLICT (token_id, day) DO UPDATE
SET recharge_total_minor = u.recharge_total_minor + EXCLUDED.recharge_total_minor,
    refund_total_minor   = u.refund_total_minor   + EXCLUDED.refund_total_minor,
    account_create_count = u.account_create_count + EXCLUDED.account_create_count,
    updated_at           = NOW()
RETURNING token_id, day, recharge_total_minor, refund_total_minor, account_create_count, updated_at;


-- name: GetTokenUsage :one
-- 查指定 token 当日用量；不存在则返 0 rows（service 层视作 0 用量）。
-- day 同样按 UTC（与累加 query 对齐）。
SELECT token_id, day, recharge_total_minor, refund_total_minor, account_create_count, updated_at
FROM gateway_admin_token_usage
WHERE token_id = @token_id
  AND day      = (NOW() AT TIME ZONE 'UTC')::date;


-- ============================================================================
-- 3. 熔断器状态机
-- ============================================================================


-- name: RecordCircuitError :one
-- 错误计数 + 1h 滚动窗口自动重置（document-review feasibility #8 完整 SQL）。
-- 行为：
--   - 不存在 token_id → INSERT (window_started_at=NOW(), error_count=1)
--   - 存在但 window_started_at < NOW() - 1h → 重置窗口（error_count=1, window_started_at=NOW()）
--   - 存在且窗口内 → error_count + 1
-- 单语句完成所有路径，行锁串行化保证 50+ 并发同 token error_count 最终 = 实际错误数。
INSERT INTO gateway_admin_token_circuit (token_id, window_started_at, error_count)
VALUES (@token_id, NOW(), 1)
ON CONFLICT (token_id) DO UPDATE
SET error_count = CASE
        WHEN gateway_admin_token_circuit.window_started_at < NOW() - INTERVAL '1 hour'
        THEN 1
        ELSE gateway_admin_token_circuit.error_count + 1
    END,
    window_started_at = CASE
        WHEN gateway_admin_token_circuit.window_started_at < NOW() - INTERVAL '1 hour'
        THEN NOW()
        ELSE gateway_admin_token_circuit.window_started_at
    END,
    updated_at = NOW()
RETURNING token_id, window_started_at, error_count, breaker_tripped_until;


-- name: TripCircuitBreaker :one
-- 跳闸：把 breaker_tripped_until 写为 NOW() + 1h；service 在 RecordCircuitError 返 error_count ≥ 100 时调。
-- 已跳闸 token 重复调用：tripped_until 取较大值（不会缩短跳闸期）。
UPDATE gateway_admin_token_circuit
SET breaker_tripped_until = GREATEST(COALESCE(breaker_tripped_until, NOW()), NOW() + INTERVAL '1 hour'),
    updated_at = NOW()
WHERE token_id = @token_id
RETURNING token_id, window_started_at, error_count, breaker_tripped_until;


-- name: GetCircuitState :one
-- 查熔断器状态；middleware 在 CheckCircuitBreaker 时调用。
-- 不存在 token_id 时返 0 rows（service 视作未熔断）。
SELECT token_id, window_started_at, error_count, breaker_tripped_until,
       (breaker_tripped_until IS NOT NULL AND breaker_tripped_until > NOW()) AS is_tripped
FROM gateway_admin_token_circuit
WHERE token_id = @token_id;


-- name: ResetCircuitBreaker :one
-- 运维手工解锁熔断器（admin-cli circuit-reset 推 P1）。
-- 重置 error_count 与 window，清 breaker_tripped_until。
UPDATE gateway_admin_token_circuit
SET breaker_tripped_until = NULL,
    error_count           = 0,
    window_started_at     = NOW(),
    updated_at            = NOW()
WHERE token_id = @token_id
RETURNING token_id, window_started_at, error_count, breaker_tripped_until;

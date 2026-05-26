-- outbox.sql —— webhook_event_outbox 写路径
--
-- 本工作流仅实现 INSERT；dispatcher（claim/lease 抢占 + 推送 + DLQ）由 C-min 落地。


-- name: InsertOutboxEvent :one
-- 同事务 INSERT outbox event；retention_until / is_financial / delivery_idempotency_key 由调用方算好。
-- delivery_status 默认 'pending'，等待 dispatcher 扫描。
INSERT INTO webhook_event_outbox (
    business_account_id, event_type, payload,
    is_financial, retention_until,
    delivery_status, delivery_attempts,
    delivery_idempotency_key
) VALUES (
    sqlc.narg('business_account_id')::text, @event_type, @payload,
    @is_financial, @retention_until,
    'pending', 0,
    @delivery_idempotency_key
)
RETURNING event_id, business_account_id, event_type, payload, is_financial, retention_until,
          delivery_status, delivery_attempts, locked_by, locked_until,
          delivery_idempotency_key, last_pushed_at, created_at;

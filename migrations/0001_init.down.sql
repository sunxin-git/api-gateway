-- ============================================================================
-- 0001_init.down.sql —— 撤销 0001_init.up.sql 的所有改动
--
-- 顺序：按反向依赖 DROP TABLE（子表先于父表），再 DROP TYPE（枚举最后）
-- 幂等：全部使用 IF EXISTS，重复执行不报错
--
-- 注意：CASCADE 不用——up.sql 的外键已经 ON DELETE 显式声明，
--       这里按依赖反向顺序逐表 DROP，避免依赖关系被无声跳过
-- ============================================================================

-- ---- J. task → 依赖 business_account / channel ----
DROP INDEX IF EXISTS idx_task_channel_id;
DROP INDEX IF EXISTS idx_task_accounting_month;
DROP INDEX IF EXISTS idx_task_submit_recover;
DROP INDEX IF EXISTS idx_task_inflight;
DROP TABLE IF EXISTS task;

-- ---- I. webhook_subscription → 依赖 business_account ----
DROP INDEX IF EXISTS idx_webhook_subscription_account_enabled;
DROP TABLE IF EXISTS webhook_subscription;

-- ---- H. gateway_admin_token → 独立 ----
DROP INDEX IF EXISTS idx_gateway_admin_token_active;
DROP TABLE IF EXISTS gateway_admin_token;

-- ---- G. webhook_event_outbox → 依赖 business_account ----
DROP INDEX IF EXISTS idx_webhook_event_outbox_retention;
DROP INDEX IF EXISTS idx_webhook_event_outbox_pending_event_id;
DROP TABLE IF EXISTS webhook_event_outbox;

-- ---- F. channel_routing_rule → 依赖 business_account ----
DROP INDEX IF EXISTS idx_channel_routing_rule_enabled;
DROP INDEX IF EXISTS idx_channel_routing_rule_account_priority;
DROP TABLE IF EXISTS channel_routing_rule;

-- ---- E. channel → 独立 ----
DROP INDEX IF EXISTS idx_channel_enabled;
DROP INDEX IF EXISTS idx_channel_restricted_accounts;
DROP TABLE IF EXISTS channel;

-- ---- D. business_account_ledger → 依赖 business_account ----
DROP INDEX IF EXISTS uq_ledger_idempotency_key;
DROP INDEX IF EXISTS idx_ledger_correlation;
DROP INDEX IF EXISTS idx_ledger_account_created;
DROP TABLE IF EXISTS business_account_ledger;

-- ---- C. business_account_balance → 依赖 business_account ----
DROP TABLE IF EXISTS business_account_balance;

-- ---- B. business_account → 父表，最后 DROP ----
DROP TABLE IF EXISTS business_account;

-- ---- A. 枚举类型（必须在所有引用表 DROP 后才能 DROP） ----
DROP TYPE IF EXISTS business_account_status;
DROP TYPE IF EXISTS fallback_policy;
DROP TYPE IF EXISTS outbox_delivery_status;
DROP TYPE IF EXISTS task_status;
DROP TYPE IF EXISTS ledger_entry_type;

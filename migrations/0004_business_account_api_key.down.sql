-- ============================================================================
-- Migration 0004 DOWN: 回滚业务侧 API Key 表
-- ----------------------------------------------------------------------------
-- PG 限制：DROP TYPE VALUE 不被支持（ALTER TYPE actor_type DROP VALUE 不存在）。
-- down 不能完全反向 up；仅删表，actor_type 留 'business_key' 枚举值。
--
-- 完整回滚方案（运维 SOP，本 down 不自动做）：
--   1. 跑本 down → 删 business_account_api_key 表
--   2. 手工 CREATE TYPE actor_type_new AS ENUM(老 4 值) →
--      ALTER TABLE business_account_ledger ALTER COLUMN actor_type TYPE actor_type_new USING actor_type::text::actor_type_new →
--      DROP TYPE actor_type → ALTER TYPE actor_type_new RENAME TO actor_type
--   仅在确实需要彻底回滚 F-min 时执行；MVP 接受残留枚举值（无害）。
-- ============================================================================

DROP TABLE IF EXISTS business_account_api_key;

-- actor_type 'business_key' 枚举值保留（PG 不支持 DROP VALUE；详见上方注释）

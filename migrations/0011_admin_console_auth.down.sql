-- ============================================================================
-- Migration 0011 DOWN: 回滚管理后台会话认证
-- ----------------------------------------------------------------------------
-- 先删 admin_session（FK → operator_account），再删 operator_account。
-- ============================================================================

DROP TABLE IF EXISTS admin_session;
DROP TABLE IF EXISTS operator_account;

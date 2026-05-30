-- ============================================================================
-- Migration 0011: 管理后台会话认证（Phase 2 配置线 Unit 2）
-- ----------------------------------------------------------------------------
-- 决策依据：ADR-0008（管理后台会话认证：用户名/密码 + bcrypt + PG 会话）
-- 实施计划：docs/plans/2026-05-31-001-feat-admin-console-config-plan.md Unit 2
--
-- 本次变更（纯增量；不动现有表/枚举）：
--   1. operator_account：运维登录账户（用户名 + bcrypt 口令哈希）；初始管理员经 env 种子，
--      其余由初始管理员经后台开通。
--   2. admin_session：PG 会话（存 session_token_hash 而非明文 + 每会话 csrf_token）。
--
-- 关键说明：
--   - 口令低熵 → password_hash 存 bcrypt（非 HMAC，区别于 admin_token / business_key）；
--     明文绝不入库（ADR-0008 决策 2）。
--   - 会话明文 token 仅写入 Cookie；库内只存其 HMAC（鉴权热路径单 row 查询）。
--   - 会话存 PG（非 Redis）：admin-only 部署可能无 Redis，PG 与 fail-closed 一致（ADR-0008 决策 3）。
-- ============================================================================


-- ----------------------------------------------------------------------------
-- 1. operator_account（运维登录账户）
-- ----------------------------------------------------------------------------
CREATE TABLE operator_account (
    id            bigserial   NOT NULL,
    -- 登录用户名；唯一；不面向公众（初始管理员开通）
    username      text        NOT NULL,
    -- bcrypt(口令) ——低熵口令必须慢哈希；明文/哈希绝不回显（ADR-0008）
    password_hash text        NOT NULL,
    -- 软禁用：false 时拒绝登录（已建会话由 sweep / 鉴权时校验 enabled 失效）
    enabled       boolean     NOT NULL DEFAULT true,
    -- 创建者：初始管理员种子为 'seed'，后台开通为 'operator:<id>'
    created_by    text        NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT NOW(),
    updated_at    timestamptz NOT NULL DEFAULT NOW(),
    CONSTRAINT pk_operator_account PRIMARY KEY (id),
    CONSTRAINT uq_operator_account_username UNIQUE (username)
);

COMMENT ON TABLE  operator_account               IS '运维登录账户（管理后台会话认证；ADR-0008）；初始管理员 env 种子，其余后台开通';
COMMENT ON COLUMN operator_account.password_hash IS 'bcrypt(口令)；低熵口令慢哈希；明文/哈希绝不回显/不入日志';
COMMENT ON COLUMN operator_account.enabled       IS '软禁用；false 拒绝登录';


-- ----------------------------------------------------------------------------
-- 2. admin_session（PG 会话）
-- ----------------------------------------------------------------------------
CREATE TABLE admin_session (
    id                 bigserial   NOT NULL,
    -- HMAC(pepper, 会话明文 token) hex；鉴权热路径单 row UNIQUE 查询；明文只在 Cookie
    session_token_hash text        NOT NULL,
    -- 归属运维账户；账户删除级联清会话
    operator_id        bigint      NOT NULL,
    -- 每会话 CSRF token（登录下发，状态变更请求带 header 校验；Bearer 通道豁免）
    csrf_token         text        NOT NULL,
    -- 会话过期时刻；鉴权校验 expires_at > NOW()，sweep 清理过期行
    expires_at         timestamptz NOT NULL,
    created_at         timestamptz NOT NULL DEFAULT NOW(),
    -- 最近活跃（可选续期 / 审计）
    last_seen_at       timestamptz NULL,
    CONSTRAINT pk_admin_session PRIMARY KEY (id),
    CONSTRAINT uq_admin_session_token_hash UNIQUE (session_token_hash),
    CONSTRAINT fk_admin_session_operator
        FOREIGN KEY (operator_id) REFERENCES operator_account (id) ON DELETE CASCADE
);

-- FK / 按账户清会话（禁用账户时批量删其会话）用
CREATE INDEX idx_admin_session_operator ON admin_session (operator_id);

COMMENT ON TABLE  admin_session                    IS 'PG 会话（管理后台；ADR-0008 决策 3）；存 token 的 HMAC 而非明文';
COMMENT ON COLUMN admin_session.session_token_hash IS 'HMAC(pepper, 会话明文 token) hex；明文仅在 HttpOnly Cookie';
COMMENT ON COLUMN admin_session.csrf_token         IS '每会话 CSRF token；会话通道状态变更请求须带；Bearer 通道豁免';

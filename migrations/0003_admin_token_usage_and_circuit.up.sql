-- ============================================================================
-- Migration 0003: Admin Token 阀门基础设施（refund 字段 + 用量计数 + 熔断器状态）
-- ----------------------------------------------------------------------------
-- 设计文档：docs/multimedia-gateway-design.md §9bis.6（Admin Token 安全 5 件套）
-- 实施计划：docs/plans/2026-05-27-003-feat-workflow-d-min-admin-api-plan.md Unit 1
-- 评审依据：document-review 2 轮深化 —— refund 阀门（高代价高保护）+ pepper hash
--
-- 本次变更总览：
--   1. ALTER gateway_admin_token：加 single_refund_max + daily_refund_quota_limit 两阀门
--   2. CREATE TABLE gateway_admin_token_usage：每日累计用量（recharge + refund + create）
--   3. CREATE TABLE gateway_admin_token_circuit：熔断器窗口状态
--   4. COMMENT 修订：token_hash 算法说明改为 HMAC-SHA-256 + pepper（修 Phase 1 笔误）
--   5. COMMENT 补：新增 refund 阀门字段语义说明
--
-- 关键 PG 语义说明（与 0002 一致）：
--   - ADD COLUMN ... bigint NULL 不重写表，秒级完成
--   - 新建表只引用 gateway_admin_token，无破坏性变更
--   - down.sql DROP 顺序与 up 相反
-- ============================================================================


-- ----------------------------------------------------------------------------
-- 1. ALTER gateway_admin_token：加 refund 维度阀门两列
-- ----------------------------------------------------------------------------
-- 决策来源（document-review round 1）：leaked refund-scope token 可一次性清空账户
-- used_total；与 recharge 阀门对称引入 single / daily refund 上限是 P0 必须的金额维度防御

ALTER TABLE gateway_admin_token
    ADD COLUMN single_refund_max         bigint NULL,
    ADD COLUMN daily_refund_quota_limit  bigint NULL,
    ADD CONSTRAINT chk_gateway_admin_token_single_refund_non_negative
        CHECK (single_refund_max IS NULL OR single_refund_max >= 0),
    ADD CONSTRAINT chk_gateway_admin_token_daily_refund_non_negative
        CHECK (daily_refund_quota_limit IS NULL OR daily_refund_quota_limit >= 0);

COMMENT ON COLUMN gateway_admin_token.single_refund_max
    IS '单笔退款金额上限（minor unit, CNY 分）；NULL = 无限制。document-review 添加';
COMMENT ON COLUMN gateway_admin_token.daily_refund_quota_limit
    IS '当日累计退款金额上限（minor unit, UTC day）；NULL = 无限制';


-- ----------------------------------------------------------------------------
-- 2. COMMENT 修订：token_hash 算法
-- ----------------------------------------------------------------------------
-- Phase 1 schema 注释写 "bcrypt / argon2id"，那是笔误。
-- D-min 决策 D1 锁定：HMAC-SHA-256 + server-side pepper（防 DB 全量泄露离线穷举）。

COMMENT ON COLUMN gateway_admin_token.token_hash
    IS 'HMAC-SHA-256(GATEWAY_TOKEN_PEPPER, token_plaintext) 的 hex 字符串（64 char）；token 明文绝不入库；pepper 通过 env 注入，DB 泄露但 env 未泄时 hash 无法离线穷举';


-- ----------------------------------------------------------------------------
-- 3. gateway_admin_token_usage：每日累计用量
-- ----------------------------------------------------------------------------
-- 主键 (token_id, day)；day 按 UTC（计划决策 D9）；refund / recharge / create 分列累加
-- 用 ON CONFLICT DO UPDATE UPSERT 实现并发安全的累加（行锁串行化）

CREATE TABLE gateway_admin_token_usage (
    token_id              bigint      NOT NULL,
    -- UTC 当日（PostgreSQL 端用 `(NOW() AT TIME ZONE 'UTC')::date` 取值）
    day                   date        NOT NULL,
    -- 累计成功充值金额（minor unit）；service 在 LedgerService.Recharge outcome=FreshlyWritten 后累加
    recharge_total_minor  bigint      NOT NULL DEFAULT 0,
    -- 累计成功退款金额（minor unit）；同理仅 FreshlyWritten 后累加
    refund_total_minor    bigint      NOT NULL DEFAULT 0,
    -- 累计成功创建账户次数；CreateAccount 无幂等，UNIQUE 冲突即 ErrAccountAlreadyExists，仅成功后累加
    account_create_count  int         NOT NULL DEFAULT 0,
    updated_at            timestamptz NOT NULL DEFAULT NOW(),
    CONSTRAINT pk_gateway_admin_token_usage PRIMARY KEY (token_id, day),
    CONSTRAINT fk_gateway_admin_token_usage_token
        FOREIGN KEY (token_id) REFERENCES gateway_admin_token (id) ON DELETE CASCADE,
    -- 三列非负（与 ledger CHECK 一致的防御编程）
    CONSTRAINT chk_gateway_admin_token_usage_non_negative
        CHECK (recharge_total_minor >= 0 AND refund_total_minor >= 0 AND account_create_count >= 0)
);

-- 日常清理 job 用：按 day 扫旧记录
CREATE INDEX idx_gateway_admin_token_usage_day
    ON gateway_admin_token_usage (day);

COMMENT ON TABLE  gateway_admin_token_usage                       IS 'Admin Token 每日用量累计；阀门 daily_* 字段的真相源（CONTEXT.md throttle）';
COMMENT ON COLUMN gateway_admin_token_usage.day                   IS 'UTC 当日；多时区业务系统看到的"今日"与配额"今日"可有 8h 偏差';
COMMENT ON COLUMN gateway_admin_token_usage.recharge_total_minor  IS '当日累计成功充值金额；仅 LedgerService outcome=FreshlyWritten 后累加';
COMMENT ON COLUMN gateway_admin_token_usage.refund_total_minor    IS '当日累计成功退款金额；同上语义';
COMMENT ON COLUMN gateway_admin_token_usage.account_create_count  IS '当日累计成功创建账户次数';


-- ----------------------------------------------------------------------------
-- 4. gateway_admin_token_circuit：熔断器窗口状态
-- ----------------------------------------------------------------------------
-- 主键 token_id（1:0..1）；window 1h 滚动重置；breaker_tripped_until 在未来时拒绝请求
-- RecordCircuitError UPSERT 用 CASE WHEN 实现窗口滚动（详见 sql/queries/admin_token.sql）

CREATE TABLE gateway_admin_token_circuit (
    token_id              bigint      NOT NULL,
    -- 1 小时滚动窗口起点；> 1h 时下次 RecordCircuitError 重置为 NOW()
    window_started_at     timestamptz NOT NULL DEFAULT NOW(),
    -- 当前窗口内累计错误数（4xx / 5xx）；超阈值 100 触发 TripCircuitBreaker
    error_count           int         NOT NULL DEFAULT 0,
    -- 跳闸截止时间；NULL = 未跳闸；> NOW() = 熔断中（middleware 拒绝请求）
    breaker_tripped_until timestamptz NULL,
    updated_at            timestamptz NOT NULL DEFAULT NOW(),
    CONSTRAINT pk_gateway_admin_token_circuit PRIMARY KEY (token_id),
    CONSTRAINT fk_gateway_admin_token_circuit_token
        FOREIGN KEY (token_id) REFERENCES gateway_admin_token (id) ON DELETE CASCADE,
    CONSTRAINT chk_gateway_admin_token_circuit_error_count_non_negative
        CHECK (error_count >= 0)
);

COMMENT ON TABLE  gateway_admin_token_circuit                       IS 'Admin Token 熔断器状态；1h 滚动窗口；100 次 4xx/5xx 触发跳闸 1h';
COMMENT ON COLUMN gateway_admin_token_circuit.window_started_at     IS '1h 滚动窗口起点；下次 RecordCircuitError 时若 > 1h 则重置';
COMMENT ON COLUMN gateway_admin_token_circuit.error_count           IS '当前窗口内累计错误数；> 100 触发 TripCircuitBreaker';
COMMENT ON COLUMN gateway_admin_token_circuit.breaker_tripped_until IS '跳闸截止时间；NULL 或过去 = 未熔断；未来 = 熔断中，middleware 拒绝请求';

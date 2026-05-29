-- ============================================================================
-- Migration 0005: 异步视频中继数据层（Phase 2 Unit 2）
-- ----------------------------------------------------------------------------
-- 设计文档：docs/multimedia-gateway-design.md §9 / §9ter / §9.5
-- 实施计划：docs/plans/2026-05-28-001-feat-async-video-relay-mvp-plan.md Unit 2
-- 决策依据：ADR-0006（异步基座 + R15 DB 原子 claim + 上游幂等缺失 fail-closed）
--
-- 本次变更（纯增量；**不动 task_status 枚举**——枚举增值见 0006，索引重建见 0007）：
--   1. task 加 callback_token（回调 per-task token）+ upstream_submitted_at（进上游已提交态
--      时刻，供 fetch reconciler 判超时）
--   2. account_model_concurrency：R15 并发硬上限的权威计数行（每 (account,model) 一行 inflight），
--      原子占位/释放用（claim = 上游并发槽）
--   3. business_account_model_entitlement：账户×模型授权（grant/revoke/check）
--
-- 关键说明：
--   - 并发 cap 不入本表：cap 由 Go 侧（Unit 8）按 config 默认 + 覆写解析后作查询参数传入；
--     本表只持 inflight 计数（详见 ADR-0006 决策 2 / 计划 Unit 2 Approach）。
--   - settle_failed 枚举值（SETTLE_FAILED）与 idx_task_inflight 重建分别在 0006 / 0007
--     （PG 不能在加值的同事务里使用新枚举值；migrate 每文件 1 事务）。
-- ============================================================================


-- ----------------------------------------------------------------------------
-- 1. task 新增列
-- ----------------------------------------------------------------------------
ALTER TABLE task
    ADD COLUMN callback_token        text         NULL,
    ADD COLUMN upstream_submitted_at timestamptz  NULL;

COMMENT ON COLUMN task.callback_token        IS '回调 per-task 随机 token（Unit 8 生成）；进终态后置空；绝不入日志 / 不放 query string';
COMMENT ON COLUMN task.upstream_submitted_at IS '进入 UPSTREAM_SUBMITTED 的时刻；供 fetch reconciler 判上游超时未终态';


-- ----------------------------------------------------------------------------
-- 2. account_model_concurrency（R15 并发硬上限权威计数行）
-- ----------------------------------------------------------------------------
-- 每 (business_account_id, model) 一行 inflight 计数；提交前原子占位：
--   UPDATE ... SET inflight = inflight + 1 WHERE ... AND inflight < @cap RETURNING inflight
-- 进上游终态 CAS 同事务释放 inflight = inflight - 1。cap 不存本表（Go 侧解析为查询参数）。
CREATE TABLE account_model_concurrency (
    business_account_id   text         NOT NULL,
    -- gateway 可见 model 名（与 task.model / entitlement.gateway_model 同口径）
    model                 text         NOT NULL,
    -- 在途（占上游并发槽）任务数；只数 SUBMITTED / UPSTREAM_SUBMITTING / UPSTREAM_SUBMITTED
    inflight              int          NOT NULL DEFAULT 0,
    updated_at            timestamptz  NOT NULL DEFAULT NOW(),
    CONSTRAINT pk_account_model_concurrency PRIMARY KEY (business_account_id, model),
    CONSTRAINT fk_account_model_concurrency_account
        FOREIGN KEY (business_account_id) REFERENCES business_account (id) ON DELETE CASCADE,
    -- 释放永不为负（防 double-release）；占位不超由查询的 inflight < @cap 保证
    CONSTRAINT chk_account_model_concurrency_inflight_non_negative
        CHECK (inflight >= 0)
);

COMMENT ON TABLE  account_model_concurrency          IS 'R15 并发硬上限权威计数行（每账户×模型一行 inflight）；claim = 上游并发槽（ADR-0006 决策 2）';
COMMENT ON COLUMN account_model_concurrency.inflight IS '在途任务数（SUBMITTED/UPSTREAM_SUBMITTING/UPSTREAM_SUBMITTED）；占位 +1 / 上游终态 -1；cap 由 Go 侧作查询参数';


-- ----------------------------------------------------------------------------
-- 3. business_account_model_entitlement（账户×模型授权）
-- ----------------------------------------------------------------------------
-- 授权 = 行存在；revoke = 删行；check = 存在性查询（计划 Unit 10）。
-- 复合主键 (business_account_id, gateway_model)，天然唯一。
CREATE TABLE business_account_model_entitlement (
    business_account_id   text         NOT NULL,
    gateway_model         text         NOT NULL,
    created_at            timestamptz  NOT NULL DEFAULT NOW(),
    updated_at            timestamptz  NOT NULL DEFAULT NOW(),
    CONSTRAINT pk_business_account_model_entitlement
        PRIMARY KEY (business_account_id, gateway_model),
    CONSTRAINT fk_business_account_model_entitlement_account
        FOREIGN KEY (business_account_id) REFERENCES business_account (id) ON DELETE CASCADE
);

COMMENT ON TABLE business_account_model_entitlement IS '账户×模型授权；行存在 = 已授权，revoke = 删行，check = 存在性（计划 Unit 10）';

-- ============================================================================
-- Migration 0010: TOS 结果对象元数据（Phase 2 Unit 9）
-- ----------------------------------------------------------------------------
-- 设计文档：docs/multimedia-gateway-design.md §9（结果存储）
-- 实施计划：docs/plans/2026-05-28-001-feat-async-video-relay-mvp-plan.md Unit 9
-- 决策依据：ADR-0006 决策 3（官方 TOS SDK；结果对象 + 受限签名 URL）
--
-- 本次变更（纯增量）：新增 oss_object_meta —— 每个成功任务转存到企业 TOS 的产物对象的
-- **持久元数据**（bucket / object_key / region / endpoint / 大小 / 类型）。
--
-- 关键说明：
--   - **不存签名 URL**：签名 URL 短时有效且含签名秘密，读时（Unit 10 GET）按 meta 现签现取，
--     绝不持久化（避免落库的过期 URL + 秘密泄露）。
--   - **不存上游源 URL**：上游 video_url 仅 24h 有效且属上游秘密，转存后无保留价值。
--   - PK = task_id：一任务一结果对象，天然幂等（重复 store 命中 PK 冲突 → 跳过）。
--   - object_key 含不可枚举随机段（Go 侧生成，project_id 隔离前缀），不可枚举。
--   - FK task_id ON DELETE CASCADE：任务删则元数据随删（TOS 对象由生命周期策略 / 运维清理，不在此）。
--   - 计划原拟 0006，因 task_status 枚举增值占用 0006、索引重建占 0007/0008/0009，顺延至 0010。
-- ============================================================================

CREATE TABLE oss_object_meta (
    -- 任务 id（一任务一结果对象）
    task_id              text         NOT NULL,
    -- 归属账户（denormalized，便于按账户清理 / 归属查询；task 亦持有，避免读时强制 join）
    business_account_id  text         NOT NULL,
    -- TOS 目标（转存时绑定的 channel TOS 配置快照；调价 / 换 bucket 不影响已存对象的取回）
    bucket               text         NOT NULL,
    object_key           text         NOT NULL,
    region               text         NOT NULL,
    endpoint             text         NOT NULL,
    -- 对象内容元数据
    content_type         text         NOT NULL DEFAULT '',
    size_bytes           bigint       NOT NULL DEFAULT 0,
    stored_at            timestamptz  NOT NULL DEFAULT NOW(),
    CONSTRAINT pk_oss_object_meta PRIMARY KEY (task_id),
    CONSTRAINT fk_oss_object_meta_task
        FOREIGN KEY (task_id) REFERENCES task (id) ON DELETE CASCADE,
    CONSTRAINT chk_oss_object_meta_size_non_negative
        CHECK (size_bytes >= 0)
);

COMMENT ON TABLE  oss_object_meta             IS '成功任务转存到企业 TOS 的产物对象持久元数据；签名 URL 读时现签不入库（Unit 9）';
COMMENT ON COLUMN oss_object_meta.object_key  IS 'TOS 对象 key，含不可枚举随机段 + project_id 隔离前缀；不可枚举';
COMMENT ON COLUMN oss_object_meta.bucket      IS '转存时绑定的 channel TOS bucket 快照（换 bucket 不影响已存对象取回）';


-- ----------------------------------------------------------------------------
-- 支持 recoverMissingStore 的 ScanSettledNeedingStore 扫描（Unit 9）
-- ----------------------------------------------------------------------------
-- 扫描谓词：status='SETTLED' AND error_code IS NULL AND terminal_at>=… AND updated_at<… AND 无 meta。
-- 既有 idx_task_inflight 排除了 SETTLED（部分索引谓词），不可用；故建专用部分索引覆盖该后台 sweep，
-- 避免随 task 表增长退化为顺序扫描（ce-review data-migrations）。按 updated_at 排序（查询 ORDER BY）。
CREATE INDEX idx_task_settled_needing_store
    ON task (updated_at)
    WHERE status = 'SETTLED' AND error_code IS NULL;

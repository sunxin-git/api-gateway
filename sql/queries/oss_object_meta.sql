-- oss_object_meta.sql —— TOS 结果对象元数据 CRUD + 转存恢复扫描（Phase 2 Unit 9）
--
-- 实施计划：docs/plans/2026-05-28-001-feat-async-video-relay-mvp-plan.md Unit 9
-- 决策：ADR-0006 决策 3（官方 TOS SDK；结果对象 + 受限签名 URL）
--
-- 不存签名 URL / 上游源 URL（见 migration 0010 说明）；PK=task_id 天然幂等。


-- name: InsertOSSObjectMeta :execrows
-- 记录转存产物元数据；ON CONFLICT DO NOTHING 实现幂等（重复 store / 并发重投命中 PK 冲突 → 0 行）。
-- 返回受影响行数：1 = 本次插入，0 = 已存在（幂等跳过）。
INSERT INTO oss_object_meta (
    task_id, business_account_id, bucket, object_key, region, endpoint, content_type, size_bytes
) VALUES (
    @task_id, @business_account_id, @bucket, @object_key, @region, @endpoint, @content_type, @size_bytes
)
ON CONFLICT (task_id) DO NOTHING;


-- name: GetOSSObjectMetaByTask :one
-- 按 task_id 取产物元数据（store 幂等判存 + Unit 10 GET 现签 URL 用）。不命中返 0 rows。
SELECT * FROM oss_object_meta WHERE task_id = @task_id;


-- name: ScanSettledNeedingStore :many
-- 转存恢复扫描（6b fetch reconciler 兜底丢失 / 失败的 store job）：
-- 扫「COMPLETED 来源已 SETTLED（error_code 为空）、结算超阈值仍无 oss_object_meta、且上游 URL 仍在
-- 24h 有效窗口内」的任务 → 调用方幂等重投 store。
--   - status='SETTLED' + error_code IS NULL ⟺ COMPLETED 来源成功结算（失败来源 error_code 非空；
--     缺 usage 者为 SETTLE_FAILED，状态不同，均被排除）。
--   - terminal_at >= @url_valid_after（= now-24h）：上游 video_url 仅 24h 有效，超窗无法转存 → 不再扫
--     （转人工对账，避免无效重投）。
--   - updated_at < @stale_before（= now-阈值）：只扫结算已过阈值仍无 meta 者，避让正常 store job 刚跑的新任务。
SELECT * FROM task
WHERE status = 'SETTLED'
  AND error_code IS NULL
  AND terminal_at >= @url_valid_after
  AND updated_at  <  @stale_before
  AND NOT EXISTS (SELECT 1 FROM oss_object_meta m WHERE m.task_id = task.id)
ORDER BY updated_at
LIMIT @batch_size;

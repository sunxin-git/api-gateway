-- ============================================================================
-- Migration 0006: task_status 枚举增 SETTLE_FAILED（Phase 2 Unit 2）
-- ----------------------------------------------------------------------------
-- 计划：docs/plans/2026-05-28-001-feat-async-video-relay-mvp-plan.md Unit 2
-- 决策：ADR-0006 决策 5（缺 usage / Poll 持续失败 / settle 重试耗尽 → 落此终态）
--
-- SETTLE_FAILED = 第 10 个 task_status：上游已终态但结算失败 → 终态 + 告警 + 进对账队列
--   （不按 reserve 上界 commit、不静默 release）。**不持并发 claim**（claim 在上游终态已释放）。
--
-- 命名：沿用既有枚举的全大写约定（其余 9 值均大写，如 SUBMITTED / SETTLED），保持枚举一致。
--
-- PG 硬约束：ALTER TYPE ADD VALUE 不能在同事务里被"使用"（migrate 每文件 1 事务，见 0004 先例）。
--   故本文件**只加值、不使用**；idx_task_inflight 重建（在排除谓词里使用该值）放 0007 独立文件。
-- ============================================================================

ALTER TYPE task_status ADD VALUE IF NOT EXISTS 'SETTLE_FAILED';

COMMENT ON TYPE task_status IS '任务状态机 10 态：SUBMITTED / UPSTREAM_SUBMITTING / UPSTREAM_SUBMITTED / COMPLETED / FAILED / CANCELLED / EXPIRED / SETTLING / SETTLED / SETTLE_FAILED（SETTLE_FAILED = 结算失败终态，不持并发 claim）';

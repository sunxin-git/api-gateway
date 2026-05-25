# Codex 独立评审报告 — 多媒体 AI 网关设计 v1.2.2

> **评审时间：** 2026-05-25
> **评审对象：** `docs/multimedia-gateway-design.md`（v1.2.2）
> **评审者：** Codex（裁判视角第五轮）
> **总评：** 9/10（v1.2.1 的 8.5/10 → 9/10）
> **P0 编码准入判断：** ⚠️ **Partial** —— 5 项断点全部 Yes，但 v1.2.2 自身引入 2 个 Important

---

## v1.2.1 提出的 5 项断点核验

| 断点 | v1.2.2 落点 | 评分 |
|------|-----------|------|
| 1 entry_type 枚举补 cashout / recharge_reversal | 3ter.2 表格 | ✅ Yes |
| 2 删 3ter.1 旧 410 表述 | 3ter.1 第 4 条 | ✅ Yes |
| 3 修正 DSN 示例 | 9bis.4.1 `extractDSN(db)` | ✅ Yes |
| 4 SQLite expired lease 抢占 | 工作流 C-min | ✅ Yes |
| 5 task submit 强保证 | 9.5 + 9ter.2 | ✅ Yes |

---

## v1.2.2 新引入问题

### Important 1：`UPSTREAM_SUBMITTING` 缺崩溃恢复

worker 抢占到 `UPSTREAM_SUBMITTING` 后若崩溃，状态会卡住。9ter.2 只列出 worker 主动推进到 `UPSTREAM_SUBMITTED` / `SUBMITTED` / `FAILED`，未列超时回收机制。

### Important 2：状态集合未同步纳入 `UPSTREAM_SUBMITTING`

- 9ter.4 删除校验仍只查 `IN ('SUBMITTED','UPSTREAM_SUBMITTED','SETTLING')`
- 9ter.6 月结"未终结的"只列 `SUBMITTED / UPSTREAM_SUBMITTED / 终态未 SETTLED`
- 9ter.2 状态图仍直接从 `SUBMITTED` 到 `UPSTREAM_SUBMITTED`

---

## P0 编码准入判断

**Partial — 9/10**

剩余阻塞项（v1.2.3 解决）：

1. 为 `UPSTREAM_SUBMITTING` 增加抢占租约 + 超时恢复（`submit_locked_until` / `submit_attempt_id`，超时 CAS 回 `SUBMITTED` 或转 `EXPIRED`）
2. 把 `UPSTREAM_SUBMITTING` 补入所有 inflight / 未终结状态集合：9ter.4 删除校验、9ter.6 月结分类、状态图、`task_inflight_count` 口径

---

## 最终建议

修完上述 2 点后，v1.2.2 即可作为 P0 编码稳定基线，可立即启动 P0。这两点都是机械补漏，不动架构。

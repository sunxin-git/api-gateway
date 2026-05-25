# Codex 独立评审报告 — 多媒体 AI 网关设计 v1.2.1

> **评审时间：** 2026-05-25
> **评审对象：** `docs/multimedia-gateway-design.md`（v1.2.1，对 v1.2 收敛修订后）
> **评审者：** Codex（裁判视角第四轮）
> **总评：** 8.5/10（v1.2 的 8/10 → 8.5/10）
> **P0 编码准入判断：** ⚠️ **Partial** —— 5 个实现级断点需 v1.2.2 收尾

---

## v1.2 提出的 6 项修订核验

### C1 账本不变量数学一致性
- **达成：Yes**
- 主不变量 `available + reserved + used_total = recharge_total`，`refund_total` 退化为审计字段
- 独立验证 recharge / reserve / commit / release / refund / cashout 六类操作守恒
- 遗留：`entry_type` 枚举未列出 `cashout` / `recharge_reversal`

### C2 outbox 多节点 claim/lease
- **达成：Partial**
- PG/MySQL 主路径成立（`FOR UPDATE SKIP LOCKED` + 抢占 expired delivering）
- 遗留：SQLite 降级 CAS 只 cover 'pending'，没 cover expired delivering 抢占

### I1 替换 GORM DB 指针比较
- **达成：Partial**
- 方向正确（normalized DSN + schema + tx 强制传递）
- 遗留：示例两边都从 `os.Getenv("SQL_DSN")` 归一化，无法检测 OutboxDB 实际错接到 LOG_DB

### I2 旧 quota 分阶段迁移
- **达成：Partial**
- 3ter.4 已明确 P0 内部转调 + T+60 天 410
- 遗留：3ter.1 第 4 条仍残留"P0 上线时统一禁用并返回 410 Gone"，与 3ter.4 冲突

### I3 task:submit + 事件清理
- **达成：Partial**
- `handleTaskSubmit` 已补；月结事件清单已切换为 provisional/adjustment/finalized
- 遗留：submit 先调上游再 CAS，CAS 失败靠 best-effort 取消上游，无强保证防孤儿任务

### M1 object_item 不直接扣费
- **达成：Yes**
- 6.3.7 强约束明确

---

## v1.2.1 新引入问题

**实现级断点 5 个**（非架构问题）：

1. `cashout` / `recharge_reversal` 进入语义和 SQL 示例，但 ledger `entry_type` 枚举未同步补充
2. 3ter.1 vs 3ter.4 旧 410 表述冲突（**最高优先级**）
3. outbox SQLite claim/lease 降级缺 expired lease 抢占
4. `handleTaskSubmit` 输家取消孤儿上游任务只是 best-effort，应加本地抢占或上游幂等
5. I1 normalized DSN 示例两边同源，漏检风险

---

## P0 编码准入最终判断

**判断：Partial**
**评分：8.5/10**

理由：v1.2.1 已实质修复账本数学、月结事件命名、outbox 多节点主路径、事务传递约束、object_item 双扣费风险，整体明显优于 v1.2。但 5 个实现级断点（旧 410 冲突、I1 DSN 示例漏检、SQLite lease、task submit 孤儿任务、entry_type 补漏）会直接影响实现正确性。

**剩余必修项（v1.2.2 解决）：**

1. 补齐 `entry_type` 枚举（`cashout` / `recharge_reversal`）
2. 删除 3ter.1 旧 410 表述，与 3ter.4 保持一致
3. 修正 normalized DSN 示例为真实各自连接源
4. 补 SQLite expired lease CAS 抢占逻辑
5. 强化 task submit 的本地抢占或上游幂等策略

---

## 最终建议

不建议直接开放 P0 编码。建议出 **v1.2.2 小修订**，**无需重写架构**，只需清理上述 5 个实现级断点。修订后可立即进入 P0。

重点优先级：
1. **3ter.1 旧 410 冲突** —— I2 残留矛盾最高风险，编码者可能误按旧规则切断运营入口
2. **SQLite lease 恢复缺口** —— C2 遗留，影响多节点正确性

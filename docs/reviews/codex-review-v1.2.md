# Codex 独立评审报告 — 多媒体 AI 网关设计 v1.2

> **评审时间：** 2026-05-25
> **评审对象：** `docs/multimedia-gateway-design.md`（v1.2，对 v1.1 收敛修订后）
> **评审者：** Codex（裁判视角，对照 v1.1 评审报告的准入标准）
> **总评：** 8/10（v1.1 的 7/10 → 8/10）
> **P0 编码准入判断：** ⚠️ **Partial** —— 接近准入但还需 v1.2.1 小修订（5 项）

---

## 一、准入标准 5 条逐条核验

### 准入标准 1：删 business_account.quota + ledger 不变量
**v1.2 落点：** 3ter.1 / 3ter.2 / 3ter.3 / 3ter.4 / 9bis.7 / 十六工作流 E
**是否达成：** **Partial**
**理由：** `business_account.quota/used_quota` 已删除，`user.quota/token.RemainQuota` 改只读派生，`balance` 定义为 ledger 严格投影，方向正确。但新不变量 `available + reserved + used_total = recharge_total - refund_total` 与 3ter.3 的 refund 操作冲突：refund 同时 `available += r`、`used_total -= r`、`refund_total += r`，左侧不降，右侧下降，会制造 drift。
**遗留风险：** 账本 reconcile 会误判，或实现者为通过测试绕开不变量，重新引入账务歧义。

### 准入标准 2：禁止隔离账号进入 global_pool/legacy_distributor
**v1.2 落点：** 8.3 / 8.3.6 / 9bis.7 / 十三 P0 B-min / 十六工作流 B-min
**是否达成：** **Yes**
**理由：** 文档明确新增 `isolation_required`，保存配置和运行时都禁止 `global_pool` / `legacy_distributor` / 跨企业 `next_rule`，且要求至少有一个本企业专属 channel。break-glass 被定义为显式审批逃生门，不是默认降级。
**遗留风险：** break-glass 运营流程较重，小团队可能出现不可用时无人审批。

### 准入标准 3：outbox 主库 + ledger 同事务
**v1.2 落点：** 9bis.4.1 / 十三 P0 C-min / 十六工作流 C-min
**是否达成：** **Yes**
**理由：** 文档明确 `webhook_event_outbox` 必须落主库，与 `business_account_ledger` 同事务；`LOG_SQL_DSN` 只能承载 `webhook_delivery_log` 副本。
**遗留风险：** 9bis.4.1 的 `model.OutboxDB() != model.LedgerDB()` 指针比较示例不可靠，需要改成配置/连接身份校验和同一个 `tx *gorm.DB` 传播约束。

### 准入标准 4：状态机 + 9.5 示例 + 跨月规则
**v1.2 落点：** 9.5 / 9ter.2 / 9ter.6 / 9bis.5
**是否达成：** **Partial**
**理由：** 9ter.2 已给唯一状态转移表，9.5 已拆 fetch/settle CAS，删除 24 小时硬窗口并引入 provisional/adjustment。但 9.5 未展示 `SUBMITTED → UPSTREAM_SUBMITTED` 的 `task:submit` CAS handler；9bis.5 仍保留 `billing.monthly_settle` "完整账单"旧说法，与 9ter.6 的 provisional/finalized 事件不完全一致。
**遗留风险：** submit 路径实现可能再次绕过状态机；业务系统可能误把 provisional 当最终月账。

### 准入标准 5：P0 范围重切
**v1.2 落点：** 十三 / 十六
**是否达成：** **Yes**
**理由：** P0 已收缩为 ledger、routing fail-closed、outbox 主库事务、最小 Admin API；billingexpr v2/vp、OSS 月结、完整 UI、完整 webhook 补偿均后移。
**遗留风险：** 3-4 周仍偏紧，尤其包含 Admin Token 阀门、旧 quota 写入冻结、break-glass、路由 CRUD 和 outbox 临时投递。

---

## 二、v1.2 修订引入的新问题

### Critical

**[Critical 1] 账本不变量数学错误**
- 位置：3ter.1 / 3ter.2 / 3ter.3
- 描述：不变量公式与 refund 更新规则数学不自洽。设 refund r：左侧 `(available+r) + reserved + (used-r) = available + reserved + used`（不变），右侧 `recharge - (refund+r) = recharge - refund - r`（下降）→ 等式破裂。
- 后果：账本投影天然 drift，冻结机制可能频繁误触发。
- 建议：先区分"退回可用额度"和"向客户退款出账"。若 refund 是返还额度，则不应进入 `recharge_total - refund_total`；若是出账退款，则不应 `available += r`。必要时拆 `credit_refund` 与 `cash_refund` 两类 entry。**最简方案：** 改不变量为 `available + reserved + used_total = recharge_total`，refund_total 仅作审计字段不进等式。

**[Critical 2] outbox 多节点扫描重复推送**
- 位置：十六工作流 C-min
- 描述：P0 "单 goroutine 每 5 秒扫 outbox"未说明多节点部署谁扫描。
- 后果：多实例会重复推送；节点重启也会丢失投递进度。
- 建议：P0 也要用 DB claim/lease：`pending → delivering` CAS、`locked_by/locked_until`、幂等 delivery key；或明确仅 master 节点扫描并说明 leader election 方案。

### Important

**[Important 1] OutboxDB 指针比较不可靠**
- 位置：9bis.4.1
- 描述：`OutboxDB() != LedgerDB()` 比较 GORM 指针不能证明同库；同库不同实例会误报，不同库也可能被封装成不可辨识对象。
- 后果：fail-fast 可能误杀或漏杀。
- 建议：校验 normalized DSN / schema / table placement，并强制 ledger/outbox 写入共享同一个事务对象。

**[Important 2] 旧 quota 410 过激**
- 位置：3ter.4 / 十六工作流 E
- 描述：`user.quota` / `token.RemainQuota` 写接口直接 410 可能过激，容易被实现成切断现有控制台充值入口。
- 后果：运营充值、修正余额流程中断。
- 建议：UI / 公开充值入口迁到 ledger recharge；只禁止直接写旧字段，410 放在 release 期之后（30-60 天）。

**[Important 3] 缺 task:submit handler 示例**
- 位置：9ter.2 / 9.5
- 描述：状态表有 `task:submit` worker，但 9.5 没有对应 `SUBMITTED → UPSTREAM_SUBMITTED` CAS 示例。
- 后果：实现者可能只写 fetch / settle，遗漏 submit 幂等。
- 建议：补 `handleTaskSubmit` 示例：仅 `SUBMITTED` 可调用上游，成功 CAS 到 `UPSTREAM_SUBMITTED`，失败到 `FAILED`。

**[Important 4] 月结事件命名混乱**
- 位置：9ter.6 / 9bis.5
- 描述：adjustment 逐条推送时，业务系统不应每条都重生成"5 月总账单"；文档有"建议累积"，但 9bis.5 旧 `billing.monthly_settle` 语义残留。
- 后果：账单版本混乱，下游对账失败。
- 建议：明确账期有 provisional、adjustment ledger、finalized 三种视图，清理旧 `monthly_settle` 事件定义。

### Minor

**[Minor 1] 6.3 / 6.3.7 双层模型 ledger 入账层级不清**
- 位置：6.3 / 6.3.7
- 描述：旧文仍称 line_item 为"不可变明细"，双层模型里 object_item 和 line_item 哪一层写 ledger 不够清晰。
- 后果：实现时可能让两层都入账造成双重扣费。
- 建议：明确"只有 line_item / adjustment 写 ledger，object_item 不直接扣费"。

---

## 三、P0 编码准入最终判断

**判断：Partial**
**总体评分：8/10**（v1.1 的 7/10 → 8/10）
**理由：** v1.2 已把五个准入方向大体收敛，文档质量明显高于 v1.1。但账本不变量是 P0 的地基，现在仍有数学错误；outbox P0 临时扫描和启动校验也存在实现级缺口。因此不建议直接开 P0 编码，应先做一次小修订。

**必须再修订项（按风险倒序，5 项）：**

1. 修正 refund 语义与 `available/reserved/used/recharge/refund` 不变量的数学一致性
2. 补 outbox 多节点扫描 claim/lease 规则，不能只写单 goroutine
3. 替换 GORM DB 指针比较为可验证的主库布局校验和事务传递约束
4. 补 `task:submit` CAS handler 示例，并清理 9bis.5 旧 `billing.monthly_settle` 语义
5. 明确旧 quota 运营入口迁移路径：ledger 转调先行，410 只针对直接写旧字段

---

## 四、最终建议

出一个 v1.2.1 或 v1.3 小修订，**不需要重写大设计**。修完上述 5 点后，可以进入 P0。当前版本只能算"接近准入"，还不是可直接编码的最终稿。

具体建议：
- **第 1 条（不变量）** 是架构师必须拍板的数学定义，建议用半天时间在文档里画清楚"返还额度"vs"退款出账"两个操作的 ledger entry 类型，各自对不变量的影响。
- **第 2、3 条**（outbox 多节点 + 指针校验）是实现层规范，属于文字补丁，1-2 天可完成。
- **第 4、5 条** 可合并进一次集中修订，不影响整体架构。

修订完成后本文件可直接作为 P0 编码参考基线。

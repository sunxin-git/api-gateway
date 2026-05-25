# Codex 独立评审报告 — 多媒体 AI 网关设计 v1.1

> **评审时间：** 2026-05-25
> **评审对象：** `docs/multimedia-gateway-design.md`（v1.1，对 v1 修订后约 2200 行）
> **评审者：** Codex（独立架构师视角，对照 v1 评审报告 + new-api 源码）
> **总评：** 7/10（与 v1 持平：覆盖面提升但引入 5 个新 Critical 抵消）
> **判断：** ❌ **不允许进入 P0 编码**，需 v1.2 修订收敛
> **处理动作：** 已据此评审产出 v1.2 修订计划（见主设计文档 changelog）

本报告为审计存档。修订后的设计文档以 `multimedia-gateway-design.md` 为准。

---

## 一、v1 问题解决质量表（Codex 评分）

| 编号 | v1 问题 | v1.1 对应章节 | 评分 | 理由 |
|---|---|---|---|---|
| C1 | 余额账本缺少单一真相源 | 三ter、9bis.7 | **B** | 引入了 ledger 但 9bis.7 同时在 business_account 表保留 quota/used_quota，business_account_balance 又作为门控，形成三源并立 |
| C2 | 企业专属路由 fail-open | 8.3、8.5 | **C** | 默认改 strict 正确，但 fallback_policy 保留 global_pool/legacy_distributor，企业规则缺失时仍可下穿。典型包装型修复 |
| I1 | billingexpr 版本协议 | 5.4、工作流 F | **B** | 协议结构已补，但源码 compile.go 仍只认 v1:；工作流 F 推后到 P0 末期/P1 |
| I2 | 异步任务财务状态机 | 九ter | **C** | 状态矩阵新增，但 9.5 示例 CAS 从 SUBMITTED→SETTLING 与矩阵冲突；跨月 24 小时窗口不足 |
| I3 | OSS 月结 | 6.1、6.3 | **C** | line item 仍是聚合粒度，没有 object_key；十四残留旧 (tenant_id,month) 幂等说法 |
| I4 | Webhook 补偿 | 9bis.4.1 | **C** | outbox/拉取/重放已设计，但承诺与 ledger 同事务而源码支持 LOG_SQL_DSN 分库，根本矛盾未解决 |
| I5 | Admin Token 风险 | 9bis.6 | **A** | scope 拆分、IP allowlist、阀门、审计全部补上，是本次真实修复 |
| I6 | 凭据 DTO 兼容与密钥治理 | 8.2、九ter.4 | **B** | envelope encryption 方向正确，但 KEK 90 天轮换期未与 inflight 任务生命周期对齐 |
| I7 | 路由表达式安全 | 8.7 | **B** | 白名单、超时、上限均补上，但性能预算自相矛盾（5ms/50ms vs p99≤10ms） |
| M1 | Asynq 运维 | 9.5、9.6 | **B** | 运维配置已补，但示例状态冲突影响实现 |

**汇总：1 A、4 B、5 C，无 D。方向都对但执行不彻底。**

---

## 二、Critical 问题（v1.1 新引入或 v1 未解决）

### [Critical 1] 账本"唯一真相源"仍被余额表和 business_account.quota 稀释

**文档定位：** 三ter.1 / 三ter.3 / 9bis.7
**对应 v1 问题：** C1
**问题描述：** 文档声明 `business_account_ledger` 是唯一权威，但实际扣减以 `business_account_balance.available >= ?` 为门控；9bis.7 又给 `business_account` 增加 `quota/used_quota`。源码 `PreConsumeQuota` 仍是先读用户余额再扣，无原子边界。这是从"三套余额漂移"包装成"ledger + balance + business_account.quota + user/token 多套投影漂移"。
**潜在后果：** balance 漂移时 CAS 会按错误缓存放行或拒绝；business_account.quota 若被 UI/API 写入，会再次成为事实源。
**建议：** 删除 business_account.quota/used_quota，或标注为只读派生；business_account_balance 必须定义为 ledger 同步投影，任何 drift 触发账户冻结而非仅告警。

### [Critical 2] legacy_distributor 仍可穿透企业隔离

**文档定位：** 8.3 / 8.5
**对应 v1 问题：** C2
**问题描述：** 默认改 strict 是正确方向，但 global_pool/legacy_distributor 仍在策略枚举里，全局默认规则可配 legacy 兜底。对"企业必须独立凭据"模型，这仍是 fail-open，只是路径更长。
**潜在后果：** 企业专属请求在规则缺失或候选全不可用时落入共享通道。
**建议：** 增加 isolation_required 硬开关，企业隔离账号禁止 global_pool/legacy_distributor，除非 break-glass 并触发审计告警。

### [Critical 3] Outbox "与 ledger 同事务"承诺和 LOG_SQL_DSN 分库现实冲突

**文档定位：** 9bis.4.1
**对应 v1 问题：** I4
**问题描述：** 文档要求 ledger entry 与 webhook_event_outbox 同事务，但 new-api 源码 `model/main.go:213-218` 支持 LOG_SQL_DSN 独立日志库。若 outbox 被归入日志库，同事务承诺必然失效。
**潜在后果：** 出现"ledger 已扣费但事件永远拉不到"的财务黑洞。
**建议：** 明确 outbox 必须在主 DB、与 ledger 同库；日志/投递记录可异步复制到 LOG_DB，但不得承载补偿游标事实源。

### [Critical 4] 状态机示例与矩阵冲突，导致结算 CAS 永远失败

**文档定位：** 9.5 / 九ter.2
**对应 v1 问题：** I2、M1
**问题描述：** 9.5 示例在上游成功后 CAS `SUBMITTED→SETTLING`；九ter 状态机要求 `SUBMITTED→UPSTREAM_SUBMITTED→...→SETTLING`。这不是文字问题，是实现路径冲突，已进入 UPSTREAM_SUBMITTED 的任务用 9.5 示例 CAS 失败。
**潜在后果：** 预扣长期滞留，无法结算。
**建议：** 给出唯一状态转移表，9.5 示例修正为从终态 CAS 到 SETTLING。

### [Critical 5] 跨月任务 24 小时窗口不足以覆盖真实场景

**文档定位：** 九ter.6 / 9bis.5
**对应 v1 问题：** I2、I3
**问题描述：** 月结留 24 小时给跨月任务完成，但 billing.monthly_settle 在 9bis.5 事件清单里写的是每月 1 日推送。视频生成、上游排队、DLQ 人工恢复均可超过 24 小时。
**潜在后果：** 月结账单声称稳定但实际漏记，后续追补冲销污染账期。
**建议：** 月结关闭条件改为"账期内无未终结任务，或超时后生成 provisional + adjustment"，不能写死窗口。

---

## 三、Important 问题

### [Important 1] KEK 轮换期与 inflight 凭据版本生命周期未对齐

**文档定位：** 8.2 / 九ter.4
**对应 v1 问题：** I6
**问题描述：** 旧 KEK 保留 90 天；inflight 任务需用 ChannelCredVersion 解旧凭据继续；没有定义任务最长执行期、DLQ 保留期、人工 reconcile 窗口的联动规则。
**潜在后果：** 旧任务或补偿任务解密失败，上游轮询/取消/对账中断。
**建议：** 凭据版本保留期绑定"最长任务生命周期 + DLQ 保留 + 财务审计补偿窗口"。

### [Important 2] OSS 月结仍不是对象级不可变明细

**文档定位：** 6.1 / 6.3 / 十四
**对应 v1 问题：** I3
**问题描述：** 6.3 line item 按 (job_run, business_account, provider, bucket, storage_class) 聚合，不含 object_key；十四残留旧 (tenant_id, month) 幂等说法。
**潜在后果：** 单对象归属修正、迁移争议无法精确重放。
**建议：** 增加 object-level immutable item，聚合表做派生汇总；删除旧方案残留。

### [Important 3] P0 5-6 周估算偏乐观，范围未收敛

**文档定位：** 十三 / 十六
**问题描述：** P0 列出 E/A/B/C/D/F 六条工作流并行，E 是其余前置；十六清单覆盖 ledger、Asynq、routing、加密、Admin API、outbox、UI、测试、灰度。
**潜在后果：** 5-6 周只能完成框架薄切，财务正确性、隔离安全、三库兼容无法同时验收。
**建议：** P0 缩为"ledger + routing fail-closed + outbox 主库事务 + 最小 Admin API"，billingexpr v2/vp、OSS 月结、完整 UI 后移。

---

## 四、Minor / Nice-to-have 问题

### [Minor 1] 章节编号和旧编号残留

**文档定位：** 六、九ter、九bis、十一、十四
**问题描述：** 6.2 重复出现；九ter 在九bis 前；十一章下用 9.1/9.2 旧编号。
**建议：** 重排编号，清理旧方案残留。

### [Minor 2] 路由表达式性能预算自相矛盾

**文档定位：** 8.7.2 / 8.7.4
**对应 v1 问题：** I7
**问题描述：** 单规则 5ms、总 50ms，但总路由 p99 ≤10ms。
**建议：** 统一以 10ms 为运行期硬上限，去掉 50ms fallback 说法。

---

## 五、本次修订亮点（3 条）

1. **账本模型从补丁升级为结构性方案**：ledger + reserve/commit/release/refund 完整模式，方向正确。
2. **Admin Token 管控真正落地**：scope 拆分、IP allowlist、阀门、审计全补（9bis.6），是本次唯一评分 A 的条目。
3. **路由隔离从口头规则到执行器约束**：allowed_channel_ids 贯穿 affinity/random selection，fail-closed 默认已建立。

---

## 六、遗留核心问题（3 条）

1. **账务事实源未唯一化**：ledger、balance、business_account.quota 边界必须先收敛，否则一切账本设计都是沙上建楼。
2. **企业隔离缺硬策略**：legacy_distributor 不能与隔离账号共享一条降级链，需要隔离硬开关。
3. **异步财务关闭不可靠**：状态机冲突、跨月窗口不足、outbox 跨库事务三个问题需统一成一个可执行协议。

---

## 七、总体评分 7/10（与 v1 持平）

v1.1 在覆盖面上明显进步（账本、隔离、outbox 均有结构性新章节），Admin Token 是真实修复。但本次修订同时引入了 5 个新 Critical（账本多源、隔离逃生门、outbox 跨库、状态机冲突、跨月窗口），抵消了质量提升。判断：文档从"草图"升级到"结构完整但内部矛盾多"，需要再一轮专注收敛的 v1.2 才能达到可编码标准。

---

## 八、P0 编码准入判断：No

**最低准入标准（5 条，必须全部满足）：**

1. 删除或只读化 business_account.quota/used_quota，明确 ledger/balance 事务不变量。
2. 禁止企业隔离账号进入 global_pool/legacy_distributor，增加 isolation_required 硬开关。
3. 明确 outbox 表落主库与 ledger 同事务，LOG_SQL_DSN 分库只存投递日志副本。
4. 修正任务状态机和 9.5 示例，保证唯一状态转移表；补跨月未完成任务的 provisional/adjustment 规则，删除 24 小时硬窗口。
5. 重新切 P0 范围：只做 ledger + routing fail-closed + outbox 主库事务 + 最小 Admin API，billingexpr v2/vp、OSS 月结、完整 UI 后移 P1。

---

## 九、v1.2 修订处理表

| 问题 | 处理方式 | 用户决策 | 落点 |
|------|---------|---------|------|
| Critical 1 账本多源 | 删除 business_account.quota；balance 定义为 ledger 投影 + drift 触发账户冻结 | 全部按推荐 | v1.2 修订三ter / 9bis.7 |
| Critical 2 隔离逃生门 | 加 isolation_required 硬开关；禁用降级；break-glass 流程 | ✅ 同意 | v1.2 修订 8.3 |
| Critical 3 outbox 跨库 | outbox 强制主库；LOG_SQL_DSN 只能存副本 | ✅ 同意 | v1.2 修订 9bis.4.1 |
| Critical 4 状态机冲突 | 9.5 示例统一到唯一状态转移表 | 全部按推荐 | v1.2 修订 9.5 |
| Critical 5 跨月窗口 | 删除 24 小时硬窗口；改 provisional + adjustment 模式 | 全部按推荐 | v1.2 修订九ter |
| Important 1 KEK 与任务对齐 | KEK 保留期 = max(任务最长执行期, DLQ, 审计补偿) | 全部按推荐 | v1.2 修订 8.2 |
| Important 2 OSS object 级 | 加 object-level immutable item；聚合表降级派生 | 全部按推荐 | v1.2 修订六 / 十四 |
| Important 3 P0 范围收缩 | 收缩到 4 项；F/OSS/完整 UI 后移 P1+ | ✅ 同意 | v1.2 修订 P0 表 |
| Minor 1 章节编号 | 局部修正；完整重排留 v2.0 | — | v1.2 修订六/九ter/十一 |
| Minor 2 性能预算 | 统一到 10ms 硬上限 | — | v1.2 修订 8.7 |

---

*v1.2 修订完成后将进行第三轮 Codex 评审，目标达到 P0 准入标准。*

# 多媒体 AI 网关设计方案

> **版本：** v1.2.4 **(P0 准入版，10/10 Yes)** —— 2026-05-25 经 7 轮 Codex 独立评审最终拿到 Yes 准入；评审报告见 `docs/reviews/codex-review-{v1,v1.1,v1.2,v1.2.1,v1.2.2,v1.2.3,v1.2.4}.md`。
>
> **🟢 P0 编码状态：可立即启动**
> **范围：** 基于 `third-party/new-api` 二开，构建以多媒体（图像 / 视频 / 音频）模型为主、少量 LLM 为辅的商业化 AI API 网关。
> **核心目标：** 上游原始成本透明可审计，下游对客户支持「倍率 + 固定积分阶梯」两种计费方式，OSS / TOS 存储成本入账，所有计费规则在网关层可配置可叠加。
>
> **v1.2.3 Changelog（收尾 v1.2.2 自身引入 UPSTREAM_SUBMITTING 状态后漏改的 2 处同步项）：**
> - **[v1.2.2-1 修正]** `UPSTREAM_SUBMITTING` 加崩溃恢复机制：task 表新增 `submit_locked_until`，超时 cron 任务（每 1 分钟）把卡住的 `UPSTREAM_SUBMITTING` CAS 回 `SUBMITTED`（最多 3 次），超出转 `FAILED`（见 9.5 与 9ter.7）。
> - **[v1.2.2-2 修正]** 全局状态集合补 `UPSTREAM_SUBMITTING`：9ter.4 删除校验、9ter.6 月结 provisional 分类、9ter.8 `task_inflight_count` 维度全部纳入；9ter.2 状态图同步。
>
> **v1.2.2 Changelog（保留供追溯）：**
>
> **v1.2.2 Changelog（清理 v1.2.1 评审发现的 5 个实现级断点，最终达到 P0 编码准入；不动架构）：**
> - **[v1.2.1-1 修正]** `business_account_ledger.entry_type` 枚举补 `cashout` / `recharge_reversal`，与 SQL 示例对齐（见 3ter.2）。
> - **[v1.2.1-2 修正]** 删除 3ter.1 第 4 条「P0 上线时统一禁用并返回 410 Gone」旧表述，改为与 3ter.4 一致的「P0 内部转调 + T+60 天 410」分阶段（见 3ter.1）。
> - **[v1.2.1-3 修正]** 启动校验示例改为读 `OutboxDB().Dialector.(*postgres.Dialector).Config.DSN` 各自真实连接源，不再两边从同一 env 归一化（见 9bis.4.1）。
> - **[v1.2.1-4 修正]** SQLite outbox 扫描降级 CAS 补 expired delivering 抢占路径（见 9bis.4.1 与工作流 C-min）。
> - **[v1.2.1-5 修正]** `handleTaskSubmit` 改为「先 CAS 本地抢占再调上游」语义，且要求上游侧带 `idempotency_key` 保证多 worker 同时调上游时不重复创建任务（见 9.5）。
>
> **v1.2.1 Changelog（保留供追溯）：**
>
> **v1.2.1 Changelog（修复 v1.2 评审发现的 2 Critical + 3 Important + 1 Minor，最终达到 P0 编码准入）：**
> - **[v1.2-C1 修正]** 账本不变量改为 `available + reserved + used_total = recharge_total`（去掉 refund_total 项），refund_total 退化为审计字段不参与等式；refund 操作只 `available += r; used_total -= r`；新增「返还额度 vs 退款出账」概念区分（见 3ter.1 / 3ter.2 / 3ter.3）。
> - **[v1.2-C2 修正]** outbox P0 临时扫描改为 DB claim/lease 模式：扫描走 `SELECT FOR UPDATE SKIP LOCKED`（PG/MySQL）或 `pending → delivering` CAS（SQLite），加 `locked_by` / `locked_until` 字段；幂等 delivery key 防重复（见 9bis.4.1 与 工作流 C-min）。
> - **[v1.2-I1 修正]** 启动校验示例从「比较 GORM `*DB` 指针」改为「比较 normalized DSN + 强制 ledger/outbox 写入共享同一个 `*gorm.DB` 事务对象」（见 9bis.4.1）。
> - **[v1.2-I2 修正]** 旧 `user.quota` / `token.RemainQuota` 写入接口的 410 Gone 时点改为「release 期 60 天之后」；P0 阶段先做「内部转调 ledger recharge」过渡，老调用方无感切换（见 3ter.4 与 工作流 E）。
> - **[v1.2-I3 修正]** 9.5 补 `handleTaskSubmit` 示例（CAS `SUBMITTED → UPSTREAM_SUBMITTED` / `FAILED`）；9bis.5 清理旧 `billing.monthly_settle` 事件命名，统一为 `provisional` / `adjustment` / `finalized` 三件套（见 9.5 与 9bis.5 与 9ter.6）。
> - **[v1.2-M1 修正]** 6.3 / 6.3.7 明确「只有 line_item / adjustment 写 ledger，object_item 不直接扣费」，避免双层模型双重扣费风险。
>
> **v1.2 Changelog（保留供追溯）：**
>
> **v1.2 Changelog（修复 v1.1 引入的 5 个新 Critical + 3 个 Important，达到 P0 编码准入标准）：**
> - **[v1.1-C1 收敛]** 删除 `business_account.quota/used_quota` 字段；`business_account_balance` 定义为 ledger 严格投影（不是缓存），drift 触发账户冻结；账本真相源唯一化（见三ter.3 与 9bis.7 修订）。
> - **[v1.1-C2 收敛]** `business_account` 表新增 `isolation_required` 硬开关；启用时禁止 `fallback_policy ∈ {global_pool, legacy_distributor, next_rule}` 跨账号降级；break-glass 流程定义（见 8.3）。
> - **[v1.1-C3 收敛]** `webhook_event_outbox` 强制部署在主库，**不受 `LOG_SQL_DSN` 影响**；启动时 fail-fast 校验；LOG_DB 只能存投递日志副本（见 9bis.4.1）。
> - **[v1.1-C4 收敛]** 修正 9.5 Asynq handler 示例的状态机 CAS 路径，与九ter.2 状态转移表统一；给出唯一状态转移表（见 9.5 与九ter.2）。
> - **[v1.1-C5 收敛]** 删除「24 小时硬窗口」承诺；月结改用 **provisional + adjustment** 模式：账期内未完成任务先发 provisional 账单，完成后发 adjustment 事件冲销（见九ter.6）。
> - **[v1.1-I1 收敛]** KEK 保留期改为 `max(任务最长执行期, DLQ 保留期, 财务审计补偿窗口)`；定义任务最长执行期上限 30 天（见 8.2 与九ter.4）。
> - **[v1.1-I2 收敛]** OSS 月结新增 `storage_billing_object_item` 对象级不可变明细表；聚合表 `storage_billing_line_item` 降级为派生汇总；删除十四章残留旧方案（见 6.3.7）。
> - **[v1.1-I3 收敛]** P0 范围收缩到 4 项：账本 + routing fail-closed + outbox 主库事务 + 最小 Admin API；周期改 3-4 周；OSS 月结 / billingexpr v2-vp / 任务状态机 / 完整 UI 全部后移 P1-P2（见十三与十六）。
> - **[Minor 修订]** 局部修正章节编号问题（6.2 重复 / 十一章 9.1/9.2）；路由表达式性能预算统一到 10ms 硬上限。
>
> **v1.1 Changelog（保留供追溯）：**
> - **[Critical 1]** 新增「三ter、账本与原子扣减设计」章节：定义 `business_account_ledger` 单一真相源 + 原子条件更新 + reserved/used/refunded 不变量 + quota 字段统一迁移 BIGINT。
> - **[Critical 2]** 修订 8.3 Distributor：路由结果以 `allowed_channel_ids` 强制约束 affinity & random selection；规则加 `fallback_policy`（默认 `strict`，fail-closed）。
> - **[Important 1]** 修订 5.4 billingexpr：明确版本协议（v1/v2/vp 独立 runner，未知前缀拒绝），snapshot 必须含价格目录版本号/SKU 引用/汇率/usage；`vp` 跳过 GroupRatio 二次缩放。
> - **[Important 2]** 修订 8.2 ChannelCredentials：正式纳入 `ChannelOtherSettings` DTO，merge-preserving 更新；密钥 envelope encryption + `key_version` 字段。
> - **[Important 3]** 新增「九ter、任务财务状态机」章节：任务提交时双快照（授权 + 价格），跨月归属规则，suspend/delete 与 inflight 任务交互。
> - **[Important 4]** 修订 6.3 OSS 月结：不可变 line item + `job_run` 分阶段 apply + 归属缺失走人工待处理队列。
> - **[Important 5]** 修订 9bis.4 Webhook：outbox 模式 + `event_id` 单调递增 + `GET /events?since_id=` 拉取 + `POST /webhook-deliveries/{id}/replay` 重放。
> - **[Important 6]** 修订 9bis.6 Admin Token：IP allowlist / mTLS / 按动作拆 scope / 单日充值与账户创建额度阀门 / 充值幂等键含请求 hash。
> - **[Important 7]** 修订 8.7 路由表达式安全：前置抽取 normalized context + body 字段白名单 + 求值超时 + 规则数上限。
> - **[Minor 1]** 修订 9 章 Asynq：明确「队列是执行器、DB ledger 是真相源」+ 任务唯一键 + `IsMasterNode` 迁移矩阵 + Redis AOF 持久化要求。
> - P0 工作流新增「工作流 E：账本基础设施」（前置工作流 B/C/D）；周期从 4-5 周更新到 5-6 周。
>
> **基线决策（已与业务方确认）：**
> 1. **价格目录：** 手工维护 SKU 价目表 + 系统定时变价检测。
> 2. **积分精度：** 整数积分，1 积分 ≈ 1 分钱（基于现有 `int64 quota` + `QuotaPerUnit` 体系）。
> 3. **OSS / TOS 成本：** 按月对账精确结算（Inventory 拉取 + 月结 Job）。
> 4. **后端语言：** 沿用 Go，不更换；保留 new-api 已有架构与生态。
> 5. **异步任务调度：** P0 阶段即引入 [Asynq](https://github.com/hibiken/asynq)（Redis 后端，Go 原生），替换 new-api 现有的 DB 轮询任务调度。
> 6. **账号体系定位：** 网关不实现 organization / RBAC / 计费主体；这些在上游业务系统（多媒体创作平台）实现。网关对外表现为「**一个 Key 对应业务系统的一个企业账户**」。
> 7. **上游多凭据映射：** 一个对外 Key 在网关内部按请求参数路由到上游多套凭据（如火山 seedance 2.0 真人 / 仿真人 / 默认三套项目），通过新增「路由规则表 + 表达式」实现。
> 8. **路由规则配置粒度：** 全局默认 + 企业覆盖。
> 9. **多媒体渠道凭据结构：** seedance 这类渠道需要 API_KEY + 上游云服务 AK/SK（ARK / TOS 等）+ 存储桶 + 项目 ID 共 5 类配置；用结构化 `ChannelCredentials` 类型挂在 `Channel.OtherSettings`，SecretKey 必须 AES-GCM 加密。
> 10. **计费分层：** 网关侧负责「上游真实成本记录 + 基础积分定价 + 自有 quota 池」（必须，单一真相源）；业务系统侧可选实现「销售层二次加价」（合同价 / VIP 折扣 / 活动促销）。P1 阶段网关侧 1:1 透传，销售层留待 P4+ 视需要再加。
> 11. **依赖方向：** 业务系统调网关（基础设施被上层调用的正常依赖方向）。网关不感知营业执照等业务细节，只认 `business_account_id` 字符串。
> 12. **反向通知：** 用 Webhook 事件订阅模式（业务系统注册回调 URL + HMAC 签名），不构成代码层反向依赖；失败由 Asynq 重试队列保障，不阻塞主流程。
> 13. **【v1.1 新增】账本单一真相源：** `business_account_ledger` 不可变流水表是计费的唯一真相源；所有 quota 字段统一迁移到 `BIGINT/int64`；所有扣减走原子条件更新；`user.quota` / `token.RemainQuota` 退化为只读视图或限流标签，**不再参与对账**。
> 14. **【v1.1 新增】路由 fail-closed 默认：** 企业专属路由规则默认 `strict`（候选 channel 全不可用时直接报错，不回退共享池）；只有显式配置 `fallback_policy` 才允许向下一规则或全局池降级；`allowed_channel_ids` 在 distributor 全链路强制约束（含 affinity / random selection）。
> 15. **【v1.1 新增】ChannelCredentials 入 DTO：** 正式纳入 `dto.ChannelOtherSettings` DTO，避免 `SetOtherSettings` marshal 时丢字段；密钥治理用 envelope encryption + `key_version`（数据密钥 DEK 由 KMS / 主密钥 KEK 加密），支持平滑轮换。
> 16. **【v1.1 新增】billingexpr 版本协议严格化：** v1（LLM token）/ v2（USD 单次）/ vp（直接积分）三版本独立 runner、独立 quota conversion；未知前缀直接拒绝；`BillingSnapshot` 必须含价格目录版本号、SKU 引用列表、汇率、usage 全字段。
> 17. **【v1.1 新增】任务双快照：** 异步任务提交时冻结「授权快照（business_account_id / token_id / quota 状态 / 路由规则）」+「价格快照（billingexpr / 引用的 catalog 版本 / 汇率）」；结算时以快照为准，跨月任务按 **提交时刻** 归属账期；账户删除前置校验「无 inflight + 无未结余额」。
> 18. **【v1.1 新增】Webhook outbox 模式：** 事件按 outbox 表写入（`event_id` 单调递增）；推送失败由 Asynq 重试；业务系统可通过 `GET /events?since_id=` 主动拉取补偿；DLQ 提供后台重放接口；财务事件按审计周期（≥ 1 年）保留。
> 19. **【v1.2 新增】账本真相源唯一化：** `business_account_ledger` 是唯一真相源；`business_account_balance` 是 ledger 的严格投影（不是缓存），drift 触发账户冻结；`business_account.quota/used_quota` 字段**删除**；`user.quota` / `token.RemainQuota` 仅作 read-only 派生展示，**不允许写入**。
> 20. **【v1.2 新增】企业隔离硬开关：** `business_account.isolation_required` 字段；启用后该业务账户禁止任何形式的渠道降级（含 `global_pool` / `legacy_distributor` / `next_rule` 跨企业降级），只允许同企业 `next_rule` 降级；break-glass 需 Root 双人审批 + 24 小时窗口 + 全程审计。
> 21. **【v1.2 新增】outbox 强制主库：** `webhook_event_outbox` 必须部署在主数据库，与 `business_account_ledger` 同库同事务；启动时 fail-fast 校验（main DB schema 必须含 outbox 表）；LOG_SQL_DSN 拆库时 LOG_DB 只能存投递日志副本（`webhook_delivery_log`），不能存 outbox 本身。
> 22. **【v1.2 新增】跨月任务 provisional + adjustment：** 月结按 `submitted_at` 归属，**不写死 24 小时窗口**；账期内未终结的任务首次月结时发 `billing.monthly_settle_provisional`，任务完成后发 `billing.monthly_adjustment` 冲销；6 月才确认的 5 月任务由 5 月调整账单收尾。
> 23. **【v1.2 新增】KEK 保留期对齐：** 旧 KEK 保留期 = `max(任务最长执行期 30 天, Asynq DLQ 保留期, 财务审计补偿窗口 1 年)`；缺省 1 年；任务最长执行期硬上限 30 天，超时自动 EXPIRED 并退预扣。
> 24. **落地方式：** v1.2 已达 Codex P0 编码准入标准；P0 收缩到 3-4 周。

---

## 一、目标与非目标

### 目标

1. **多媒体优先**：图像（按分辨率 1K/2K/4K 分档）、视频（按 480P/720P/1080P + 时长分档）、音频（按时长）的计费第一类公民化。
2. **双轨计费**：
   - 「**官方模型 + Usage 字段**」：按上游真实用量，乘以一个倍率（含利润）扣积分；
   - 「**第三方平台**」：按模型 + 参数组合查固定积分表，与上游真实成本解耦。
3. **成本可审计**：每条调用日志能回答「这次调用，上游 SKU 是哪一条、原价多少、OSS 占了多少、利润多少」。
4. **规则可叠加**：基础价 + 渠道加价 + 租户加价 + 时段折扣 + 活动减免，通过表达式 DSL 组合，运营在管理后台编辑即可生效。
5. **复用 new-api**：用户 / 令牌 / 渠道 / 异步任务 / OAuth / 支付 / 仪表盘 / 多语言 / Web 控制台直接复用，不重造轮子。

### 非目标（一期不做）

- 不做"自动抓官方价目页"，价目人工维护 + hash 变价告警即可。
- 不做"按地域 / 按合约阶梯折扣"等复杂企业销售规则（留给后期版本）。
- 不替换 new-api 的核心架构（保持 Router → Controller → Service → Model 分层）。
- **不在网关侧实现** organization / 子账号 / RBAC / 计费主体 / 充值发票，这些归上游业务系统（多媒体创作平台）。
- 不更换后端语言。Go + Gin + GORM + Redis 的现有栈对该场景已是最优解，换语言意味着从零重写 new-api，没有额外收益。

---

## 二、需求 → new-api 能力映射

| 需求 | new-api 既有 | 需要新增 / 改造 |
|------|----------------|------------------|
| LLM 倍率计费 | `setting/ratio_setting/model_ratio.go` + `service/text_quota.go` | 直接用 |
| 异步任务生命周期 | `relay/relay_task.go` + `relay/channel/task/{kling,jimeng,sora,vidu,hailuo,suno,...}` | 直接用 |
| 任务级计费 | `service/task_billing.go`（目前是硬编码价格） | 改造：接入 `billingexpr` 路径 |
| 表达式 DSL | `pkg/billingexpr/`（基于 `expr-lang/expr`，支持 `tier()` / `param()` / `header()` / 时间函数 / `|||` 后置规则 / AST 自检 / `BillingSnapshot` 冻结） | **直接用**，扩展变量与函数 |
| 分档计费 | `setting/billing_setting/tiered_billing.go` + 前端 `TieredPricingEditor.jsx` | 改造：扩展 UI 支持「视频时长 × 分辨率」「图像分辨率」可视化 |
| 渠道分发 / 健康检测 | `middleware/distributor.go` + `service/channel_affinity.go` | 直接用 |
| 配额预扣 / 结算 | `service/pre_consume_quota.go` + `service/tiered_settle.go` + `BillingSnapshot` | 直接用 |
| 数据看板 | `router/dashboard.go` + `model/usedata*.go` | 扩展：新增 `provider_cost` / `storage_cost` / `profit` 维度 |
| 业务账户 ↔ 对外 Key | `Token` 表天然对应「一个 Key = 一个业务账户」，加 `business_account_id` 字段即可 | 小幅扩展 |
| 上游单一凭据 | `Channel` 已含 Key / BaseURL / Group / Tag / Priority / Weight / ParamOverride / HeaderOverride | 直接用 |
| 同一渠道多 Key 轮询 | `ChannelInfo.IsMultiKey` 多 Key 模式 + 状态机 | 直接用 |
| 注入企业专属上游参数（如火山 `project_id`） | `Channel.ParamOverride` / `HeaderOverride` 按 channel 独立配置 | 直接用 |
| 按 model + 用户分组选 channel | `middleware/distributor.go` | 直接用 |
| **按请求体参数（is_real_person / style 等）二级路由** | ❌ 无 | **新增**（模块 5） |
| **Token 绑定到特定 Channel 白名单** | ❌ Token 只能限模型，不能限 channel | **新增**（模块 5） |
| **业务账户 ID 穿透到日志 / 计费** | ❌ 无 | **新增**（模块 5） |
| **上游成本目录** | ❌ 无 | **新增**（模块 1） |
| **多媒体专属计费变量** | ❌ `billingexpr` 当前只有 token 变量 | **新增**（模块 2） |
| **OSS / TOS 月度对账** | ❌ 无 | **新增**（模块 3） |
| **价目变价检测** | ❌ 无 | **新增**（模块 4） |
| **专业任务队列（替换 DB 轮询）** | ❌ 现状是 gopool + DB 轮询 | **新增**（模块 6，Asynq） |

**结论：** 计费引擎本身（`billingexpr`）已经非常强，**真正缺失的是「事实层」**——上游花了多少钱、存了多少文件，把这两件事接进表达式即可让现有引擎覆盖所有诉求。

---

## 三、积分体系基线

| 项目 | 取值 |
|------|------|
| 内部 quota 类型 | `int64`（沿用 new-api 现状） |
| `QuotaPerUnit` | **500000**（即 $1 = 500000 quota，1 quota = 0.0002 美元 ≈ 0.0014 人民币 ≈ 0.14 分钱） |
| 用户可见积分单位 | 「**积分**」，前端展示时 `display = quota / 1000`（让 1 积分 ≈ 0.14 元，便于「生图 10 积分 = 1.4 元」这种直觉记账） |
| 货币结算单位 | 上游成本目录以**实际币种**入库（USD / CNY），结算时按当日汇率换算到 USD 后再换算 quota |
| 表达式输出单位 | 与现有 `billingexpr` 保持一致：表达式输出 = USD/1M token；任务类表达式输出 = USD（单次调用） |

> **为什么不用 decimal？** `billingexpr` 的 `quotaConversion()` 已经走 `int64`，全链路改 decimal 需要改 quota 池、token 估算、日志、前端展示、并发计数器（`atomic` 也需要替换），代价过高；而 `int64` 在 `QuotaPerUnit=500000` 下足够覆盖单次 ≥ 1e-10 USD 的精度，满足多媒体场景（多媒体单价远高于 LLM token）。

---

## 三bis、网关与业务系统的边界（计费分层架构）

> **设计要点：** 网关与业务系统是「基础设施 + 上层应用」的分层关系。所有涉及商业决策的事（销售、合同、活动）尽量留给业务系统，网关只负责"成本真相"。这一节定义两层各自的职责边界。

### 3bis.1 两层职责切分

```text
┌──────────────────────────────────────────────────────────────────┐
│ 业务系统侧 (多媒体创作平台,本设计不涉及实现)                    │
│                                                                  │
│ 【账号体系】                                                      │
│   - organization / team / 子账号 / RBAC                           │
│   - 合同主体 / 营业执照 / 税号 / 法人 / 发票                       │
│                                                                  │
│ 【销售层计费(可选,P4+ 阶段考虑)】                                 │
│   - 客户合同价、VIP 折扣、活动促销、套餐充值赠送                   │
│   - 计费公式: charged_quota × 销售倍率 + 销售加价                  │
│   - 最终用户余额扣减 / 发票 / 财务流水                             │
│                                                                  │
│ 【对客业务】                                                      │
│   - 创作工作台、模板库、协作、分享                                 │
│   - 调用网关 API,持有 K_X 这个企业账户专属 Key                    │
└──────────────────────────────────────────────────────────────────┘
                              ↑↓ HTTP API + Webhook
┌──────────────────────────────────────────────────────────────────┐
│ 网关侧 (本设计的对象)                                            │
│                                                                  │
│ 【账号映射】                                                      │
│   - Token 持有 business_account_id 字段                          │
│   - 不知道营业执照、不知道法人,只认字符串 ID                       │
│                                                                  │
│ 【上游成本层 (必须)】                                             │
│   - Provider Cost Catalog: 上游真实 SKU 单价                     │
│   - 每次调用记录 provider_cost_usd (上游真实花费,从 Usage 解析)   │
│   - OSS / TOS 月度对账                                            │
│                                                                  │
│ 【基础积分定价层 (必须)】                                         │
│   - billingexpr 表达式: 基于成本 + 加价 + 分档规则计算 charged_quota │
│   - 网关自有 quota 池: 业务账户先充值再消费                        │
│   - 单一真相源: 这是月底对账的铁打基线                             │
│                                                                  │
│ 【路由 / 调度层】                                                 │
│   - Channel Routing: 一个 Key 路由到多套上游凭据                  │
│   - Asynq 任务队列: 异步任务调度与并发控制                         │
└──────────────────────────────────────────────────────────────────┘
```

### 3bis.2 关键原则

1. **网关必须有自己的 quota 池**——业务系统先在网关侧"为业务账户充值"，网关按基础定价扣减；这样"网关账单 = 业务账户消费"形成铁打的对账基线。
2. **网关只能加价不能减价**——业务系统拿到 `charged_quota` 是底价（成本 + 网关利润），如要做减价让利由业务系统承担差额。
3. **真相单向流动**：成本与基础积分价的真相在网关；销售与最终用户余额的真相在业务系统。业务系统**只读不改**网关账单数据。
4. **签名防篡改**：网关 API 响应里的 `charged_quota` 与 `provider_cost_usd` 字段用业务系统注册时颁发的 secret 做 HMAC 签名，业务系统可校验，月底对账如发现差异立刻审计。

### 3bis.3 P1-P3 阶段不引入销售层

P1-P3 阶段网关侧暴露 `charged_quota`，业务系统**直接 1:1 透传**扣最终用户余额。这样：

- 网关与业务系统计费规则**只在网关一处**，零同步成本；
- 销售层留待业务量上来、运营提出明确销售策略需求时再加（P4+ 工作）；
- 即使加销售层，也只是业务系统侧的纯软件实现，不动网关。

### 3bis.4 三个常见误区

| 误区 | 真相 |
|------|------|
| "业务系统应该有自己的计费引擎，网关只是转发" | 网关必须有计费引擎——上游账单只发给网关，业务系统看不到真实成本；如果业务系统也算一遍，规则就在两边分裂，对账永远不准 |
| "网关应该感知客户是谁、给 VIP 打折" | 网关只认 `business_account_id`，不感知客户身份；VIP 折扣是销售决策，归业务系统 |
| "依赖网关 = 反向依赖很糟糕" | 业务系统调网关是天经地义的下层依赖；webhook 也不是反向依赖，是业务系统**主动订阅**网关事件 |

---

## 三ter、账本与原子扣减设计（v1.1 新增）

> **缘起：** Codex 评审 Critical 1 指出 new-api 现有 `user.quota`（int）+ `token.RemainQuota` 不是 int64、无原子条件更新、不是单一真相源。本设计若简单沿用，业务账户余额会和 user/token 余额三套数据漂移，并发扣减会超卖。本章定义独立的账本子系统，作为所有计费操作的唯一真相源。

### 3ter.1 设计原则（v1.2 修订：账本真相源唯一化）

> **v1.2 修订动因：** Codex v1.1 评审 Critical 1 指出 v1.1 引入了 ledger 但同时保留了 `business_account.quota/used_quota` + `business_account_balance` 缓存表 + `user/token.quota`，形成四源并立。v1.2 收敛为「ledger 是唯一真相源 + balance 是严格投影（不是缓存）+ 其他余额字段全部删除或只读派生」。

1. **唯一真相源（v1.2 强化）**：`business_account_ledger` 是所有扣减、退款、对账的**唯一**权威。任何与 ledger 不一致的字段都是 bug，必须修复 ledger 或重建投影。
2. **balance 是严格投影**（v1.2 改）：`business_account_balance` 不是缓存，是 ledger 的**严格投影**。drift 触发**账户冻结**（不是告警），运营介入排查后从 ledger 重建投影。这是为了让 drift 立即可见，而不是慢慢恶化。
3. **删除 `business_account.quota/used_quota` 字段**（v1.2 改）：模块 7 的 `business_account` 表**不再有** quota / used_quota 列；余额仅在 `business_account_balance` 投影表呈现。
4. **`user.quota` / `token.RemainQuota` 写入分阶段迁移**（v1.2.2 与 3ter.4 统一）：v1.1 说"仅展示不参与对账"；v1.2.2 修正为「**P0 阶段保留写入 + 内部转调 ledger**（对老 caller 无感）」+ 「**T+60 天 release 期后才返回 410 Gone**」+ 静态检查规则禁止新代码写入。它们由后台 job 从 ledger 定时同步用于展示。详见 3ter.4 分阶段时间线。
5. **不可变流水**：每一笔变动（充值 / 预占 / 结算 / 退款 / 冲销）都写一条 ledger entry，永不修改、永不删除。
6. **原子条件更新**：所有扣减操作走 `WHERE available >= ?` 的 CAS 模式，并发请求由数据库唯一约束 + 行锁兜底，**杜绝超卖**。
7. **三态余额**：`available`（可用） / `reserved`（预占，inflight 任务占用） / `used`（已结算）三态分离；任何时刻 **`available + reserved + used_total = recharge_total`**（v1.2.1 重新拍板的最简不变量，refund_total 不进等式，仅作审计字段；详见 3ter.2 的数学推导）。
8. **金额统一 BIGINT**：所有 quota 字段（`business_account_ledger.amount` / `business_account_balance.*` / Webhook payload / 日志 `charged_quota`）一律 `int64`；旧 `user.quota` / `token.RemainQuota` 在 v1.2 迁移期改为 `BIGINT`（只读用途）。

### 3ter.2 数据库设计

**主表 `business_account_ledger`（不可变流水，永不 UPDATE）：**

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | bigint PK | 自增 |
| `business_account_id` | varchar(64) NOT NULL | 索引 |
| `entry_type` | enum NOT NULL | `recharge` / `reserve` / `commit` / `release` / `refund` / `cashout` / `recharge_reversal` / `adjust` / `expire` (v1.2.2 补 `cashout` 退款出账与 `recharge_reversal` 充值冲销) |
| `amount` | bigint NOT NULL | 正数 = 入账，负数 = 出账（无论何种 entry_type，方向都明确） |
| `available_delta` | bigint NOT NULL | 对 available 余额的影响 |
| `reserved_delta` | bigint NOT NULL | 对 reserved 余额的影响 |
| `used_delta` | bigint NOT NULL | 对 used 累计的影响 |
| `reference_type` | varchar(32) | 关联实体类型：`task` / `chat_request` / `topup_order` / `manual_adjust` / `monthly_settle` |
| `reference_id` | varchar(64) | 关联实体 ID（如 `task_id`） |
| `correlation_id` | varchar(64) NULL | 关联同一笔业务流的多个 entry（如 reserve → commit / release） |
| `idempotency_key` | varchar(128) NULL UNIQUE | 防重复入账，如充值的 `external_ref` 或 `external_ref + body_hash` |
| `snapshot_billing_expr` | text NULL | 该笔扣减用的计费表达式（审计） |
| `snapshot_cost_refs` | text JSON NULL | 引用的 cost catalog 条目 ID + 版本 |
| `metadata` | text JSON NULL | 业务侧附加标签 |
| `created_at` | datetime NOT NULL | |
| `created_by` | varchar(64) | 操作来源（Admin Token / 系统 Job / 用户调用） |

**索引：**
- `(business_account_id, created_at DESC)`：账户流水查询
- `(idempotency_key)` UNIQUE：充值幂等
- `(correlation_id)`：reserve/commit/release 配对
- `(reference_type, reference_id)`：按业务实体反查

**辅表 `business_account_balance`（v1.2 改：ledger 严格投影，不是缓存）：**

| 字段 | 类型 | 说明 |
|------|------|------|
| `business_account_id` | varchar(64) PK | |
| `available` | bigint NOT NULL DEFAULT 0 | 可用余额 |
| `reserved` | bigint NOT NULL DEFAULT 0 | 预占余额（inflight 任务） |
| `used_total` | bigint NOT NULL DEFAULT 0 | 累计已用 |
| `recharge_total` | bigint NOT NULL DEFAULT 0 | 累计充值（含初始 + 后续，单调递增） |
| `refund_total` | bigint NOT NULL DEFAULT 0 | 累计退款（**正数**，记录退回总额） |
| `last_ledger_id` | bigint NOT NULL DEFAULT 0 | 已聚合到的最大 ledger.id（投影游标） |
| `frozen` | bool NOT NULL DEFAULT false | **v1.2 新增**：drift 检测命中后冻结账户 |
| `frozen_at` | datetime NULL | 冻结时间 |
| `frozen_reason` | varchar(256) NULL | 冻结原因（如 `ledger_drift_detected`） |
| `updated_at` | datetime | |
| `version` | int NOT NULL DEFAULT 0 | 乐观锁版本号 |

**关键不变量（v1.2.1 重新拍板，refund_total 不进等式）：**
```
available + reserved + used_total = recharge_total
```

含义：账户**累计入账总额** = 当前可用 + 当前预占中 + 累计实际消耗。

**为什么 refund_total 不进不变量？** 因为 refund 在网关侧的语义是**"把已记为 used 的额度退回 available"**，是 used 与 available 之间的内部转移，不改变账户的累计入账总额。`refund_total` 仅作为审计字段累计退回总额，便于运营对账。

**v1.2.1 修正前的错误：** v1.2 曾用 `available + reserved + used = recharge - refund`，但 refund 操作 `available++ used-- refund_total++` 会让左侧不变、右侧下降，等式破裂。修正后操作语义明确：

| 操作 | 等式左侧增量 | 等式右侧增量 | 是否守恒 |
|------|------------|------------|---------|
| recharge(+r) | available+r → left +r | recharge+r → right +r | ✅ |
| reserve(+e) | available-e, reserved+e → left 0 | 0 | ✅ |
| commit(+a) | reserved-a, used+a → left 0 | 0 | ✅ |
| release(+e) | reserved-e, available+e → left 0 | 0 | ✅ |
| refund(+r) | available+r, used-r → left 0 | 0（refund_total++ 不进等式） | ✅ |

**"返还额度"与"退款出账"两类语义（v1.2.1 新增澄清）：**

| 概念 | 描述 | ledger entry 类型 | 等式影响 |
|------|------|------------------|---------|
| **返还额度（credit refund）** | 网关把已扣的额度退回 available，客户可继续消费 | `entry_type='refund'`，available++ used-- | 不变 ✅（最常见，本系统 99% 退款属于此类） |
| **退款出账（cash refund）** | 业务系统决定彻底退款给客户，把钱退离系统（账户余额减少） | `entry_type='cashout'` 或 `'recharge_reversal'`，recharge_total-- available-- | 不变 ✅（罕见，仅财务退订时用） |

**reconcile job 必须校验上面不变量**：drift 检测发现 `available + reserved + used_total ≠ recharge_total` 时立即冻结账户（见前面的 frozen 字段）。

可由后台 reconcile job 每 5 分钟对账（按 ledger 重算 balance），不一致时**冻结账户**（不再是仅告警）。

**Drift 检测与处理（v1.2 新增）：**

```
后台 reconcile job 每 5 分钟运行:
  for each business_account:
    expected = compute_from_ledger(business_account_id, last_ledger_id_at_time_of_check)
    actual = SELECT * FROM business_account_balance WHERE business_account_id = ?
    if expected != actual:
      # 立即冻结账户,阻断所有新预占
      UPDATE business_account_balance
      SET frozen = true,
          frozen_at = NOW(),
          frozen_reason = 'ledger_drift_detected: expected=... actual=...'
      WHERE business_account_id = ?
      # 触发 critical 告警 + Webhook account.frozen
      # 运营介入排查,从 ledger 重建投影后人工解冻
```

冻结期间：所有 reserve 操作直接拒绝（return 412 account_frozen），webhook 推送 `account.frozen`；commit / release（已 reserved 的）允许继续完成，避免 inflight 任务卡死。

**投影重建脚本：**
```
service/balance_rebuild.go (新增):
  REPLAY 所有 ledger entries → 重算 balance → 替换 business_account_balance 行
  (单账户操作,需运营手工触发,过程中账户暂时不可用)
```

### 3ter.3 关键操作模式

**充值（recharge）—— 业务系统调用：**
```sql
-- 幂等检查
SELECT id FROM business_account_ledger
WHERE idempotency_key = :external_ref_hash;
-- 命中 → 直接返回原结果

-- 不命中 → 事务内执行
BEGIN;
  INSERT INTO business_account_ledger
    (business_account_id, entry_type, amount, available_delta,
     reserved_delta, used_delta, reference_type, reference_id,
     idempotency_key, metadata, created_at, created_by)
  VALUES (..., 'recharge', :quota, :quota, 0, 0,
          'topup_order', :ref, :external_ref_hash, ..., NOW(), ...);

  UPDATE business_account_balance
  SET available = available + :quota,
      recharge_total = recharge_total + :quota,
      last_ledger_id = LAST_INSERT_ID(),
      version = version + 1,
      updated_at = NOW()
  WHERE business_account_id = :biz_id
    AND version = :expected_version;
  -- 影响 0 行 → 重试整个事务
COMMIT;
```

**预占（reserve）—— 请求进入时：**
```sql
BEGIN;
  -- 原子条件扣减,防超卖
  UPDATE business_account_balance
  SET available = available - :estimated_cost,
      reserved = reserved + :estimated_cost,
      version = version + 1
  WHERE business_account_id = :biz_id
    AND available >= :estimated_cost;
  -- 影响 0 行 → 余额不足,拒绝请求

  INSERT INTO business_account_ledger
    (..., entry_type='reserve', amount=-:estimated_cost,
     available_delta=-:estimated_cost, reserved_delta=+:estimated_cost,
     correlation_id=:request_id, snapshot_billing_expr=..., ...);
COMMIT;
```

**结算（commit）—— 上游响应回来 / 任务完成：**
```sql
BEGIN;
  -- 先释放预占
  -- 再按真实 usage 计费
  -- 写两条 ledger:release(reserved-) + commit(used+)
  -- 差额若为正 → 从 available 再扣
  -- 差额若为负 → 退还到 available
  ...
COMMIT;
```

**返还额度（refund / credit refund）—— 上游故障 / 客户投诉 / 人工冲销，最常见：**
```sql
-- v1.2.1 修订:不变量守恒,refund_total 仅审计
INSERT INTO business_account_ledger
  (..., entry_type='refund', amount=+:refund_amount,
   available_delta=+:refund_amount,
   used_delta=-:refund_amount,                          -- v1.2.1 新增 used 减回
   reference_type='manual_adjust' / 'task' / 'monthly_settle',
   created_by='admin:xxx', metadata={"reason": "..."});
UPDATE business_account_balance
  SET available = available + :refund_amount,           -- 可用增加
      used_total = used_total - :refund_amount,         -- 已用减回
      refund_total = refund_total + :refund_amount,     -- 仅审计累计,不进不变量
      version = version + 1
  WHERE business_account_id = :biz_id;
-- 验证: 左 = (available+r) + reserved + (used-r) = 不变 ✅
--       右 = recharge = 不变 ✅
```

**退款出账（cashout / 罕见）—— 业务系统决定彻底退款给客户，把钱离开系统：**
```sql
-- v1.2.1 新增:用于业务系统订阅退订等场景,从总账户余额扣减
INSERT INTO business_account_ledger
  (..., entry_type='cashout', amount=-:cashout_amount,
   available_delta=-:cashout_amount,
   reference_type='subscription_cancel' / 'manual_cashout',
   created_by='admin:xxx');
UPDATE business_account_balance
  SET available = available - :cashout_amount,          -- 可用减少
      recharge_total = recharge_total - :cashout_amount, -- 累计入账抵消
      version = version + 1
  WHERE business_account_id = :biz_id
    AND available >= :cashout_amount;                   -- 防止扣到负数
-- 验证: 左 = (available-r) + reserved + used = 左 -r
--       右 = (recharge-r) = 右 -r ✅
```

**跨库 SQL 注意（v1.2 新增）：** 上面示例用 MySQL 风格的 `LAST_INSERT_ID()`，PG 没有这个函数。实际实现按 `CLAUDE.md` Rule 2 走 GORM 抽象，三库各自用：
- MySQL: `LAST_INSERT_ID()`
- PostgreSQL: `RETURNING id`
- SQLite: `last_insert_rowid()`

GORM 的 `Create` 方法自动处理这种差异，业务代码不直接写 SQL。

> **「网关只能加不能减」的修订：** v1 版本的硬规则在退款场景过于刚性；v1.1 修正为「**网关可以发起退款，但必须有明确的退款来源（task_failure / manual_adjust / monthly_settle）且写入 ledger 可审计**」。退款不破坏对账：网关侧 ledger 减去 used、加回 available，业务系统通过 Webhook 收到 `account.refunded` 事件后自行调整最终用户账单。

### 3ter.4 与 new-api 现有 quota 的兼容（v1.2.1 修订：分阶段迁移，避免运营断流）

> **v1.2.1 修订动因：** Codex Important 2 指出 v1.2 写「API/UI 写入全部禁用并返回 410 Gone」过激，容易切断现有控制台的运营充值流程。改为「**P0：内部转调 ledger，对老调用方无感；T+60 天：才真正 410**」的分阶段过渡。

**分阶段时间线：**

```
P0 上线 (Day 0):
  ├─ ledger 表 + balance 表 + Recharge/Reserve/Commit/Release/Refund 五个核心 API 上线
  ├─ 旧 controller (POST /user/{id}/topup 等) 改造为「内部转调 ledger.Recharge」适配层
  ├─ user.quota / token.RemainQuota 字段保留 BIGINT,继续被旧代码读
  └─ 新增 sync job 每 5 分钟从 ledger 同步 user.quota / token.RemainQuota 用于管理后台展示
       (注:此时若有老代码直接 UPDATE user.quota,会被 sync job 覆盖,业务感知"我改的没保存",这就是预警信号)

T+30 天 (warning 期):
  ├─ 所有 controller 转调验证稳定后,旧字段 UPDATE 接口加 Deprecated header + warning 日志
  ├─ 不阻断,仅告警
  └─ PR review 卡控:新代码禁止写 user.quota / token.RemainQuota (lint 规则)

T+60 天 (release 期之后):
  ├─ 老接口 UPDATE 路径返回 410 Gone
  ├─ 管理后台 UI 全部切到 ledger 充值入口
  └─ sync job 仍持续(老字段作为只读展示视图)

T+90 天 (可选):
  └─ 完全删除 user.quota / token.RemainQuota 字段(若没有外部依赖)
```

**P0 阶段的处置表（v1.2.1）：**

| new-api 字段 | P0 处置 | T+60 天处置 |
|--------------|---------|-------------|
| `user.quota` (int → BIGINT) | 字段保留；读路径不变；写路径**内部转调 ledger**，sync job 反向同步给展示用 | 写接口 410 Gone |
| `user.used_quota` (int → BIGINT) | 同上 | 同上 |
| `token.RemainQuota` (int → BIGINT) | 同上 | 同上 |
| `token.UsedQuota` (int → BIGINT) | 同上 | 同上 |
| `channel.UsedQuota` (int64) | 不变，用于渠道维度统计（与账户余额无关） | 不变 |
| `service/pre_consume_quota.go` | **P0 重写**：完全走 `business_account_ledger.reserve()`，不再读写 user/token quota | 不变 |
| `service/quota.go` | **P0 重写**：commit / release / refund 全部走 ledger | 不变 |
| 现有 controller `POST /user/{id}/topup` / `PUT /token/{id}` 等 | **P0 内部转调**：保留外部 API 不变，内部改调 ledger（对 caller 无感）；写 deprecated header | T+60 天 410 |

**P0 强约束：** PR review 必须确认**新代码**无任何 `INSERT/UPDATE user.quota` 或 `user.used_quota`；老代码（controller 适配层除外）的写入也要在 P0 阶段全部清理；引入静态检查规则（grep / golangci-lint custom check）卡控。**仅适配层允许写**，且必须有注释 `// COMPAT: ledger sync mirror`。

**内部转调适配层示例：**

```go
// controller/user.go (现有 controller,改造内部实现)
func TopUpUser(c *gin.Context) {
    userID := c.Param("id")
    var req TopUpRequest
    c.ShouldBindJSON(&req)

    // v1.2.1: 内部转调 ledger,外部 API 不变
    // 找到该 user 对应的 business_account_id (P0 期间可能是 legacy_user_<id> 占位)
    bizAccountID := resolveLegacyUserToBizAccount(userID)

    err := ledger.Recharge(c, bizAccountID, req.Quota, "legacy_topup:"+req.RefID)
    if err != nil {
        c.JSON(500, gin.H{"error": err.Error()})
        return
    }

    // 加 deprecated 提示
    c.Header("Deprecation", "T+60d: use POST /admin/api/business-accounts/{biz_id}/recharge")
    c.JSON(200, gin.H{"success": true})
}
```

**迁移策略（v1.2 修订）：**

引入 ledger 时启动 `data migration job`：

```
for each user in user table:
  business_account_id = "legacy_user_" + user.id  // 占位 ID
  写 ledger entry:
    entry_type = 'recharge'
    amount = +user.quota
    reference_type = 'migration_init'
    metadata = {"legacy_user_id": user.id, "migrated_at": "..."}
  写 ledger entry:
    entry_type = 'commit'
    amount = -user.used_quota
    used_delta = +user.used_quota
    reference_type = 'migration_init'
  写 business_account_balance:
    available = user.quota - user.used_quota
    used_total = user.used_quota
    recharge_total = user.quota
```

**`legacy_user_<id>` 占位账户的后续处理：**

业务系统在调 `POST /admin/api/business-accounts` 时可传 `legacy_user_id` 参数主动接管：

```
POST /admin/api/business-accounts
{
  "business_account_id": "biz_001",
  "legacy_user_id": 42,         // v1.2 新增,接管原 user 的余额
  ...
}
```

接管动作触发：
1. 验证 legacy_user 余额未被业务系统 ID 占用
2. 写 ledger transfer entry：从 `legacy_user_42` 转到 `biz_001`
3. 标记 `legacy_user_42` 为 archived
4. 后续 user 42 的 Token 调用都重定向到 `biz_001`

未被业务系统接管的 `legacy_user_*` 账户继续保留余额，但**不再支持充值**（防止双轨账户）。

### 3ter.5 高并发并发性能

**单账户高并发场景：** 同一企业账户在毫秒内提交大量请求，所有 reserve 都竞争同一行 `business_account_balance` 的乐观锁，QPS 上限受限。优化方案：

1. **预留池模式（P2 优化）**：账户余额按租户预先切分到 N 个子池（如 N=16），请求按 `request_id % N` 落到子池；reserve 在子池内 CAS，跨子池失败时再向相邻子池借调。
2. **批量结算（P2 优化）**：高频小额扣减（如 LLM token 计费）改用「先记账后聚合」，每秒聚合一次写 ledger，期间用 Redis 原子计数器扛 QPS。多媒体单价高、QPS 低，可不优化。

P1 阶段先实现简单乐观锁版本，跑稳定后视压测结果决定是否引入预留池。

### 3ter.6 对账保障

| 对账层级 | 频率 | 内容 |
|---------|------|------|
| **balance vs ledger** | 每小时 | 重算 `business_account_balance`，与 ledger 聚合比对，不一致告警 |
| **网关账单 vs 业务系统账单** | 每日（Webhook `billing.daily_summary`） | 业务系统侧对照自己的最终用户消费明细 |
| **上游账单 vs 网关 provider_cost** | 每月（人工） | 运营在管理后台拉网关侧 `provider_cost` 累计，与火山 / OpenAI 月度发票比对 |
| **OSS 月结对账** | 每月 1 日（自动） | 见模块 3 |

### 3ter.7 P0 工作流

详见模块 7 之后的「分阶段实施路线」与「P0 落地清单」新增的**工作流 E：账本基础设施**。**工作流 E 是 B / C / D 的前置依赖**——账本不立起来，其他工作流的扣费动作没有真相源可写。

---

## 四、模块 1：Provider Cost Catalog（上游成本目录）

### 4.1 设计原则

- **「事实表」**：只记录上游真实定价，不掺杂自家加价规则；
- **与 `model_ratio` 解耦**：现有 LLM 倍率配置不变，新表只覆盖**多媒体 SKU** 和**官方 LLM 原价**两类；
- **版本化**：单价生效区间 `effective_from / effective_to` 严格区间，结算时按调用时间命中条目；
- **可审计**：每次结算日志记录引用的 SKU id + 版本号。

### 4.2 数据库表

**主表 `provider_cost_catalog`**（运营手工维护）：

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | bigint PK | |
| `provider` | varchar(64) | `openai` / `anthropic` / `google_vertex` / `bytedance_jimeng` / `alibaba_wanxiang` / `kling` / `volcengine_tos` / `aliyun_oss` / 第三方平台名 |
| `sku_type` | enum | `llm_token` / `image` / `video` / `audio` / `storage` / `egress` |
| `sku_key` | varchar(255) | 业务键，例：`seedance-2.0:1080p:per_sec` / `gpt-image-2:2048x2048:high_quality` / `tos:standard:gb_month` |
| `unit` | enum | `per_1m_input_token` / `per_1m_output_token` / `per_image` / `per_video_second` / `per_audio_second` / `per_request` / `per_gb_month` / `per_gb_egress` |
| `unit_price` | decimal(18,8) | 单价 |
| `currency` | char(3) | `USD` / `CNY` |
| `effective_from` | datetime | 生效起 |
| `effective_to` | datetime NULL | 生效止（NULL = 当前有效） |
| `source_url` | varchar(512) | 官方价目页快照 URL |
| `source_snapshot_md5` | char(32) | 抓回的官方页面 hash |
| `source_last_checked_at` | datetime | 上次比对时间 |
| `notes` | text | 备注（合约编号、商务联系人等） |
| `created_by` / `updated_by` / `created_at` / `updated_at` | 审计字段 | |

**索引：** `(provider, sku_key, effective_from)` 唯一；`(sku_key, effective_from, effective_to)` 用于结算时查询。

**变价告警表 `provider_cost_alert`**：

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | bigint PK | |
| `catalog_id` | bigint FK | 关联 catalog 条目 |
| `detected_at` | datetime | 发现变价时间 |
| `old_md5` / `new_md5` | char(32) | hash 对比 |
| `status` | enum | `pending` / `confirmed` / `ignored` |
| `handled_by` / `handled_at` | 处理人 | |

### 4.3 跨库兼容

按 `CLAUDE.md` Rule 2 实现：
- `decimal` 字段：MySQL/PG `DECIMAL(18,8)`；SQLite 用 `TEXT` 存字符串，读时用 `shopspring/decimal.NewFromString`。
- `enum` 字段：统一用 `varchar(32)` + 应用层校验（GORM 的 enum 在三库支持差异大）。
- 时间字段统一 `datetime` + UTC。

### 4.4 变价检测 Job

**位置：** `service/cost_catalog_sync.go`（新增），由 `main.go` 启动后台 goroutine：

```
每天凌晨 03:00 (Asia/Shanghai) 触发：
  for each catalog entry with source_url != "":
    fetch(source_url) → html
    new_md5 = md5(html)
    if new_md5 != source_snapshot_md5:
      insert into provider_cost_alert (catalog_id, old_md5, new_md5, pending)
      send notification via service/user_notify.go (邮件 / Webhook / Uptime-Kuma)
    update source_last_checked_at
```

**告警通知模板：**
```
[变价告警] Provider: bytedance / SKU: seedance-2.0:1080p:per_sec
旧 hash: abc123... → 新 hash: def456...
请在管理后台「成本目录 > 变价告警」核对并新增 effective_from = <今天> 的新版本。
原始页面: <source_url>
```

### 4.5 管理后台 UI（落在 `web/default/src/features/system-settings/cost-catalog/`）

- **列表页**：按 provider + sku_type 筛选，显示当前生效价格、上次校验时间、是否有未处理变价告警。
- **编辑页**：新增 / 修改条目；修改时强制新建一条 `effective_from = 今天` 的新版本，旧版本自动写 `effective_to = 今天 - 1秒`（保持历史可追溯）。
- **变价告警面板**：列出所有 `pending` 告警，"已处理"按钮关闭告警。

---

## 五、模块 2：多媒体计费表达式扩展

### 5.1 新增变量

在 `pkg/billingexpr/compile.go` 的编译环境中追加：

| 变量 / 函数 | 类型 | 含义 | 数据来源 |
|---|---|---|---|
| `usage(key string)` | float64 | 读上游 Usage 字段中的实际数值 | 各 channel adaptor 在 `fetch` / 响应解析时填入 `dto.Usage.Multimedia` |
| `cost(sku_key string)` | float64 | 查 Provider Cost Catalog 当前生效单价（统一换算为 USD） | `provider_cost_catalog` |
| `storage_cost(file_id string)` | float64 | 查 OSS 文件预估成本（USD），轻量方案下= `size_gb * cost("tos:standard:gb_month") * estimated_months` | `oss_object_meta` + catalog |
| `param(path string)` | any | （已有，复用）读请求 body 字段 | gjson |
| `header(key string)` | string | （已有，复用） | |
| `tier(name string, value float64)` | float64 | （已有，复用）记账分档 | |

**`usage()` 常用 key 约定：**

| key | 含义 | 适用 |
|-----|------|------|
| `usage("video_seconds")` | 视频实际时长（秒） | 视频任务 |
| `usage("video_resolution")` | 视频实际分辨率（"480p" / "720p" / "1080p" / "4k"） | 视频任务 |
| `usage("image_count")` | 实际生成图片数 | 图像任务 |
| `usage("image_resolution")` | 图像实际分辨率（"1024x1024" 等） | 图像任务 |
| `usage("audio_seconds")` | 音频时长 | TTS / STT |
| `usage("output_file_ids")` | 输出文件 id 列表（JSON 字符串） | 配合 `storage_cost` |
| `usage("prompt_tokens")` / `usage("completion_tokens")` | LLM token（向后兼容现有 `p`/`c`） | 通用 |

### 5.2 `dto.Usage` 扩展

新增结构（不影响现有字段，向后兼容）：

```go
// dto/openai_response.go (或新增 dto/multimedia_usage.go)
type MultimediaUsage struct {
    VideoSeconds     float64           `json:"video_seconds,omitempty"`
    VideoResolution  string            `json:"video_resolution,omitempty"`
    ImageCount       int               `json:"image_count,omitempty"`
    ImageResolution  string            `json:"image_resolution,omitempty"`
    AudioSeconds     float64           `json:"audio_seconds,omitempty"`
    OutputFileIDs    []string          `json:"output_file_ids,omitempty"`
    ExtraKV          map[string]any    `json:"extra_kv,omitempty"`
}

type Usage struct {
    // ... 现有 token 字段保持不变 ...
    Multimedia *MultimediaUsage `json:"multimedia,omitempty"`
}
```

各 `relay/channel/task/*/adaptor.go` 的 `fetch` 阶段必须把上游回执标准化到此结构。

### 5.3 表达式示例

**生图（按分辨率固定积分，第三方平台典型）：**
```
v1:
  param("size") == "1024x1024" ? tier("1k", 10) :
  param("size") == "2048x2048" ? tier("2k", 30) :
  param("size") == "4096x4096" ? tier("4k", 100) :
  tier("default", 10)
```
> `tier()` 输出值的单位是 USD/1M（已有约定）；当结算单元是「单次调用」时，约定输出为 USD × 1e6，这样 `10` 即 `10 USD / 1M = 1e-5 USD/次 = 0.005 积分`？不直观。
>
> **修订：为多媒体新增一个表达式版本 `v2:`**，约定输出直接是 USD（单次结算），quota 转换为 `output * QuotaPerUnit * groupRatio`。版本号通过表达式前缀切换，`pkg/billingexpr/run.go` 已有版本派发机制（`v1:` 默认），新增 `v2:` 走单次计费换算。这样上面例子在 `v2:` 下：
```
v2:
  param("size") == "1024x1024" ? tier("1k", 0.014) :  -- $0.014 = 10 积分（按 QuotaPerUnit=500000 → quota=7000 → 显示 7 积分？得调）
  ...
```
> **再次修订（最终方案）：** 引入「**直接积分模式**」表达式版本 `vp:`（p = points），表达式输出**直接是 quota**（int64），跳过 USD 换算。这样运营写「10」就是 10 quota，与显示积分按 `display = quota / 1000` 折算（让 10000 quota = 10 积分）。

**最终采用三种表达式版本：**

| 版本前缀 | 输出单位 | 适用场景 |
|---|---|---|
| `v1:` | USD/1M token | LLM token 计费（沿用现有） |
| `v2:` | USD（单次） | 官方多媒体 + Usage 倍率定价 |
| `vp:` | quota（int64，直接积分） | 第三方平台固定积分定价 |

**重新写示例：**

```
# 生图（第三方平台，固定积分）
vp:
  param("size") == "1024x1024" ? tier("1k", 10000) :
  param("size") == "2048x2048" ? tier("2k", 30000) :
  param("size") == "4096x4096" ? tier("4k", 100000) :
  tier("default", 10000)
```

```
# 视频（官方模型，按上游成本 + 倍率 + OSS）
v2:
  tier(
    usage("video_resolution") + ":" + string(usage("video_seconds")) + "s",
    cost("seedance:" + usage("video_resolution") + ":per_sec")
      * usage("video_seconds")
      * 1.30                                  -- 30% 利润倍率
    + storage_cost(usage("output_file_ids"))  -- OSS 预估
    + 0.002                                   -- 平台固定加价 $0.002
  )
```

```
# LLM（沿用 v1，保持向后兼容）
v1: tier("base", p * 2.5 + c * 15 + cr * 0.25)
```

```
# 时段折扣（已有 |||后置规则）
v2: tier("base", cost("kling:1080p:per_sec") * usage("video_seconds") * 1.5)
||| weekday(0) in [0, 6] ? 0.8 : 1.0   -- 周末 8 折
```

### 5.4 表达式编译器扩展点（v1.1 修订：版本协议严格化）

> **v1.1 修订动因：** Codex Important 1 指出 v2/vp 不是局部加函数，而是计费语义变化；现有 `pkg/billingexpr` 只识别 `v1:` 前缀，运行环境只注册 v1 函数，表达式结果固定 `float64`；snapshot 里没有 `CostRefs`。如果实现不清，结算重放无法复现，`vp` 直接积分还会被 `GroupRatio` 二次缩放。本节修订为「严格版本协议 + 独立 runner + 完整 snapshot」。

**5.4.1 版本协议（严格）**

```text
表达式字符串前缀规则:
  ┌─ "v1:" / 无前缀 ─→ v1 runner (LLM token 计费,沿用现有)
  ├─ "v2:"         ─→ v2 runner (多媒体 USD 单次计费)
  ├─ "vp:"         ─→ vp runner (直接积分计费)
  └─ 其他前缀      ─→ 编译期 hard error,拒绝保存
```

**关键约束：**
- **未知版本前缀直接拒绝**（编译期 + 保存时 + 运行时三层校验，运行时见到未知前缀视为系统配置错误，**不降级**，直接 503）
- **v1 / v2 / vp 各自有独立的 `Runner` 实现**：独立编译环境（变量集、函数集）、独立返回类型校验、独立 `quotaConversion`
- **跨版本不允许互相引用**，例如 v1 表达式不能用 `cost()`（v2/vp 专属）

**5.4.2 三个版本的 Runner 对比**

| 维度 | v1 (LLM) | v2 (多媒体 USD 单次) | vp (直接积分) |
|------|----------|---------------------|---------------|
| 变量集 | `p` `c` `cr` `cc` `cc1h` `img` `ai` `ao` `img_o` `len` | `usage(key)` + `cost(sku_key)` + `storage_cost(file_id)` + `param()` + `header()` | 同 v2 |
| 函数集 | `tier()` `param()` `header()` 时间函数 | + `cost()` `storage_cost()` `usage()` `string()` | 同 v2 |
| 返回类型 | `float64`（USD/1M token） | `float64`（USD/单次） | `float64`（直接 quota，须 ≥ 0 且整数语义） |
| quotaConversion | `output / 1e6 * QuotaPerUnit * groupRatio` | `output * QuotaPerUnit * groupRatio` | `int64(output) * groupRatio`（**仅乘 groupRatio，不再乘 QuotaPerUnit**） |
| 典型用法 | `v1: tier("base", p*2.5 + c*15)` | `v2: tier("1080p:10s", cost("seedance:1080p:per_sec")*usage("video_seconds")*1.3)` | `vp: param("size")=="1024x1024" ? tier("1k", 10000) : tier("4k", 100000)` |

**关于 `vp` 与 `GroupRatio` 的明确约定：**

| 选项 | 决策 |
|------|------|
| `vp` 仍乘 `groupRatio` | ✅ 采纳（保留运营按租户分组打折/加价的能力） |
| `vp` 不乘 `groupRatio` | ❌ 否决（会让企业覆盖的折扣失去入口） |
| `vp` 还乘 `QuotaPerUnit` | ❌ 否决（vp 输出本就是 quota，再乘会数量级错乱） |

**5.4.3 BillingSnapshot 内容（v1.1 扩展）**

预扣阶段冻结的 snapshot 必须包含**结算所需的全部上下文**，结算阶段不能依赖任何外部"最新值"：

```go
// pkg/billingexpr/types.go (扩展)
type BillingSnapshot struct {
    // 现有字段
    ExprVersion       string          `json:"expr_version"`       // "v1" / "v2" / "vp"
    ExprText          string          `json:"expr_text"`          // 原始表达式字符串
    CompiledExprHash  string          `json:"compiled_expr_hash"` // 编译后 AST hash,验证一致性
    GroupRatio        float64         `json:"group_ratio"`
    QuotaPerUnit      int64           `json:"quota_per_unit"`

    // v1.1 新增
    CostRefs          []CostRef       `json:"cost_refs,omitempty"`        // 引用的 cost catalog 条目
    StorageRefs       []StorageRef    `json:"storage_refs,omitempty"`     // 引用的 storage 配置
    FxRate            map[string]float64 `json:"fx_rate,omitempty"`       // 汇率快照: {"usd_cny": 7.2}
    UsageInputs       map[string]any  `json:"usage_inputs,omitempty"`     // 结算时用的 usage 字段值
    EvalTimestamp     int64           `json:"eval_timestamp"`             // 求值时刻(秒)
}

type CostRef struct {
    SkuKey       string  `json:"sku_key"`         // "seedance:1080p:per_sec"
    CatalogID    int64   `json:"catalog_id"`      // provider_cost_catalog.id
    UnitPriceUsd float64 `json:"unit_price_usd"`  // 快照时刻的单价(USD)
    Currency     string  `json:"currency"`        // 原始币种
}

type StorageRef struct {
    FileID       string  `json:"file_id"`
    SizeGb       float64 `json:"size_gb"`
    UnitPriceUsd float64 `json:"unit_price_usd"`  // gb_month
    RetentionMonths float64 `json:"retention_months"`
}
```

**5.4.4 实现要点**

1. **`pkg/billingexpr/version.go`（新增）**：版本前缀解析 + Runner 注册表
2. **`pkg/billingexpr/runner_v1.go` / `runner_v2.go` / `runner_vp.go`（新增）**：每个版本独立 runner
3. **`cost()` / `storage_cost()` 内置函数**：
   - 预扣阶段（`pre_consume`）：查 catalog → 写入 `BillingSnapshot.CostRefs` → 表达式求值用 snapshot 中的价格
   - 结算阶段（`tiered_settle`）：完全用 snapshot 中的 CostRefs 重放，**不再查 catalog**
4. **AST 自检扩展**：编译时遍历 AST 收集所有 `cost()` / `storage_cost()` / `usage()` 调用，把"该表达式将引用哪些外部值"写入元数据，预扣阶段据此预先 fetch 全部依赖一次性写入 snapshot
5. **回归测试**：v1 现有所有表达式必须 byte-for-byte 兼容；v2/vp 各有独立单元测试矩阵

**5.4.5 错误处理**

| 场景 | 行为 |
|------|------|
| 表达式编译失败（保存时） | 拒绝保存，前端报错 |
| 表达式输出非数字 | 编译期类型校验拒绝 |
| `vp` 输出负数 | 运行时拒绝，扣 0 quota + 写 error 日志 + 告警 |
| `cost("xxx")` 查不到 SKU | 运行时报错，预扣失败 → 503 + 告警（不允许默认价兜底，避免静默错算） |
| snapshot 中的 `CompiledExprHash` 与结算时不一致 | 配置漂移，拒绝结算 + 告警 |

### 5.5 前端编辑器扩展

`web/default/src/features/system-settings/billing/`（新增）：

- **可视化模式**：根据模型 `sku_type` 切换 UI
  - `image` 类型 → 显示「分辨率档位表」，列：分辨率 / 单价（USD 或积分）
  - `video` 类型 → 显示「分辨率 × 时长矩阵」
  - `llm_token` 类型 → 沿用现有 token 编辑器
- **原始模式**：直接编辑表达式字符串，带 lint 提示与模板下拉（`v1` / `v2` / `vp` 预设）。
- **预览与试算**：填写示例参数（分辨率 = 1080p, 时长 = 10s）即时算出 quota 和「等同积分」。

---

## 六、模块 3：OSS / TOS 月度对账

### 6.1 文件元数据登记（v1.1 修订：归属 business_account + 三重归属来源）

> **v1.1 修订动因：** Codex Important 4 指出 v1 把归属定为 `tenant_id (= user_id)`，但本设计核心实体是 `business_account_id`；仅靠 `object_key` 反查归属在命名不规范或对象迁移时会丢失。本节修订为「归属字段改 business_account_id + 三重归属来源（路径 / metadata / 本地映射）+ 缺失走人工待处理队列」。

任何由本网关产生 / 接收的多媒体文件，落 OSS / TOS 时**必须**写一条 `oss_object_meta`：

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | bigint PK | |
| `file_id` | varchar(64) UNIQUE | 业务侧文件 id（用作 `storage_cost(file_id)` 参数） |
| `provider` | varchar(32) | `aliyun_oss` / `volcengine_tos` |
| `bucket` | varchar(128) | |
| `object_key` | varchar(512) | OSS 对象 key |
| `size_bytes` | bigint | |
| `storage_class` | varchar(32) | `Standard` / `IA` / `Archive` |
| **`business_account_id`** | varchar(64) NOT NULL | **v1.1 改为业务账户归属**（不再是 user_id） |
| `task_id` | bigint NULL | 关联的中继任务（用于分摊） |
| `created_at` | datetime | |
| `deleted_at` | datetime NULL | 软删，便于月度对账还原历史占用 |

**三重归属来源（任一命中即可归属）：**

1. **路径前缀**：约定 `PathPrefix = "<provider>/biz_{business_account_id}/{yyyy}/{mm}/..."`，扫描 Inventory 的 `object_key` 用正则提取
2. **对象 metadata**：上传时同时写 `x-meta-business-account-id: biz_001`（阿里 OSS / 火山 TOS 都支持自定义 metadata），Inventory 报告也包含 metadata
3. **本地映射表 `oss_object_meta`**：网关侧主动登记的权威映射，优先级最高

**归属解析顺序：本地映射 → 对象 metadata → 路径前缀**，三个都失败的对象进入「待人工处理队列」（见 6.3.5）。

### 6.2 预扣阶段（轻量预估，仅用于预扣额度避免欠费）

任务产生文件时（任务回执解析后），算一次预扣存储费：

```
estimated_storage_usd = size_gb * cost("<provider>:<class>:gb_month") * EST_RETENTION_MONTHS
```

`EST_RETENTION_MONTHS` 全局配置，默认 1 个月。这笔费用走 `v2` 表达式的 `storage_cost()` 路径，写入 `BillingSnapshot`，并通过账本（见三ter）的 `reserve` 入账。

> 注意：预扣只是"先占住积分别欠费"，**真实结算以月结 Job 为准**，月底会冲销预扣 + 重算。

### 6.3 月度精确对账 Job（v1.1 修订：不可变 line item + job_run 分阶段 apply）

> **v1.1 修订动因：** Codex Important 4 指出 v1 用 `(tenant_id, month)` 做唯一约束的覆盖式幂等存在两个问题：(1) 同一企业多个 bucket / provider / class 会互相覆盖；(2) 月结跑挂后中途失败可能写一半难以无副作用重跑。修订为「不可变 line item + 显式 job_run + 分阶段 apply + 失败可重跑」。

**6.3.1 三张表**

**(a) `storage_billing_job_run`（每次月结的元数据，一个月一行）：**

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | bigint PK | |
| `month` | char(7) UNIQUE | `2026-04` |
| `inventory_snapshot_ref` | varchar(512) | Inventory 报告的 S3 / OSS 路径 |
| `inventory_snapshot_md5` | char(32) | 报告 hash（防重复拉取） |
| `status` | varchar(32) | `pending` / `extracting` / `extracted` / `applying` / `applied` / `failed` |
| `extract_completed_at` / `apply_completed_at` | datetime | |
| `total_line_items` / `total_applied_items` / `total_failed_items` | bigint | 进度统计 |
| `error_summary` | text | |

**(b) `storage_billing_line_item`（不可变明细，每个 `(job_run, business_account, provider, bucket, storage_class)` 一行）：**

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | bigint PK | |
| `job_run_id` | bigint FK | |
| `business_account_id` | varchar(64) | |
| `provider` | varchar(32) | |
| `bucket` | varchar(128) | |
| `storage_class` | varchar(32) | |
| `gb_months` | decimal(18,6) | 当月 GB·月 |
| `unit_price_usd` | decimal(18,8) | 该月该 SKU 单价 |
| `catalog_id` | bigint | 引用的 `provider_cost_catalog.id` |
| `total_usd` | decimal(18,8) | 子合计 USD |
| `quota_amount` | bigint | 折算 quota |
| `apply_status` | varchar(32) | `pending` / `applied` / `skipped` / `failed` |
| `applied_ledger_id` | bigint NULL | 写入 ledger 后的 entry id（apply 成功才有） |
| `apply_error` | text NULL | |
| `created_at` / `applied_at` | datetime | |

**唯一约束：** `(job_run_id, business_account_id, provider, bucket, storage_class)` 不重复 → 同一月结 run 内绝不重复扣同一个组合。

**(c) `storage_unassigned_object`（归属缺失，待人工）：**

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | bigint PK | |
| `job_run_id` | bigint FK | |
| `provider` / `bucket` / `object_key` / `size_bytes` / `storage_class` | 同 Inventory 字段 | |
| `status` | enum | `pending` / `assigned` / `ignored` |
| `assigned_business_account_id` | varchar(64) NULL | 运营手工赋值后填 |
| `assigned_at` / `assigned_by` | | |

**6.3.2 月结执行流（分阶段、可重跑）**

```
每月 1 日 04:00 (Asia/Shanghai) 触发 (Asynq cron):

  Phase 1: ensure_job_run
    upsert storage_billing_job_run(month=2026-04) status=pending
    幂等: 已 applied 则直接返回
       ↓
  Phase 2: extract (status: extracting → extracted)
    for each (provider, bucket):
      拉取 inventory.csv (or .parquet)
      按 6.1 三重归属解析每个对象 → business_account_id
      归属命中 → 写 storage_billing_line_item (按组合聚合 GB·月)
      归属缺失 → 写 storage_unassigned_object
    Phase 2 完成: status = extracted
    幂等保障: line_item 唯一约束防止重跑重复
       ↓
  Phase 3: apply (status: applying → applied)
    for each line_item where apply_status = pending:
      BEGIN TX
        根据账本接口 (见三ter) 写一条 ledger entry:
          entry_type = 'commit'
          amount     = -quota_amount
          reference  = ('monthly_settle', job_run_id)
          idempotency_key = sha256(job_run_id|line_item_id)
        update line_item set apply_status='applied', applied_ledger_id=…
      COMMIT
    全部完成: status = applied
    幂等保障: ledger entry 的 idempotency_key 防止重复扣费
       ↓
  Phase 4: 推送 webhook billing.monthly_settle_provisional 给业务系统（v1.2.1: 统一命名）
```

**6.3.3 失败重跑**

| 失败发生在 | 行为 |
|----------|------|
| Phase 2 中途 | 重跑：line_item 唯一约束确保已写的不重写；未写的继续；status 维持 `extracting` 直到全部完成 |
| Phase 3 中途 | 重跑：只处理 `apply_status = pending` 的 line_item；已 `applied` 的跳过；ledger idempotency_key 双重保险 |
| Phase 4 失败 | webhook 重试（见 9bis.4 outbox）；不影响账本已 applied 状态 |

**6.3.4 与预扣的冲销**

预扣阶段（6.2）已通过账本 `reserve` 占用了 quota。月结 apply 时：

```
对每个 business_account:
  1. 查该账户上月所有 reserved 项（reference_type='task' / 'storage_estimate'，时间窗 = 上月）
  2. 一次性 release 这些 reserved（写一条 ledger entry, entry_type='release'）
  3. apply 月结 line_item（写 entry_type='commit'）
```

这样实际净扣 = `sum(line_item.quota_amount) - sum(上月预扣)`，差额自然由账本三态平衡反映。

**6.3.5 归属缺失的人工处理**

`storage_unassigned_object` 表通过管理后台 UI 暴露给运营：
- 按 bucket / path 模式批量标记归属
- 标记完成后触发**该 line_item 的二次 apply**（不影响其他已 applied 项）
- 长期不处理告警（≥ 7 天未处理触发邮件 + Webhook 通知业务系统）

**6.3.6 监控**

- `storage_settle_phase_duration_seconds{phase=...}`
- `storage_unassigned_objects_pending`（gauge）
- `storage_settle_failed_total`

**6.3.7 Object-level 不可变明细（v1.2 新增，解 v1.1-I2）**

> **缘起：** Codex v1.1 评审 Important 2 指出 `storage_billing_line_item` 按 5 维聚合（不含 `object_key`），单对象归属修正、迁移争议无法精确重放。v1.2 引入对象级不可变表，line_item 降级为派生汇总。

**新增表 `storage_billing_object_item`（v1.2 核心）：**

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | bigint PK | |
| `job_run_id` | bigint FK | 哪次月结 run 记录的 |
| `provider` | varchar(32) | |
| `bucket` | varchar(128) | |
| `object_key` | varchar(512) | 对象级 |
| `size_bytes` | bigint | |
| `storage_class` | varchar(32) | |
| `gb_months` | decimal(18,8) | 该对象在该账期占用的 GB·月（精确到秒级生命周期） |
| `unit_price_usd` | decimal(18,8) | 该账期 SKU 单价 |
| `total_usd` | decimal(18,8) | 子合计 |
| `business_account_id` | varchar(64) NULL | 归属（NULL = 仍在 unassigned，未被纳入扣费） |
| `attribution_source` | enum | `local_map` / `object_metadata` / `path_prefix` / `manual` |
| `attribution_conflict_resolved_to` | varchar(64) NULL | 若三重归属冲突，最终采纳的 source |
| `created_at` | datetime | |

**唯一约束：** `(job_run_id, provider, bucket, object_key)` 不重复 → 同一月结绝不重复扣同一对象。

**与原 `storage_billing_line_item` 的关系（v1.2 修订，v1.2.1 强化）：**

`storage_billing_line_item` 现在是**从 object_item 派生的聚合汇总**（而非独立账实），仅用于：
- 管理后台快速查看「企业 X 在 bucket Y 的存储成本」
- **写入账本时按 (business_account_id, provider, bucket, storage_class) 聚合 commit 一笔 ledger entry**（避免每个 object 都写一条 ledger 太碎）

> **v1.2.1 强约束：账本入账层级**
>
> **只有 `storage_billing_line_item` 与后续的 `adjustment` 流水写 ledger entry，`storage_billing_object_item` 不直接扣费、不写 ledger。**
>
> 这避免「双层模型双重扣费」的实现风险：若 object_item 和 line_item 都写 ledger，账户会被扣两次。明确职责：
> - `storage_billing_object_item`：**事实层**，记录 Inventory 每个对象的占用与归属，仅供审计、查询、归属修正
> - `storage_billing_line_item`：**记账层**，聚合后入 ledger，是账本的来源
> - `adjustment`：归属修正后的差额调整，单独写 ledger，与 line_item 通过 `correlation_id` 关联

聚合规则：

```sql
INSERT INTO storage_billing_line_item (...)
SELECT
  job_run_id,
  business_account_id,
  provider, bucket, storage_class,
  SUM(gb_months) AS gb_months,
  AVG(unit_price_usd) AS unit_price_usd,  -- 注意:若价格变动需用加权平均
  SUM(total_usd) AS total_usd
FROM storage_billing_object_item
WHERE job_run_id = ? AND business_account_id IS NOT NULL
GROUP BY job_run_id, business_account_id, provider, bucket, storage_class;
```

**单对象修正流程（v1.2 新增）：**

```
场景: 运营发现 object_X 错归属到企业 A,应该是企业 B

1. 在管理后台 storage_billing_object_item 列表找到该对象
2. 修改 business_account_id 为 B,attribution_source = 'manual'
3. 系统自动:
   a. 重新生成对应 (job_run, A) 与 (job_run, B) 的 line_item 派生数据
   b. 写两条 adjustment ledger entry:
      - A 账户 refund(object_X_amount)
      - B 账户 commit(object_X_amount)
      - correlation_id 关联,便于审计
   c. 推送 webhook billing.storage_attribution_corrected
4. 全程留审计:who/when/from/to/reason
```

**归属三源冲突处理（v1.2 明确）：**

| 冲突场景 | 处理 |
|---------|------|
| 三源指向同一 business_account_id | 直接归属 |
| 本地映射 vs 其他源不一致 | 取本地映射，记 `attribution_conflict_resolved_to='local_map'`，对其他源发告警 |
| metadata vs 路径前缀不一致（无本地映射） | 取 metadata，记 conflict |
| 三源都缺失 | business_account_id = NULL，进 unassigned 等人工 |

**十四章「未决项」清理（v1.2）：** 删除原十四章中残留的 `(tenant_id, month)` 幂等表述，统一指向本节的 object-level 不可变表。

### 6.4 配置开关

- `STORAGE_BILLING_ENABLED` = `false`（默认关闭）：未上 OSS 对接时可关闭整条链路。
- `STORAGE_ESTIMATED_RETENTION_MONTHS` = `1`：预扣预估月数。
- `STORAGE_INVENTORY_S3_PATH` = `s3://...`：Inventory 投递路径。
- `STORAGE_MONTHLY_RECONCILE_HOUR` = `4`：月结 Job 触发小时。

---

## 七、模块 4：网关层规则叠加

### 7.1 规则叠加链（按表达式求值顺序）

```
基础价 (cost() or 固定 quota)
  × 渠道倍率 (channel_ratio，已有，沿用)
  × 模型倍率 (model_ratio，已有，沿用)
  × 租户分组倍率 (group_ratio，已有，沿用)
  × 时段折扣 (||| 后置规则，已有，沿用)
  + 固定加价 (表达式内 +N)
  + OSS 存储预估 (storage_cost)
  + 平台服务费 (表达式内 +N)
  = 最终扣费 (quota)
```

**所有这些层级都已经被 `billingexpr` 表达式语法天然支持**，唯一新增的是 `cost()` 与 `storage_cost()`，以及 `v2` / `vp` 两个版本。

### 7.2 表达式存储与生效

- 沿用 `setting/billing_setting/tiered_billing.go` 现有机制：
  - `ModelBillingMode[model_name] = "tiered_expr"`
  - `ModelBillingExpr[model_name] = "<expression>"`
- **保存时验证**：编译 + 用预置样例参数（多分辨率 / 多时长组合）跑一遍，确保非负。
- **生效**：`SyncOptions` 周期性热更新（已有），新表达式无需重启。

### 7.3 优先级与降级

| 配置 | 行为 |
|------|------|
| 该模型已配置 `tiered_expr` | 走表达式 |
| 未配置但 `model_ratio` 有值 | 走旧倍率（LLM 兼容路径） |
| 都没有 | 拒绝调用并返回 `model_not_priced` 错误 |

---

## 八、模块 5：账号映射与多上游路由

### 8.1 业务场景

网关作为多媒体创作平台的**基础设施层**，自身**不持有账号体系**。所有 organization / 子账号 / 计费主体 / 充值发票都在上游业务系统（多媒体创作平台）实现。

业务系统与网关的接口非常简洁：

```
业务系统 (多媒体创作平台)         网关层 (本设计)
─────────────────────           ─────────────────
企业账户 X                       Token (Key K_X)
  ├─ 充值 / 子用户 / 发票           ↓
  └─ 调网关 → 发 Key K_X          按 model + body 参数路由
                                  内部多套上游凭据：
                                    ├─ Channel #101: 火山 seedance:真人:proj_X
                                    ├─ Channel #102: 火山 seedance:仿真:proj_X
                                    └─ Channel #103: 火山 seedance:default:proj_X
```

**典型场景：seedance 2.0**

火山引擎要求为每个企业用户创建独立的项目 / 组合用户进行计量与隔离；同时该模型本身在「仿真人 / 真人」两个生成路径上需要不同的上游配置。因此一个企业 X 的 seedance 调用，在上游表现为 **3 套独立的凭据**，**每套凭据包含 5 类配置项**：

| 配置项 | 用途 |
|---|---|
| **API_KEY** | seedance API 主调用凭据 |
| **ARK AK/SK** | 火山方舟服务凭据（访问火山其他能力时需要） |
| **TOS AK/SK** | 火山对象存储凭据（企业资产上传 / 下载） |
| **桶名（Bucket）** | TOS 存储桶，企业资产隔离的物理位置 |
| **项目名（Project ID）** | 火山的项目隔离 ID，用于计量上游成本归属 |

但对业务系统而言，他们只调用 `model=seedance-2.0`，参数里说明 `is_real_person=true/false` 或 `style=synthesized` 即可。

**网关的核心职责：** 把业务系统送来的「一个 Key + 一个 model + 参数」翻译成正确的「上游凭据 + project_id + 真实参数 + 存储路径」。

### 8.2 数据库变更

**`token` 表新增字段（小幅扩展，向后兼容）：**

| 字段 | 类型 | 说明 |
|------|------|------|
| `business_account_id` | varchar(64) NULL | 业务系统的企业账户 ID；NULL 表示个人账户或测试 Key |
| `business_account_type` | varchar(16) NULL | `enterprise` / `individual` / `internal`，方便日志与对账聚合 |

> 业务侧通过现有 `POST /api/token` API 创建 Key 时一并写入这两个字段；调用时 TokenAuth 中间件读出后塞进 context，路由、日志、计费均可引用。

**`channel` 表新增字段（可选白名单）：**

| 字段 | 类型 | 说明 |
|------|------|------|
| `restricted_business_accounts` | text JSON NULL | 形如 `["biz_001","biz_002"]`；非空时表示该 channel 只允许列表里的业务账户使用，防止 X 的专属 channel 被误路由到 Y |
| `channel_purpose` | varchar(64) NULL | 标签，如 `seedance:realperson:biz_001`，纯人类可读，便于运营管理 |

**结构化渠道凭据（v1.1 修订：正式入 DTO + envelope encryption）：**

> **v1.1 修订动因：** Codex Important 2 指出 new-api 的 `dto.ChannelOtherSettings` 是固定 DTO，`SetOtherSettings` 会 marshal 该 DTO 把未识别字段静默丢弃；同时单一密钥 + AES-GCM 没有 key version 和轮换机制，主密钥泄露所有凭据全裸。本节修订为「正式入 DTO + envelope encryption + key_version」。

**Step 1：把 `ChannelCredentials` 正式加进 `dto.ChannelOtherSettings`**

不要让 `ChannelCredentials` 游离在 `Setting` / `OtherSettings` 的裸 JSON 里——直接扩展 `dto.ChannelOtherSettings` 结构：

```go
// dto/channel_settings.go (扩展现有文件)
type ChannelOtherSettings struct {
    // ... 现有字段（不动） ...

    // v1.1 新增
    Credentials *ChannelCredentials `json:"credentials,omitempty"`
}

// dto/channel_credentials.go (新增)
type ChannelCredentials struct {
    APIKey       string             `json:"api_key,omitempty"`         // 主调用凭据,等价于 Channel.Key,保留在主字段以复用 MultiKey 轮询
    UpstreamAKSK map[string]AKSK    `json:"upstream_ak_sk,omitempty"`  // 例: {"ark": {...}, "tos": {...}}
    Storage      *StorageBinding    `json:"storage,omitempty"`         // 关联 TOS / OSS 配置
    ProjectID    string             `json:"project_id,omitempty"`      // 上游项目隔离 ID
    ExtraKV      map[string]string  `json:"extra_kv,omitempty"`        // 其他键值,便于未来扩展
}

type AKSK struct {
    AccessKey     string `json:"access_key"`
    SecretCipher  string `json:"secret_cipher"`     // base64(envelope encryption: KEK_version + IV + ciphertext + tag)
    KeyVersion    int    `json:"key_version"`       // 该密文用哪个 KEK 版本加密,支持轮换
    Endpoint      string `json:"endpoint,omitempty"`
    Region        string `json:"region,omitempty"`
}

type StorageBinding struct {
    Provider   string `json:"provider"`              // "tos" / "oss"
    Bucket     string `json:"bucket"`
    PathPrefix string `json:"path_prefix,omitempty"` // "video/biz_X/{yyyy}/{mm}/{task_id}",支持模板变量
    AKSKRef    string `json:"ak_sk_ref"`             // 引用 UpstreamAKSK 里的 key 名,如 "tos"
}
```

**Step 2：Merge-preserving 更新**

`Channel.SetOtherSettings()` 改造为 read-modify-write 模式，避免静默丢字段：

```go
// 错误写法(现状,会丢字段):
//   channel.OtherSettings = mustMarshal(newSettings)

// 正确写法(v1.1):
func (c *Channel) MergeOtherSettings(patch ChannelOtherSettingsPatch) error {
    existing := c.GetOtherSettings()  // 解析现有 JSON 到 DTO
    patch.ApplyTo(&existing)          // 仅覆盖 patch 中非 nil 的字段
    raw, err := common.Marshal(existing)
    if err != nil { return err }
    c.OtherSettings = string(raw)
    return nil
}
```

所有 controller 层修改 channel settings 必须走 `MergeOtherSettings`，不允许直接 marshal 覆盖。

**seedance 2.0 的 5 项配置落点：**

| 你的配置 | 落点 |
|---|---|
| API_KEY | `Channel.Key`（主字段，复用 MultiKey 轮询） |
| ARK AK/SK | `Channel.OtherSettings.Credentials.UpstreamAKSK["ark"]` |
| TOS AK/SK | `Channel.OtherSettings.Credentials.UpstreamAKSK["tos"]` |
| 桶名 | `Channel.OtherSettings.Credentials.Storage.Bucket` |
| 项目名 | `Channel.OtherSettings.Credentials.ProjectID`（同时也注入 `ParamOverride` 透传给上游） |

**Step 3：Envelope Encryption + Key Version**

不要用单一长期密钥直接加密所有凭据。改用**两层密钥**：

```text
KEK (Key Encryption Key) - 主密钥,从环境变量/KMS 读取,极少访问
  ├─ KEK v1 (创建于 2026-05)
  ├─ KEK v2 (创建于 2027-01,轮换时启用)
  └─ ...
     ↓ 加密
DEK (Data Encryption Key) - 数据密钥,每条凭据一个,随机生成
     ↓ 用于加密
SecretKey (明文凭据)
```

**加密流程（写）：**
1. 随机生成 32 字节 DEK
2. 用当前活跃 KEK 加密 DEK → `enc_dek`
3. 用 DEK 加密 SecretKey → `ciphertext`
4. 拼接 `key_version + enc_dek + IV + ciphertext + tag`，base64 存入 `secret_cipher`

**解密流程（读）：**
1. 解析 `secret_cipher` 取出 `key_version`
2. 用对应版本的 KEK 解出 DEK
3. 用 DEK 解出 SecretKey
4. **解密结果只在最小作用域内使用**（构造上游请求的瞬间），用完立刻 `runtime.KeepAlive` 后置 nil

**密钥治理：**

| 项 | 配置 |
|---|---|
| KEK 存储 | 优先 KMS（阿里 KMS / 火山 KMS / Hashicorp Vault）；过渡期允许环境变量 `GATEWAY_KEK_V1` / `GATEWAY_KEK_V2` 等 |
| KEK 轮换 | 新增 KEK v(N+1) → 启动后台 job 用 v(N+1) 重加密 DEK（注意：是重加密 DEK，不是 SecretKey，DEK 不变）→ 标记 v(N+1) 为 active → vN 按下述「保留期对齐」规则保留 |
| **KEK 保留期（v1.2 修订，对齐任务生命周期）** | 旧 KEK 必须保留 `max(任务最长执行期, Asynq DLQ 保留期, 财务审计补偿窗口)`；默认值组合是 `max(30天, 7天, 365天) = 365天 = 1 年`；**禁止**在该窗口内回收旧 KEK，否则可能导致 inflight 任务、DLQ 重放、月底对账时解密失败 |
| **任务最长执行期硬上限（v1.2 新增）** | 30 天。任务从 `SUBMITTED` 起 30 天未达 `SETTLED` 则强制 `EXPIRED`（账本 release 预扣），与 KEK 保留期下限对齐 |
| DEK 数量 | 每条 AKSK 一个独立 DEK，互不影响 |
| 加密库 | Go 标准库 `crypto/aes` + GCM 模式；KEK 加密 DEK 也用 AES-GCM |
| 实现位置 | `pkg/envelope_crypto/` 新增包；`common/crypto.go` 不动 |

**关键合规底线：**

- ✅ `SecretCipher` 字段在 DB 中永远是密文，运维 / DBA 直接读库看到的也是密文
- ✅ 解密只在调上游请求构造的瞬间发生
- ✅ **明文 SecretKey 不出现在任何日志、panic stack、JSON 序列化、metrics、错误信息**
- ✅ 前端编辑：仅 Root + 二次验证后返回明文；列表页全程脱敏 `****1234`
- ✅ 审计日志：记录 `channel_id × 解密时间 × caller`，不记录密钥本身
- ✅ 进程崩溃 core dump：默认禁用 core dump（`ulimit -c 0`），或开启时配置 sanitization
- ⚠️ Go 内存暂时无法完全防 GC 扫描泄露明文（这是语言层限制），上线后接 KMS 短时 token（每次申请 5 分钟有效的临时凭据）可彻底规避

**未来演进（P3+）**：若火山 / 阿里 / AWS 支持「角色扮演 + STS 短时令牌」，把长期 AK/SK 替换为每次请求向 STS 申请短时 token，根本上避免长期凭据存储。

**新增 `channel_routing_rule` 表：**

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | bigint PK | |
| `business_account_id` | varchar(64) NULL | **NULL = 全局默认规则**；非 NULL = 企业覆盖规则 |
| `model` | varchar(128) | 目标模型，如 `seedance-2.0` |
| `condition_expr` | text | 用 `expr-lang/expr`（复用 billingexpr 编译环境）写的条件表达式，例：`param("is_real_person") == true` |
| `target_channel_ids` | text JSON | 形如 `[101]` 或 `[101,102]`；命中后从这些 channel 中按 priority + weight 选 |
| `priority` | int | 同一 `(business_account_id, model)` 下规则的求值优先级，数大优先 |
| `enabled` | bool | |
| `notes` | text | 运营备注 |
| `created_by` / `updated_by` / `created_at` / `updated_at` | 审计字段 | |

**索引：** `(business_account_id, model, priority DESC, enabled)`，路由查询的热路径。

**跨库兼容**（按 `CLAUDE.md` Rule 2）：
- `text JSON` 在 PG/MySQL 走 `TEXT`，应用层用 `common.Marshal/Unmarshal`；
- 不用 PG 专属的 JSONB 操作符。

### 8.3 Distributor 改造（v1.1 修订：fail-closed + allowed_channel_ids 强制约束）

> **v1.1 修订动因：** Codex Critical 2 指出 v1 设计"未命中则回退原逻辑"在企业专属凭据场景下会变成 fail-open，把本应隔离的企业请求落到共享池，造成数据隔离 / 合规 / 成本归属事故。本节修订为默认 fail-closed，并把路由结果以 `allowed_channel_ids` 形式强制约束 distributor 的全部下游选择（含 affinity 和 random selection）。

**新增字段 `channel_routing_rule.fallback_policy`：**

| 取值 | 语义 |
|------|------|
| `strict`（**默认**） | 候选 channel 全不可用 → 直接报错 `503 routing_no_available_channel`，**不降级** |
| `next_rule` | 候选不可用 → 求值下一条规则（仍限定在当前 `business_account_id` 的规则集内） |
| `global_pool` | 候选不可用 → 降级到全局默认规则池（`business_account_id IS NULL` 的规则） |
| `legacy_distributor` | 候选不可用 → 降级到 new-api 原逻辑（按 model + group 选） |

**企业覆盖规则默认 `strict`**；全局默认规则可配 `legacy_distributor` 作为最后兜底。

**改造后的执行流：**

```text
TokenAuth 把 business_account_id 注入 context
       ↓
Distribute()
       ↓
modelRequest := getModelRequest(c)
normalized_ctx := ExtractNormalizedContext(c, request)  // 见 8.7
       ↓
══════════ 新增逻辑：路由规则求值 ══════════
1. 取 (business_account_id, model) 查 channel_routing_rule（带缓存）
   - 优先匹配 business_account_id != NULL 的(企业覆盖)
   - 未命中则匹配 business_account_id IS NULL 的(全局默认)
2. 按 priority DESC 依次求值 condition_expr (用 normalized_ctx,有超时)
   - 求值异常 → 跳过此规则,记 warning 日志,继续下一条
   - 首条命中 → allowed_channel_ids = target_channel_ids
3. 应用 restricted_business_accounts 过滤:
   - 从 allowed_channel_ids 中剔除 channel.restricted_business_accounts
     非空且不包含当前 business_account_id 的项
4. 应用渠道健康过滤:
   - 剔除 status != enabled / 已熔断 的 channel
5. allowed_channel_ids 是否非空?
   - 是 → 进入下游选择,但所有选择必须从 allowed_channel_ids 内挑
   - 否 → 按 fallback_policy 执行
═══════════════════════════════════════════
       ↓
══════════ 下游选择(强制约束) ══════════
service.GetPreferredChannelByAffinity(c, model, group, allowed_channel_ids)
  - 现有亲和性逻辑 + 必须命中 allowed_channel_ids
service.CacheGetRandomSatisfiedChannel(c, model, group, allowed_channel_ids)
  - 现有随机选择 + 必须命中 allowed_channel_ids
═══════════════════════════════════════════
       ↓
SetupContextForSelectedChannel(c, channel)
context 写入:
  - matched_routing_rule_id (审计用)
  - allowed_channel_ids (整个候选集,日志用)
  - fallback_invoked (是否走了降级,用于告警)
```

**改造涉及的代码点：**

| 文件 | 改动 |
|------|------|
| `model/channel_routing_rule.go`（新增） | 表 + GORM 模型 + 缓存 |
| `service/channel_routing.go`（新增） | 规则求值 + fail-closed 处理 + 监控埋点 |
| `middleware/distributor.go` | `Distribute()` 在原逻辑前插入路由判断；调用现有选择函数时传入 `allowed_channel_ids` |
| `service/channel_affinity.go` | `GetPreferredChannelByAffinity` 增加 `allowed []int` 参数；亲和缓存命中后必须在 allowed 内才返回 |
| `model/channel_satisfy.go` / `service/channel_select.go` | `CacheGetRandomSatisfiedChannel` 增加 `allowed []int` 参数；SQL / 内存过滤都增加约束 |

**关键不变量：**

> **如果 routing rule 命中且 `allowed_channel_ids` 非空，那么本次请求最终被选中的 channel 必须是 `allowed_channel_ids` 的子集。** 违反此不变量被认为是高危 bug，单元测试 + 集成测试必须覆盖各种降级路径。

**监控指标：**

- `routing_rule_evaluation_duration_p99` —— 路由求值耗时（应 < 1ms）
- `routing_rule_evaluation_errors_total` —— 表达式求值失败计数（按 rule_id 分桶）
- `routing_strict_failures_total` —— `strict` 策略下导致 503 的次数（按 business_account_id × model 分桶，告警阈值 5/分钟）
- `routing_fallback_invoked_total` —— 任何形式 fallback 触发的次数

**关键点：**
- 求值表达式时**复用 `pkg/billingexpr` 的编译环境**（已有 `expr-lang/expr`），不引入第二套 DSL。
- 路由命中的规则 ID + 候选集 + 是否走降级 全部写 context 与日志，便于审计「这次调用为什么路由到 channel #101」「为什么 503」。
- 全局默认规则承担 95% 的常规请求；只有需要专属上游隔离的企业才配企业覆盖。
- **企业账户专属 channel 设置 `restricted_business_accounts` 是硬墙**——即使路由规则配错指向了不允许的 channel，过滤阶段也会拦下。

### 8.3.6 企业隔离硬模式（v1.2 新增，解 v1.1-C2）

> **缘起：** Codex v1.1 评审 Critical 2 指出 `fallback_policy` 仍保留 `global_pool` / `legacy_distributor`，对必须独立凭据的企业（如合规要求的 seedance 大客户）形成 fail-open 逃生门。本节定义 `isolation_required` 硬开关，从账户级别强制约束。

**硬规则：当 `business_account.isolation_required = true` 时，**

1. **`fallback_policy` 候选集合受限**：只允许 `strict` 与同企业 `next_rule`（即只能在 `business_account_id = 该企业` 的规则集内继续找下一条），**严禁** `global_pool` / `legacy_distributor` / 跨企业 `next_rule`
2. **保存路由规则时强校验**：管理后台保存 routing_rule 时，若 `business_account_id` 对应企业 `isolation_required=true`，则 `fallback_policy` 选项 UI 上只显示 `strict` / 同企业 `next_rule`，后端 API 也强校验
3. **distributor 运行时强校验**：即使配置层绕过保存校验（如直接 SQL 写入），运行时 distributor 也必须二次校验，命中违规配置则该路由失败（503）+ critical 告警
4. **`restricted_business_accounts` 协同约束**：`isolation_required=true` 的企业必须有至少 1 个 `restricted_business_accounts` 包含自己的 channel；保存 / 启动时校验

**Break-glass 流程（紧急逃生门）：**

业务方临时需要绕过隔离（如专属 channel 全部宕机、紧急保住业务）时：

```
1. 网关运营 Root 登录 → 进入「业务账户管理 > 紧急逃生门」
2. 选择目标 business_account_id + 填理由 + 选择时长(默认 1 小时,最长 24 小时)
3. **必须**第二个 Root 在线审批通过
4. 系统 UPDATE business_account.break_glass_until = NOW() + duration
5. 期间 distributor 允许该账户走 global_pool / legacy_distributor 降级
6. 每次降级触发:
   - critical 审计日志(who/when/which rule/which channel)
   - Webhook account.break_glass_used 通知业务系统
   - Prometheus 指标 break_glass_invocations_total++
7. 超时自动失效,break_glass_until 清空
```

**审计与监控：**

| 指标 | 告警阈值 |
|------|---------|
| `isolation_violation_blocked_total` | > 0（任何违规配置被运行时拦截 → 紧急告警） |
| `isolation_strict_failure_total` | > 5/分钟（隔离账户因 strict 导致 503，可能上游故障） |
| `break_glass_active_accounts` | > 0（gauge，有 active break-glass 即告警，确认运营有人值守） |
| `break_glass_invocations_total` | 每次都打日志 + Webhook，不告警但全程审计 |

**典型配置示例：**

```
seedance 真人合规客户 biz_001:
  isolation_required = true
  break_glass_until = NULL (常态)

  routing rules (biz_001):
    priority=100 condition="param('is_real_person')==true"
                  target_channels=[101]
                  fallback_policy=strict           # 真人专属,绝不降级
    priority=90  condition="param('style')=='synthesized'"
                  target_channels=[102]
                  fallback_policy=next_rule        # 仿真可降级到下一条同企业规则
    priority=0   condition="true"
                  target_channels=[103]
                  fallback_policy=strict           # 默认渠道也是 strict,坏了直接 503

  保存时 UI 不显示 global_pool/legacy_distributor 选项
```

### 8.4 配置示例：seedance 2.0 三套上游 + 多企业

**Channel 配置（运营在管理后台逐条新建）：**

| Channel ID | Name | Type | Key (上游) | ParamOverride | restricted_business_accounts |
|---|---|---|---|---|---|
| 201 | seedance-shared-pool | volcengine | `sk_shared_xxx` | `{"project_id":"proj_shared"}` | NULL（共享池，全局默认） |
| 101 | seedance-realperson-bizX | volcengine | `sk_real_X` | `{"project_id":"proj_realX","extra_flag":"realperson"}` | `["biz_X"]` |
| 102 | seedance-synth-bizX | volcengine | `sk_synth_X` | `{"project_id":"proj_synthX"}` | `["biz_X"]` |
| 103 | seedance-default-bizX | volcengine | `sk_default_X` | `{"project_id":"proj_defaultX"}` | `["biz_X"]` |

**Routing Rules 配置：**

*全局默认（biz_id = NULL）：*

| Priority | Condition | Target Channels | 含义 |
|---|---|---|---|
| 0 | `true` | `[201]` | 兜底，所有未覆盖企业走共享池 |

*企业 X 覆盖（biz_id = "biz_X"）：*

| Priority | Condition | Target Channels | 含义 |
|---|---|---|---|
| 100 | `param("is_real_person") == true` | `[101]` | 真人请求走 X 的专属真人渠道 |
| 90 | `param("style") == "synthesized"` | `[102]` | 仿真人请求走 X 的专属仿真渠道 |
| 0 | `true` | `[103]` | X 的兜底渠道 |

**业务系统调用时：**

```http
POST /v1/video/generations
Authorization: Bearer K_X
{
  "model": "seedance-2.0",
  "prompt": "...",
  "is_real_person": true,
  "duration": 10,
  "resolution": "1080p"
}
```

网关内部依次：
1. TokenAuth 解出 `business_account_id = "biz_X"`
2. Distributor 查 biz_X + seedance-2.0 的 routing rules，priority=100 的条件 `param("is_real_person") == true` 命中
3. 候选集 = `[101]`，单 channel 直接选定
4. 加载 channel 101 的 `ParamOverride` `{"project_id":"proj_realX","extra_flag":"realperson"}`，与请求体合并后转发给火山
5. 日志记录：`business_account_id=biz_X` / `channel_id=101` / `routing_rule_id=R_real_X`

**新企业 Y 上线流程：**
1. 业务侧给 Y 创建 Key K_Y（业务系统的事）
2. 运营在网关后台为 Y 新建 3 个 channel（复制 X 的，改 key 与 project_id）
3. 复制 X 的 3 条 routing rules，把 `business_account_id` 改为 `biz_Y`

完成。无须改业务侧调用代码。

### 8.5 安全与合规

- **business_account_id 唯一来源是 Token**：业务侧创建 Key 时写死，调用方**无法**通过 header 覆盖，防止越权调用别人企业的专属渠道。
- **restricted_business_accounts 是硬墙**：即使 routing rule 配错指向了不允许的 channel，也会被 distributor 兜底过滤。
- **管理后台的 channel + routing rule 编辑必须走 `middleware/secure_verification.go`**：防止配置被恶意篡改导致企业渠道串号。
- **审计日志**：channel 与 routing rule 的每次变更写入审计表（who / when / before / after），保留 ≥ 1 年。

### 8.6 前端 UI

落在 `web/default/src/features/system-settings/channel-routing/`：

- **Channel 列表页**：现有页面扩展两列 `restricted_business_accounts` / `channel_purpose`。
- **Routing Rules 页**（新增）：
  - 顶部 tab 切换「全局默认」/「企业覆盖（按企业筛选）」
  - 每条规则可视化编辑器：业务账户选择器 + 模型选择器 + 条件表达式编辑器（复用计费表达式同款 monaco editor）+ 候选 channel 多选 + 优先级 + **`fallback_policy` 单选（默认 strict）**
  - 试算面板：填一个示例请求体，按规则列表跑一遍展示命中规则、`allowed_channel_ids`、最终 channel、是否触发 fallback

### 8.7 路由表达式安全（v1.1 新增）

> **缘起：** Codex Important 7 指出复用 `billingexpr` 直接读 body 的方式在多媒体场景下有性能与可靠性风险——视频 / 图像请求常含 base64 大字段、multipart，body 可能几十 MB，每次请求遍历数十条规则求值会成为热路径瓶颈；表达式失败时无降级策略。

**8.7.1 Normalized Context 前置抽取**

不允许路由表达式直接读取原始 body。在 distributor 入口先抽取「**小型规范化上下文**」供表达式使用：

```go
// service/channel_routing.go
type NormalizedRoutingContext struct {
    Model         string                 // 模型名
    BusinessID    string                 // business_account_id
    TokenTags     map[string]string      // Token 上的标签
    ParamsAllowed map[string]any         // 白名单参数(仅小型标量)
    Headers       map[string]string      // 白名单 header
    EstimatedSize int                    // 请求体大小(字节,仅做条件判断,不传内容)
}
```

**白名单字段（仅这些可在路由表达式中访问）：**

| 来源 | 允许的字段 | 限制 |
|------|----------|------|
| body | `is_real_person` / `style` / `resolution` / `duration` / `size` / `aspect_ratio` / `watermark` / `quality` / `mode` 等约 30 个常用枚举/标量字段 | 全部字符串 / 数字 / 布尔；**禁止访问 `prompt` / `image` / `reference_images` / `messages` 等大字段** |
| header | `x-resolution` / `x-duration` / `x-priority` 等约 10 个自定义 header | 字符串，长度 ≤ 128 |
| Token 属性 | `business_account_id` / `business_account_type` / 自定义 tag | |

白名单在 `setting/routing_setting/whitelist.go` 配置，可热更新；新字段需要 Root 审批。

**8.7.2 表达式求值约束（v1.2 修订：统一性能预算）**

- **路由阶段总硬上限**：含 normalized context 抽取 + 全部规则求值 + channel 选择 ≤ **10ms 硬上限**（不是 p99，是单次硬上限，超出直接报错走 fallback_policy）
- **单条规则求值**：≤ 1ms 软上限（p99 监控）；连续超 3ms 触发该规则告警
- **规则总数上限**：单 `(business_account_id, model)` 下规则数 ≤ 10（v1.2 从 20 缩到 10，配合 10ms 总预算）
- **表达式黑名单**：禁止 `param("xxx")` 中 `xxx` 不在白名单；保存时静态校验
- **panic 兜底**：每条规则求值用 `defer recover()` 包裹；panic 记 error 日志 + 视为求值失败

**8.7.3 表达式失败降级**

| 失败类型 | 处理 |
|---------|------|
| 编译期错误（保存时） | 拒绝保存，前端报红 |
| 运行时表达式异常（如类型不匹配） | 视为该规则不匹配（return false），继续下一规则 |
| 运行时 panic | 同上 + ERROR 日志 + 告警 |
| 求值超时 | 同上 + 累计触发熔断（同一 rule 1 分钟内 10 次超时 → 自动禁用 + 邮件告警） |
| 所有规则求值后无命中 | 走 fallback_policy（见 8.3） |

**8.7.4 性能预算（v1.2 修订：统一为 10ms 硬上限）**

- 路由阶段总耗时（含 normalized context 抽取 + 全部规则求值 + channel 选择）≤ **10ms 硬上限**（与 8.7.2 一致）
- 触达硬上限：本次请求按 fallback_policy 处理（隔离账户即 503）+ 触发性能告警
- 单条 channel_routing_rule 求值 ≤ 1ms p99
- normalized context 抽取本身不解析 body 大字段（用 `gjson.GetBytes` 按白名单字段选择性读取，body 超过 1MB 时启用 streaming 解析）

**8.7.5 监控**

- `routing_normalized_ctx_build_duration_p99` —— 上下文抽取耗时
- `routing_expr_eval_duration_p99` —— 单条表达式求值耗时（按 rule_id 分桶）
- `routing_expr_timeout_total` / `routing_expr_panic_total` —— 异常计数

---

## 九、模块 6：Asynq 任务队列

### 9.1 为什么 P0 就上 Asynq

new-api 现状的视频 / Midjourney / Suno 任务靠 `UpdateMidjourneyTaskBulk` / `UpdateTaskBulk` 两个 goroutine **轮询数据库**拉取待更新的任务（`gopool` 调度 + 主节点单点执行）。该方案在场景上有三个隐患：

1. **轮询粒度粗**：默认 ~10 秒一轮，视频任务回执延迟体感差；
2. **并发控制薄弱**：只能在数据库层用 `WHERE inflight < N` 这种乐观锁约束，跨节点协调困难；
3. **优先级 / 重试退避 / 死信都要自己写**：长期维护成本高。

业务方决策**一步到位上 Asynq**：用 Redis 作为队列后端，Go 原生，与 new-api 现有的 Redis 栈天然集成，运维成本极低，且性能远好于 DB 轮询。

### 9.2 引入位置

- `service/task_queue.go`（新增）：封装 Asynq Client + Server 启停；
- `main.go`：用 `gopool.Go(func(){ asynqServer.Run(mux) })` 起任务消费者；
- 现有 `controller/midjourney.go` / `controller/task.go` / `controller/task_video.go` 的"立即处理 + 写库"逻辑改为「**写库 + Enqueue**」，由 Asynq worker 异步处理上游回执拉取、状态轮询、回调通知；
- `UpdateMidjourneyTaskBulk` / `UpdateTaskBulk` 两个 DB 轮询 goroutine **下线**，相关定时由 Asynq 的 scheduled task 替代。

### 9.3 任务类型设计

| Task Type | 触发时机 | 目的 |
|---|---|---|
| `task:video:fetch` | submit 成功后 enqueue，延迟 5s 首次执行；失败重试间隔 5/10/30/60s | 拉取上游视频任务状态 |
| `task:midjourney:fetch` | submit 成功后 | 同上，针对 MJ |
| `task:suno:fetch` | submit 成功后 | 同上，针对 Suno |
| `task:callback` | 任务完成后 | 向业务系统回调（HTTP webhook） |
| `task:oss:register` | 任务完成产生文件后 | 写入 `oss_object_meta` 表 |
| `cron:storage_monthly_settle` | 每月 1 日 04:00 | 触发 OSS 月度对账（模块 3） |
| `cron:cost_catalog_check` | 每天 03:00 | 触发变价检测（模块 1 / 模块 4） |
| `cron:channel_health_check` | 每 5 分钟 | 替代 `AutomaticallyTestChannels` |

### 9.4 并发池与租户配额

Asynq 的 `Queue Priority` 与 `Concurrency` 可天然表达：

```text
- 共享队列 "default"          concurrency=50
- 企业 X 专属队列 "biz_X"      concurrency=10  priority=2
- 企业 Y 专属队列 "biz_Y"      concurrency=5   priority=1
- 后台维护队列 "maintenance"   concurrency=2   priority=0
```

入队时根据 `business_account_id` 决定队列，让每个企业有独立的 inflight 上限；这是 new-api 原 DB 轮询难以表达的能力。

### 9.5 高可用与一致性（v1.1 修订：Asynq 是执行器、DB ledger 是真相源）

> **v1.1 修订动因：** Codex Minor 1 指出 v1 设计假设 Redis 高可用但没说细节，Asynq 跨节点消费但没说怎么处理重复消费 / 任务丢失；现有 `IsMasterNode` 控制的定时任务迁移没列清单。本节明确「**Asynq 是执行队列、不是真相源；DB ledger 与 task 表的状态 CAS 才是最终真相**」。

**核心原则：Asynq 重复执行无害**

任何 task handler 都必须可重入（exactly-at-least-once 语义下）：

```go
// 错误写法 (会重复扣费):
func handleTaskFetch(ctx, task) error {
    upstream := fetchUpstream(task.UpstreamID)
    if upstream.Status == "succeeded" {
        billing.Commit(task.ID, upstream.Usage)  // ❌ 重复消费会重复扣费
    }
}

// 正确写法 (v1.2 修订:与九ter.2 状态机统一):
// 任务状态流转: SUBMITTED → UPSTREAM_SUBMITTED → {COMPLETED/FAILED/...} → SETTLING → SETTLED
// fetch handler 只在 UPSTREAM_SUBMITTED 阶段处理上游回执,并把终态切到 COMPLETED/FAILED
// 真正的结算 (CAS 到 SETTLING) 由独立的 settle handler 完成
func handleTaskFetch(ctx, task) error {
    if task.Status != "UPSTREAM_SUBMITTED" {
        return nil  // 状态机已推进,本次重复触发直接放弃
    }
    upstream := fetchUpstream(task.UpstreamID)
    if upstream.Status == "pending" {
        return retry  // 上游还没完成,让 Asynq 重试
    }

    // 决定终态: COMPLETED / FAILED / CANCELLED / EXPIRED
    terminalStatus := mapUpstreamToTerminal(upstream)

    // CAS: UPSTREAM_SUBMITTED → <终态>,推进任务到终态
    ok := db.CompareAndSwapTaskStatus(task.ID, "UPSTREAM_SUBMITTED", terminalStatus)
    if !ok {
        return nil  // 已被其他 worker 处理
    }

    // 投递 settle 任务,settle handler 内做 <终态> → SETTLING → SETTLED 的 CAS
    asynq.Enqueue("task:settle", task.ID, asynq.TaskID("settle:"+task.ID))
    return nil
}

func handleTaskSettle(ctx, task) error {
    // 只有处于终态之一才执行结算
    terminalSet := []string{"COMPLETED", "FAILED", "CANCELLED", "EXPIRED"}
    if !slices.Contains(terminalSet, task.Status) {
        return nil
    }
    // CAS: <终态> → SETTLING
    fromStatus := task.Status
    ok := db.CompareAndSwapTaskStatus(task.ID, fromStatus, "SETTLING")
    if !ok {
        return nil  // 已被其他 worker settle
    }

    // 走账本结算(commit/release/refund),ledger 层 idempotency_key 再防一次
    billing.SettleWithSnapshot(task, "task_settle:"+task.ID)

    db.CompareAndSwapTaskStatus(task.ID, "SETTLING", "SETTLED")
    publishWebhook("task." + strings.ToLower(fromStatus))  // task.completed / task.failed / ...
    return nil
}
```

> **关键点（v1.2 修订）：** fetch 与 settle 拆成两个 handler，CAS 路径明确。fetch 的 CAS 从 `UPSTREAM_SUBMITTED` 推到终态；settle 的 CAS 从终态推到 `SETTLING`。这样无论 Asynq 重复消费哪个 handler，CAS 都会让重复的那次直接退出，与九ter.2 状态转移表完全一致。

**v1.2.2 修订：handleTaskSubmit 示例**（v1.2.1 版本"先调上游再 CAS"会留孤儿任务；v1.2.2 改为"先 CAS 本地抢占 + 上游 idempotency_key 双保险"）

```go
// 任务初次进队列时触发,把 SUBMITTED 推进到 UPSTREAM_SUBMITTED 或 FAILED
func handleTaskSubmit(ctx, task) error {
    if task.Status != "SUBMITTED" {
        return nil  // 状态机已推进或被其他 worker 处理,本次重复触发直接放弃
    }

    // v1.2.2: 先用乐观锁抢占本地提交权,把状态切到中间态 "UPSTREAM_SUBMITTING"
    // 抢占失败说明其他 worker 已在调上游,本 worker 完全放弃,不会调上游
    // 这彻底避免"两个 worker 同时调上游、输家事后取消但取消失败"产生孤儿
    // v1.2.3: 同时写 submit_locked_until 与 submit_locked_by,供崩溃恢复 cron 检测
    ok := db.CompareAndSwapTaskStatusWithFields(task.ID, "SUBMITTED", "UPSTREAM_SUBMITTING",
        map[string]any{
            "submit_locked_by":    workerID,
            "submit_locked_until": time.Now().Add(5 * time.Minute),
        })
    if !ok {
        return nil  // 已被其他 worker 抢占
    }

    // 抢占成功,本 worker 是唯一调上游的人
    // 上游侧也带 idempotency_key=task.ID,即使上游重试 / 网关重启重入,
    // 上游也只创建一个真实任务 (双保险)
    upstreamID, err := callUpstreamSubmit(task, /*idempotency_key=*/task.ID)
    if err != nil {
        if isFinalFailure(err, task.RetryCount) {
            // v1.2.3: 回退到 FAILED 同时清空 submit_locked_*,与 9ter.2 状态转移表一致
            db.CompareAndSwapTaskStatusWithFields(task.ID, "UPSTREAM_SUBMITTING", "FAILED",
                map[string]any{
                    "submit_locked_by":    nil,
                    "submit_locked_until": nil,
                })
            asynq.Enqueue("task:settle", task.ID, asynq.TaskID("settle:"+task.ID))
            return nil
        }
        // v1.2.3: 临时失败,回退到 SUBMITTED 同时清空 submit_locked_*
        db.CompareAndSwapTaskStatusWithFields(task.ID, "UPSTREAM_SUBMITTING", "SUBMITTED",
            map[string]any{
                "submit_locked_by":    nil,
                "submit_locked_until": nil,
            })
        return retry
    }

    // v1.2.3: 成功 UPSTREAM_SUBMITTING → UPSTREAM_SUBMITTED,同时清空 submit_locked_*
    ok = db.CompareAndSwapTaskStatusWithFields(
        task.ID, "UPSTREAM_SUBMITTING", "UPSTREAM_SUBMITTED",
        map[string]any{
            "upstream_task_id":      upstreamID,
            "upstream_submitted_at": time.Now(),
            "submit_locked_by":      nil,
            "submit_locked_until":   nil,
        },
    )
    if !ok {
        // 极罕见:本进程内 worker 持有 UPSTREAM_SUBMITTING 状态时被外部强制改写
        // (如运营手工干预),记 error 但不重做
        common.SysError("task " + task.ID + " status changed during submit")
        return nil
    }

    // 提交 fetch 任务,延迟 5 秒后首次拉状态
    asynq.Enqueue("task:fetch", task.ID,
        asynq.TaskID("fetch:"+task.ID),
        asynq.ProcessIn(5*time.Second),
    )
    return nil
}
```

**v1.2.2 新增中间状态 `UPSTREAM_SUBMITTING`**：作为「本 worker 已抢占提交权但还没拿到 upstream_id」的瞬态。九ter.2 状态转移表对应更新：

```
SUBMITTED → UPSTREAM_SUBMITTING  (worker CAS 抢占)
UPSTREAM_SUBMITTING → UPSTREAM_SUBMITTED  (上游成功)
UPSTREAM_SUBMITTING → SUBMITTED  (上游临时失败,放回让 Asynq 重试)
UPSTREAM_SUBMITTING → FAILED  (上游永久失败)
```

**上游 idempotency_key 配合（v1.2.2 强约束）：** 所有 `callUpstreamSubmit` 实现必须在请求里带 `idempotency_key = task.ID`（或上游 API 支持的等价机制，如 OpenAI 的 `Idempotency-Key` header、火山引擎的 `request_id` 等）。即使因为网关重启、网络重试导致同一 task 真的对上游发了两次请求，上游也只会创建一个真实任务，避免成本泄漏。

**关键：** `handleTaskSubmit` 与 `handleTaskFetch` / `handleTaskSettle` 共同构成完整的 Asynq handler 三件套，每个都用 DB CAS 防重复执行，与九ter.2 状态转移表严格对齐。

**v1.2.3 新增：`UPSTREAM_SUBMITTING` 崩溃恢复 cron**

worker 在 `UPSTREAM_SUBMITTING` 阶段崩溃（panic / OOM / 进程被杀），状态会卡住没人推。新增 Asynq cron 兜底：

```go
// Asynq cron "cron:task_submit_recover", 每分钟运行
func recoverStaleSubmitTasks(ctx) error {
    // 查所有 submit_locked_until 已过期的 UPSTREAM_SUBMITTING 任务
    rows := db.Where("status = ? AND submit_locked_until < ?",
                     "UPSTREAM_SUBMITTING", time.Now()).
              Find(&staleTasks)

    for _, task := range staleTasks {
        if task.SubmitRecoverCount >= 3 {
            // 超过 3 次恢复仍失败,转 FAILED 走结算流程
            ok := db.CompareAndSwapTaskStatus(task.ID, "UPSTREAM_SUBMITTING", "FAILED")
            if ok {
                asynq.Enqueue("task:settle", task.ID, asynq.TaskID("settle:"+task.ID))
                common.SysError(fmt.Sprintf("task %s submit recovery exhausted, marked FAILED", task.ID))
            }
            continue
        }
        // CAS 回 SUBMITTED + 递增 recover_count
        ok := db.CompareAndSwapTaskStatusWithFields(task.ID, "UPSTREAM_SUBMITTING", "SUBMITTED",
            map[string]any{
                "submit_locked_by":     nil,
                "submit_locked_until":  nil,
                "submit_recover_count": gorm.Expr("submit_recover_count + 1"),
            })
        if ok {
            // 重新入 Asynq 队列让其他 worker 接管
            asynq.Enqueue("task:submit", task.ID, asynq.TaskID("submit:"+task.ID))
            common.SysLog(fmt.Sprintf("task %s recovered from stale UPSTREAM_SUBMITTING (attempt %d)",
                task.ID, task.SubmitRecoverCount+1))
        }
    }
    return nil
}
```

**task 表新增字段（v1.2.3）：**
- `submit_locked_by` varchar(64) NULL —— 持有 UPSTREAM_SUBMITTING 的 worker ID
- `submit_locked_until` datetime NULL —— lease 截止时间
- `submit_recover_count` int NOT NULL DEFAULT 0 —— 已恢复次数

**为什么 lease 是 5 分钟？** 单次上游 submit 调用通常 < 30 秒；5 分钟覆盖网络抖动 + 上游慢响应 + 偶发 GC 暂停。崩溃恢复 cron 每分钟检查，最坏情况任务最多卡 6 分钟。

**Redis 持久化与 HA**

| 项 | 推荐 |
|---|------|
| 持久化模式 | **AOF (everysec)** ——重启最多丢 1 秒；不依赖 RDB |
| 内存策略 | `maxmemory-policy noeviction` ——队列数据**不允许被驱逐**；queue 满应该让生产者 enqueue 失败而不是丢任务 |
| HA | 生产 Redis Sentinel；DR 场景 Redis Cluster（new-api 已支持 `REDIS_CONN_STRING` 哨兵格式） |
| 容量预估 | 单任务 payload ~5KB；按峰值 10k inflight 任务规划 50MB+ 队列内存 |
| 监控 | Asynq Server 上报指标 + Prometheus；DLQ 长度 / queue lag / processing time 三条核心告警 |

**任务唯一键与幂等**

每个任务在 enqueue 时必须带唯一键：

```go
asynqClient.Enqueue(
    asynq.NewTask("task:video:fetch", payload),
    asynq.TaskID("task_video_fetch:" + taskID),  // 唯一键
    asynq.Retention(7 * 24 * time.Hour),
    asynq.MaxRetry(10),
)
```

Asynq 的 `TaskID` 在保留期内重复 enqueue 会被 dedup，避免 controller 误重复触发。

**`IsMasterNode` 迁移矩阵**

下表列出 new-api 现有的 master-only 后台任务对应的 Asynq 迁移方案：

| 原 master-only 任务 | 迁移方案 | 备注 |
|---------------------|---------|------|
| `controller.UpdateMidjourneyTaskBulk` (DB 轮询) | 改为提交时 `enqueue("task:mj:fetch")`；Asynq 跨节点消费 | P0 必做 |
| `controller.UpdateTaskBulk` | 同上，`task:video:fetch` / `task:suno:fetch` | P0 必做 |
| `controller.AutomaticallyUpdateChannels` | Asynq cron `cron:channel_update`，所有节点都能领；DB CAS 防重 | P1 |
| `controller.AutomaticallyTestChannels` | Asynq cron `cron:channel_health`，按 channel 分桶（每个 channel 同时只能被一个 worker 测） | P1 |
| `model.SyncChannelCache` / `SyncOptions` | **不迁移**——内存缓存同步本来就需要每节点跑一份，与 master 无关 | 维持现状 |
| `service.StartCodexCredentialAutoRefreshTask` | Asynq cron `cron:codex_refresh`，DB CAS 防重 | P1 |
| `service.StartSubscriptionQuotaResetTask` | Asynq cron `cron:subscription_reset` | P1 |
| `model.UpdateQuotaData` | **改为账本聚合视图**（见三ter），不再独立任务 | P1（账本上线后） |
| `controller.StartChannelUpstreamModelUpdateTask` | Asynq cron `cron:upstream_model_sync` | P1 |
| **v1.1 新增** `cron:storage_monthly_settle` | Asynq cron 每月 1 日 04:00 | P3 |
| **v1.1 新增** `cron:cost_catalog_check` | Asynq cron 每天 03:00 | P0 末期 |

迁移后**全面下线 `IsMasterNode` 判断**——所有节点逻辑均等，扩缩容无需关心主从。

**Asynq Web UI 接入**

`asynqmon` 挂到 `/admin/asynq/`，前置：
- new-api 的 Root + secure_verification 中间件（敏感操作二次认证）
- 仅 Root 用户可访问，admin 不行
- 集成 SSO 单点：复用 new-api 现有 session 验证（asynqmon 支持 reverse proxy auth）

**关键告警阈值**

| 指标 | 告警阈值 |
|------|---------|
| `asynq:dlq:size` | > 100 → 告警 |
| `asynq:queue:lag:p99` | > 60s → 告警 |
| `asynq:processing:duration:p99` | > 60s → 警告 |
| `asynq:redis:connection_errors` | > 10/min → 紧急告警 |

### 9.6 P0 阶段引入工作量

- Asynq Client/Server 封装 + Redis 持久化配置：3 天
- 现有 video / mj / suno controller 改造为 enqueue 模式：3 天
- 队列与租户配额配置 UI：2 天
- DB 轮询代码下线 + 双跑灰度 + 回归测试：4 天
- IsMasterNode 迁移到 Asynq cron（P0 仅做 MJ/Suno/video 相关，其他 P1）：2 天

合计约 2-3 周工作量，P0 即可完成。

---

## 九ter、任务财务状态机（v1.1 新增）

> **缘起：** Codex Important 3 指出异步任务长时间运行期间，若 business_account 被冻结 / 删除 / Key 被吊销，inflight 任务的行为没有定义；Provider Cost Catalog 价格在任务运行期间变化时，结算按提交时还是完成时不明；跨月任务归属、失败重试是否退预扣全部留白。本章定义任务从提交到最终结算的完整财务状态机。

### 9ter.1 设计原则

1. **提交时双快照**：任务进入网关瞬间冻结「**授权快照**」+「**价格快照**」，结算时**永远以快照为准**，不查"最新值"。
2. **跨月归属规则**：任务按 **提交时刻** 归属到对应账期，跨月不切分。
3. **账户状态变化不影响 inflight 任务**：suspend / delete 只阻止新提交，已 inflight 任务按快照继续；delete 必须前置校验「无 inflight + 无未结余额」。
4. **失败退预扣**：任务最终失败（超过重试上限）时，所有预扣 quota 通过账本 `release` 自动退回；上游已扣的费用通过 `refund` 入账。
5. **重试不重复扣费**：上游失败重试只在快照范围内进行，不重新触发预扣；只在最终成功或失败时一次性 commit / refund。

### 9ter.2 任务财务状态机（v1.2 修订：明确为唯一权威 + 状态转移表）

> **v1.2 修订动因：** Codex v1.1 评审 Critical 4 指出 9.5 Asynq handler 示例的 CAS 路径 (`SUBMITTED → SETTLING`) 与本节状态机 (`SUBMITTED → UPSTREAM_SUBMITTED → 终态 → SETTLING`) 冲突。本节作为**状态机的唯一权威定义**，9.5 示例已修订与本节一致。

**状态转移表（唯一权威）：**

| 当前状态 | 允许的下一状态 | 触发动作 | 由谁推进 |
|---------|--------------|---------|---------|
| `SUBMITTED` | `UPSTREAM_SUBMITTING` | worker 抢占本地提交权（v1.2.2 新增中间态，防孤儿）；同时写 `submit_locked_by` + `submit_locked_until = NOW()+5min` | Asynq `task:submit` worker CAS |
| `UPSTREAM_SUBMITTING` | `UPSTREAM_SUBMITTED` | 调上游 submit 成功，拿到 upstream_task_id；清空 submit_locked_* | Asynq `task:submit` worker |
| `UPSTREAM_SUBMITTING` | `SUBMITTED` | 上游临时失败，放回让 Asynq 重试；清空 submit_locked_* | Asynq `task:submit` worker |
| `UPSTREAM_SUBMITTING` | `FAILED` | 调上游 submit 失败超过重试上限 | Asynq `task:submit` worker |
| `UPSTREAM_SUBMITTING` | `SUBMITTED` | **v1.2.3 新增：超时回退**——`submit_locked_until < NOW()` 说明 worker 崩溃，cron job CAS 回 `SUBMITTED`（最多 3 次） | Asynq cron `cron:task_submit_recover` |
| `UPSTREAM_SUBMITTING` | `FAILED` | **v1.2.3 新增：超时上限**——`submit_recover_count >= 3` 转 `FAILED`，避免无限循环 | 同上 |
| `UPSTREAM_SUBMITTED` | `COMPLETED` | 上游回执 status=succeeded | Asynq `task:fetch` worker |
| `UPSTREAM_SUBMITTED` | `FAILED` | 上游回执 status=failed 超过重试 | 同上 |
| `UPSTREAM_SUBMITTED` | `CANCELLED` | 用户主动取消（业务系统调取消 API） | API handler |
| `UPSTREAM_SUBMITTED` | `EXPIRED` | 超过 `max_wait_time`（默认 30 天） | Asynq cron `task:expire_check` |
| `COMPLETED` / `FAILED` / `CANCELLED` / `EXPIRED` | `SETTLING` | settle 任务触发 | Asynq `task:settle` worker |
| `SETTLING` | `SETTLED` | 账本 commit/release/refund 完成 | 同上 |
| `SETTLED` | — | 终态，不可变 | — |

**所有 CAS 操作只能按上表方向；非法转移直接拒绝并写 critical 日志。**

```text
┌───────────────────────────────────────────────────────────────────┐
│                       SUBMITTED                                   │
│  - 写 task 表 + authorization_snapshot + pricing_snapshot         │
│  - 账本 reserve(estimated_cost) → reserved += estimated_cost      │
│  - 入 Asynq 队列 task:submit                                      │
└───────────────────────────────────────────────────────────────────┘
              │
              │ Asynq worker CAS 抢占 (v1.2.2)
              ▼
┌───────────────────────────────────────────────────────────────────┐
│            UPSTREAM_SUBMITTING (v1.2.2 中间态)                   │
│  - 本 worker 已抢占提交权,准备调上游                              │
│  - submit_locked_until = NOW()+5min (供崩溃恢复 cron)             │
│  - 临时失败 → 回 SUBMITTED 让 Asynq 重试                          │
│  - 崩溃超时 → cron 把状态 CAS 回 SUBMITTED (最多 3 次,超出 FAILED)│
└───────────────────────────────────────────────────────────────────┘
              │
              │ 上游 submit 成功
              ▼
┌───────────────────────────────────────────────────────────────────┐
│                       UPSTREAM_SUBMITTED                          │
│  - 已提交给上游(如火山 seedance API),拿到 upstream_task_id        │
│  - 进入轮询/回调等待                                              │
└───────────────────────────────────────────────────────────────────┘
              │
       ┌──────┴──────┬────────────┬──────────────┐
       ▼             ▼            ▼              ▼
  COMPLETED      FAILED      CANCELLED       EXPIRED
  (上游成功)    (上游失败    (用户主动      (超过 max_wait_time)
                 超过重试)    取消)
       │             │            │              │
       │             │            │              │
       ▼             ▼            ▼              ▼
┌──────────────────────────────────────────────────────────────────┐
│                  SETTLING (账本结算阶段)                         │
│  根据终态计算结算动作:                                            │
│                                                                  │
│  COMPLETED:                                                      │
│    - 解析上游 Usage → 用 pricing_snapshot 表达式重算 actual_cost │
│    - 账本: release(estimated_cost) + commit(actual_cost)         │
│    - 净扣 = actual_cost - 0 (release 抵消 reserve)               │
│                                                                  │
│  FAILED:                                                         │
│    - 账本: release(estimated_cost) 全额退预扣                    │
│    - 若上游已收费,加 refund(upstream_charged) 入账(进退款流水)   │
│                                                                  │
│  CANCELLED / EXPIRED:                                            │
│    - 类同 FAILED                                                 │
│    - 上游若有取消费用,按 commit(cancel_fee) 扣                   │
└──────────────────────────────────────────────────────────────────┘
              │
              ▼
┌──────────────────────────────────────────────────────────────────┐
│                       SETTLED (终态,不可变)                       │
│  - 推送 webhook task.completed / task.failed                     │
│  - 触发文件登记 task:oss:register (若有产出文件)                 │
└──────────────────────────────────────────────────────────────────┘
```

### 9ter.3 快照数据结构

`model/task.go` 的 `TaskPrivateData` 扩展：

```go
type TaskFinancialSnapshot struct {
    // 授权快照(冻结提交时刻的账户上下文)
    AuthSnapshot struct {
        BusinessAccountID string `json:"business_account_id"`
        TokenID           int64  `json:"token_id"`
        AccountStatus     string `json:"account_status"`     // 提交时的账户状态
        AvailableQuota    int64  `json:"available_quota"`    // 提交时的可用余额(审计用)
        MatchedRuleID     int64  `json:"matched_rule_id"`    // 命中的路由规则 ID
        ChannelID         int64  `json:"channel_id"`         // 选中的 channel
        ChannelCredVersion string `json:"channel_cred_version"` // 凭据版本
    }

    // 价格快照(冻结提交时刻的计费上下文)
    PricingSnapshot BillingSnapshot  // 见 5.4.3 BillingSnapshot 定义

    // 预扣记录(用于结算时 release)
    ReservationLedgerID int64 `json:"reservation_ledger_id"`
    EstimatedQuota      int64 `json:"estimated_quota"`

    // 提交时刻
    SubmittedAt int64 `json:"submitted_at"`
    // 跨月归属:固定为 submitted_at 的月份
    AccountingMonth string `json:"accounting_month"`  // "2026-04"
}
```

### 9ter.4 账户状态变化与 inflight 任务

| 账户操作 | 对 inflight 任务的影响 | 实现 |
|---------|---------------------|------|
| `suspend` | **不影响**已 inflight 任务，继续按快照执行；新任务提交时检查账户状态 → 拒绝 | distributor 入口检查 + 任务提交前最后校验 |
| `resume` | 无副作用 | |
| `delete` | **必须前置校验**：`SELECT count(*) FROM tasks WHERE business_account_id = ? AND status IN ('SUBMITTED','UPSTREAM_SUBMITTING','UPSTREAM_SUBMITTED','COMPLETED','FAILED','CANCELLED','EXPIRED','SETTLING')` = 0 且 `reserved = 0` 才允许；否则报错 `409 account_has_pending_obligations`（v1.2.3 把 UPSTREAM_SUBMITTING 与所有终态未 SETTLED 状态全纳入） | Admin API `DELETE /admin/api/business-accounts/{id}` 内置校验 |
| `Token 吊销` | **不影响**已 inflight 任务（已用快照中的 ChannelID 通过授权检查）；新请求拒绝 | TokenAuth 现有逻辑 |
| `Channel 禁用 / 凭据轮换` | inflight 任务用快照中的 `ChannelCredVersion` 解密旧凭据继续；新任务用最新凭据 | 凭据加密支持 key_version 多版本共存（见 8.2） |

### 9ter.5 重试与退预扣

| 场景 | 行为 |
|------|------|
| 上游临时失败（5xx / 限流） | Asynq 重试（exponential backoff），同一快照内重试，**不重新预扣** |
| 重试成功 | 走 COMPLETED 流程 |
| 超过 max retries（如 5 次） | 进入 FAILED 终态，release 预扣 |
| 上游已扣费但本网关重试失败 | release 预扣 + refund 上游已扣（基于上游账单 / 人工对账，进 `entry_type='refund'` 流水） |
| 上游成功但回调网关失败（如网关 panic） | Asynq DLQ 兜底；运营手工 reconcile 时根据 `upstream_task_id` 拉上游状态决定 COMPLETED / FAILED |

### 9ter.6 跨月任务归属（v1.2 修订：provisional + adjustment 模式）

> **v1.2 修订动因：** Codex v1.1 评审 Critical 5 指出原设计「月结 2 日推送 + 留 24 小时给跨月任务完成」数学矛盾：视频任务可能跑几天、上游排队、DLQ 人工恢复都可能超过 24 小时。v1.2 改用 provisional + adjustment 模式，不再写死窗口。

**归属规则（不变）：** 任务按 `submitted_at` 归属账期。

| 提交时刻 | 完成时刻 | 归属账期 |
|---------|--------|---------|
| 2026-04-30 23:50 | 2026-05-01 00:30 | 2026-04 |
| 2026-04-15 10:00 | 2026-04-15 10:20 | 2026-04（常规） |
| 2026-04-29 10:00 | 2026-05-07 14:00（长视频） | 2026-04 |
| 2026-04-29 10:00 | 永远不完成（人工 EXPIRED） | 2026-04 |

**月结流程（v1.2 改用两阶段）：**

```
账期 M（如 2026-04）每月 1 日 02:00 (M+1) 触发月结 Job:

  Phase 1: 立即生成 provisional 账单（不等任何跨月任务）
    ├─ 统计 M 月所有任务,按状态分类:
    │     - 已 SETTLED 的: 用最终 charged_quota
    │     - 未终结的 (SUBMITTED / UPSTREAM_SUBMITTING / UPSTREAM_SUBMITTED / 终态未 SETTLED):
    │         用预扣 quota 作为 provisional 金额,标记 is_provisional=true
    │         (v1.2.3 把 UPSTREAM_SUBMITTING 中间态也纳入未终结分类)
    ├─ 写月结 line item, status='provisional'
    ├─ 推送 Webhook billing.monthly_settle_provisional 给业务系统
    └─ 业务系统据此做"暂估账单",可以先发给客户

  Phase 2: 持续 adjustment（任务每完成一笔触发一次,直到该月所有任务 SETTLED）
    每个未终结任务最终 SETTLED 时:
    ├─ 计算 final_charged - provisional_amount = adjustment_delta
    ├─ 写一条 adjustment 流水(写入对应账期 M 的 settlement 表)
    ├─ 推送 Webhook billing.monthly_adjustment 携带 task_id / delta / 原因
    └─ 业务系统侧补充客户账单(可累积一周发一次,无须立即推给客户)

  Phase 3: 最终关账 (M+3 个月后,或所有 M 月任务终结)
    ├─ 标记账期 'finalized',此后不允许任何 adjustment
    ├─ 推送 Webhook billing.monthly_settle_finalized
    └─ 若还有 inflight 任务,强制 EXPIRED 处理(M+3 个月是任务最长执行期 30 天的安全余量)
```

**Webhook 事件清单更新：**

| 事件 | 时机 | 含义 |
|------|------|------|
| `billing.monthly_settle_provisional` | M+1 月 1 日 | 暂估账单，含 is_provisional 标记 |
| `billing.monthly_adjustment` | 跨月任务终结时 | 调整账单，含 task_id / delta |
| `billing.monthly_settle_finalized` | 账期最终关账 | 此后该账期不可变 |

**业务系统侧建议处理：**

- **暂估发客户**：拿到 provisional 账单时，可以根据策略「按提交时刻完整发」或「只发已 SETTLED 部分 + 暂估说明」
- **adjustment 累积**：不要每条 adjustment 都通知客户，建议每周或每月聚合一次补账
- **finalized 后争议处理**：若 finalized 后发现错误，走人工 refund 流程（写新的 ledger 流水），不修改历史账期

**性能与体验：**

- 月结 Phase 1 通常 < 5 分钟（只查 task 表 + ledger 投影）
- 跨月 adjustment 通常 ≤ M+1 月底前完成（视频任务最长 30 天 + 上游/DLQ 余量）
- 账期 finalized 通常在 M+3 个月（极端情况）

### 9ter.7 状态机实现

| 文件 | 改造 |
|------|------|
| `model/task.go` | task 表新增 `accounting_month` `reservation_ledger_id` 字段；`TaskPrivateData` 包含 `TaskFinancialSnapshot` |
| `service/task_billing.go` | 重写：所有预扣 / 结算走账本（见三ter），不直接操作 user.quota |
| `service/task_settle.go`（新增） | 终态处理：根据 task.status 调度 settle 动作 |
| `controller/task.go` / `controller/task_video.go` / `controller/midjourney.go` | 提交时调 `service.TaskAuthorizeAndReserve()` 构造双快照 + 入账本 reserve + 入 Asynq |
| Asynq handler | 失败 / 完成时调 `service.TaskSettle()` 触发结算 |

### 9ter.8 监控

- `task_inflight_count{business_account_id, status}` —— 各账户各状态任务数；status 维度包含 `SUBMITTED` / `UPSTREAM_SUBMITTING` / `UPSTREAM_SUBMITTED` / `COMPLETED` / `FAILED` / `CANCELLED` / `EXPIRED` / `SETTLING`（v1.2.3 补 UPSTREAM_SUBMITTING）
- `task_reservation_orphan_count` —— 预扣已超时但未结算的孤儿（应为 0）
- `task_settlement_lag_seconds` —— 任务完成到结算入账的延迟
- `task_refund_total` —— 退款笔数（按原因分桶：upstream_failure / manual / monthly_settle）
- `task_submit_recover_total{outcome}` —— v1.2.3 新增：UPSTREAM_SUBMITTING 崩溃恢复触发次数；outcome=`recovered_to_submitted` / `exhausted_to_failed`
- `task_submit_stale_count` —— gauge，当前处于 UPSTREAM_SUBMITTING 且 submit_locked_until < NOW() 的任务数（应快速归零，长期 > 0 说明 cron 没跑）

---

## 九bis、模块 7：业务系统接入协议（开通 + Webhook）

> **设计要点：** 业务系统与网关之间是「上层应用 + 基础设施」的依赖关系，业务系统调网关是**正确方向**，不是反向依赖。本节定义两套接口：业务系统主动调网关的「同步 API」+ 网关主动通知业务系统的「Webhook 事件」。

### 9bis.1 设计原则

1. **业务无知**：网关不接收营业执照、税号、法人、合同等业务细节。这些留在业务系统侧。
2. **认 ID 不认人**：网关只信 `business_account_id` 这个字符串作为业务侧的「已审核通过」凭据；业务系统调网关接口这件事本身就是"我已经审核通过了"的隐式契约。
3. **Webhook 不是反向依赖**：业务系统注册回调 URL 是运行时配置（不是编译时依赖）；网关代码不 import 任何业务系统类型；双方协议由网关定义。
4. **故障隔离**：Webhook 失败由 Asynq 重试队列承担（指数退避 + 死信），不阻塞网关主流程；网关故障不影响业务系统已缓存的 Token 调用其他路径。

### 9bis.2 业务系统 → 网关：开通流程

完整流程：

```text
1. 用户在业务系统提交资质 (营业执照等)
       ↓
2. 业务系统人工 / 自动审核
       ↓
3. 审核通过 → 业务系统调网关 POST /admin/api/business-accounts
       ↓
4. 网关创建账户 + 默认 Token,状态 pending_provision,返回给业务系统
       ↓
5. 网关运营接到通知 (管理后台待办或邮件)
       ↓
6. 运营在火山控制台为该企业创建 ARK 项目 + TOS 桶 + 项目 ID
       ↓
7. 运营在网关后台为该企业:
       a. 新建 N 个 Channel,录入 ChannelCredentials (5 项配置)
       b. 配置 Channel Routing Rules (基于 condition_expr 路由)
       c. (可选) 覆盖默认的 ModelBillingExpr (基础积分定价)
       ↓
8. 运营点击"激活" → 状态 active
       ↓
9. 网关向业务系统 webhook POST: account.activated
       ↓
10. 业务系统通知最终用户"账户已开通,可以使用 K_X 调用"
```

### 9bis.3 网关同步 API（业务系统调用）

所有 `/admin/api/*` 路径要求业务系统持有 **Admin Token**（不同于普通用户 Token，由网关运营在初次部署时颁发给业务系统，存到业务系统侧的安全配置）。

**开通业务账户：**

```http
POST /admin/api/business-accounts
Authorization: Bearer <gateway_admin_token>
Content-Type: application/json

{
  "business_account_id": "biz_001",          // 业务系统的企业 ID,业务系统是真相源
  "business_account_type": "enterprise",      // enterprise / individual / internal
  "display_name": "示例企业 X",                // 仅给运营人员看的标签
  "initial_quota": 1000000,                   // 初始充值积分 (可为 0)
  "metadata": {                               // 可选,业务侧愿意分享的非敏感标签
    "industry": "education",
    "tier": "premium"
  }
}

Response 200:
{
  "business_account_id": "biz_001",
  "default_token": "sk-xxxxx",                 // 自动创建一个默认 Token
  "status": "pending_provision",               // 等待运营配置上游凭据
  "created_at": "2026-05-25T10:00:00Z"
}
```

**充值：**

```http
POST /admin/api/business-accounts/{id}/recharge
{
  "quota": 500000,
  "reason": "monthly_topup",                   // 业务侧的对账标签
  "external_ref": "biz_tx_20260525_001"        // 业务侧充值流水号,网关侧幂等键
}
```

**查询当前余额与近期消费：**

```http
GET /admin/api/business-accounts/{id}/balance
GET /admin/api/business-accounts/{id}/usage?from=2026-05-01&to=2026-05-25&group_by=day
```

**冻结 / 解冻 / 销号：**

```http
POST /admin/api/business-accounts/{id}/suspend
POST /admin/api/business-accounts/{id}/resume
DELETE /admin/api/business-accounts/{id}
```

**为业务账户管理 Token：**

```http
POST /admin/api/business-accounts/{id}/tokens   # 新增子 Key (业务侧可分配给子用户)
GET  /admin/api/business-accounts/{id}/tokens
DELETE /admin/api/business-accounts/{id}/tokens/{token_id}
```

**重要：** 业务账户的 organization / 子账号 / 计费主体仍在业务系统侧。网关只提供「**为同一个 business_account_id 创建多个 Token**」的能力，让业务系统可以为下属团队 / 项目分配不同 Key 而不混淆账户。

### 9bis.4 Webhook 事件（网关 → 业务系统）

业务系统在网关后台一次性注册 Webhook 端点：

```http
POST /admin/api/webhooks
{
  "url": "https://platform.example.com/api/internal/gateway-webhooks",
  "secret": "<32-byte-random>",               // 用于 HMAC-SHA256 签名
  "events": ["account.*", "billing.*", "channel.*", "task.*"],  // 通配订阅
  "retry_policy": "exponential_3_attempts"
}
```

**Webhook 推送格式：**

```http
POST https://platform.example.com/api/internal/gateway-webhooks
Content-Type: application/json
X-Gateway-Event: account.activated
X-Gateway-Delivery-Id: <uuid>
X-Gateway-Timestamp: 1716624000
X-Gateway-Signature: sha256=<HMAC-SHA256(secret, timestamp + "." + body)>

{
  "event": "account.activated",
  "delivery_id": "<uuid>",
  "timestamp": 1716624000,
  "data": {
    "business_account_id": "biz_001",
    "activated_at": "2026-05-25T11:00:00Z",
    "channels_count": 3
  }
}
```

业务系统侧实现要点：
- **必须校验签名**：`HMAC-SHA256(secret, timestamp + "." + body) == X-Gateway-Signature`
- **必须校验时间戳**：拒绝超过 5 分钟的旧请求，防重放
- **幂等处理**：用 `delivery_id` 去重（**财务事件按 `event_id` 长期去重 ≥ 1 年**；非财务事件用 `delivery_id` 5 分钟窗口）
- **快速响应**：必须在 5 秒内返回 2xx，业务处理放入业务侧自己的队列；超时网关会重试

### 9bis.4.1 Outbox 模式与拉取 / 重放（v1.1 新增，v1.2 强化主库约束）

> **缘起：** Codex Important 5 指出 v1 设计只有推送 + 死信，业务系统短暂故障或验签 bug 后会永久错过财务事件。本节新增 outbox + 拉取 + 重放。
>
> **v1.2 修订动因：** Codex v1.1 评审 Critical 3 指出 new-api 支持 `LOG_SQL_DSN` 拆库，若 outbox 被归入日志库则「与 ledger 同事务」承诺失效。v1.2 强制 outbox 部署在主库。

**v1.2 硬约束：outbox 必须部署在主库（与 ledger 同库）**

| 表 | 落库 | 理由 |
|---|------|------|
| `webhook_event_outbox` | **主库（DB，与 `business_account_ledger` 同库）** | 必须能与 ledger entry 在同一事务内提交；分库则同事务承诺崩塌 |
| `webhook_delivery_log` | 主库或 LOG_DB（运维可选） | 仅为投递历史归档，可异步复制；丢失不影响财务正确性 |
| `webhook_subscription` | 主库 | 配置数据，量小 |

**启动时 fail-fast 校验（v1.2.1 修订：可靠的身份校验 + 事务对象传递）：**

> **v1.2.1 修订动因：** Codex Important 1 指出比较 `*gorm.DB` 指针不可靠——同库不同 GORM 实例会误报、不同库通过 wrapper 可能漏报。改为「比较 normalized DSN + 强制写入操作传同一个 `*gorm.DB` 事务对象」。

```go
// main.go 启动时
func validateDatabaseLayout() {
    mainDB := model.MainDB()
    if !mainDB.Migrator().HasTable(&model.WebhookEventOutbox{}) {
        common.FatalLog("webhook_event_outbox MUST be on the main DB " +
            "(same as business_account_ledger). " +
            "If LOG_SQL_DSN is set, outbox table must still be created on main DB.")
    }
    if !mainDB.Migrator().HasTable(&model.BusinessAccountLedger{}) {
        common.FatalLog("business_account_ledger missing from main DB")
    }

    // v1.2.2 修正: 比较 normalized DSN, 从各自 *gorm.DB 真实 Dialector 提取
    // 不能两边都从 os.Getenv("SQL_DSN") 取——若 outboxDB 被误配到 LOG_DB,
    // 同源归一化会漏检
    outboxDSN := normalizeDSN(extractDSN(model.OutboxDB()))
    ledgerDSN := normalizeDSN(extractDSN(model.LedgerDB()))
    if outboxDSN != ledgerDSN {
        common.FatalLog(fmt.Sprintf(
            "outbox and ledger MUST be on the same database. "+
            "outbox DSN=%s, ledger DSN=%s. "+
            "If LOG_SQL_DSN is set, outbox table must explicitly be created on main DB (SQL_DSN), not LOG_DB.",
            outboxDSN, ledgerDSN))
    }

    // v1.2.1: 额外校验 schema (PG schema / MySQL db) 一致
    var outboxSchema, ledgerSchema string
    model.OutboxDB().Raw(currentSchemaSQL()).Scan(&outboxSchema)
    model.LedgerDB().Raw(currentSchemaSQL()).Scan(&ledgerSchema)
    if outboxSchema != ledgerSchema {
        common.FatalLog("outbox and ledger MUST be in the same schema")
    }
}

// v1.2.2: 从 *gorm.DB 的 Dialector 中提取真实 DSN
// 不同驱动 Dialector 类型不同,需要 type switch
func extractDSN(db *gorm.DB) string {
    switch d := db.Dialector.(type) {
    case *mysql.Dialector:
        return d.DSN
    case *postgres.Dialector:
        return d.DSN
    case *sqlite.Dialector:
        return d.DSN  // SQLite 是文件路径,可直接比较
    default:
        common.FatalLog(fmt.Sprintf("unknown dialector type: %T", d))
        return ""
    }
}

func normalizeDSN(dsn string) string {
    // 例: postgres://user:pass@host:5432/db?sslmode=require → host:5432/db
    // 例: user:pass@tcp(host:3306)/db?charset=utf8 → tcp(host:3306)/db
    // SQLite: 直接返回文件绝对路径
    // 实现细节:解析后仅保留 host/port/dbname,丢弃凭据与参数
}

func currentSchemaSQL() string {
    if common.UsingPostgreSQL { return "SELECT current_schema()" }
    if common.UsingMySQL      { return "SELECT DATABASE()" }
    return "SELECT 'main'"  // SQLite
}
```

进程启动时如果上述校验失败，**直接 panic 退出**，不允许带病启动。

**事务对象传递强约束（v1.2.1 新增）：**

仅靠启动时校验不够——运行时也要保证 ledger 写入与 outbox 写入用**同一个 `*gorm.DB` 事务对象**。所有发布事件的代码必须采用如下模式：

```go
// service/ledger.go (示例:充值操作)
func (s *LedgerService) Recharge(ctx context.Context, biz string, amount int64, ref string) error {
    return s.db.Transaction(func(tx *gorm.DB) error {
        // 1. 写 ledger entry (使用 tx)
        entry := buildRechargeEntry(biz, amount, ref)
        if err := tx.Create(entry).Error; err != nil {
            return err
        }
        // 2. 更新 balance 投影 (使用 tx)
        if err := s.updateBalance(tx, biz, /*deltas*/); err != nil {
            return err
        }
        // 3. 发布 outbox 事件 (必须传 tx,不能用 s.db)
        return s.outbox.PublishInTx(tx, "account.recharged", payload)
    })
}

// service/outbox_dispatch.go
type OutboxService struct{}

// 必须接收 tx,不接受 *gorm.DB 让调用方误传外层 db
func (o *OutboxService) PublishInTx(tx *gorm.DB, event string, payload any) error {
    return tx.Create(&model.WebhookEventOutbox{
        Event: event,
        Data:  mustMarshal(payload),
        // ...
    }).Error
}
```

`PublishInTx` 的签名强制要求 `tx *gorm.DB` 参数；code review 必须确保调用方传的是事务对象而非外层 `db`，否则视为 bug。

可考虑 lint 规则：扫描所有 `outbox.Publish*` 调用，参数名不是 `tx` 时 fail build。

**LOG_SQL_DSN 拆库时的允许行为：**

- ✅ `logs` 表可以拆到 LOG_DB
- ✅ `webhook_delivery_log` 可以异步复制到 LOG_DB 供运维查询
- ❌ `webhook_event_outbox` **不允许**拆到 LOG_DB
- ❌ `business_account_ledger` **不允许**拆到 LOG_DB
- ❌ `business_account_balance` **不允许**拆到 LOG_DB

文档化：`docs/ops/database-layout.md` 明确列出主库与 LOG_DB 的表清单划分，运维操作必读。

**Outbox 事件表 `webhook_event_outbox`（v1.2.1 修订：加 claim/lease 字段）：**

| 字段 | 类型 | 说明 |
|------|------|------|
| `event_id` | bigint PK AUTO_INCREMENT | **单调递增**，业务系统的拉取游标 |
| `business_system_name` | varchar(64) | 目标业务系统 |
| `event` | varchar(64) | `account.activated` 等 |
| `data` | text JSON | 事件 payload |
| `checksum` | varchar(64) | SHA256(data)，供业务侧二次校验 |
| `is_financial` | bool | 是否为财务事件（影响保留期） |
| **`delivery_status`** | **varchar(16) NOT NULL DEFAULT 'pending'** | **v1.2.1 新增**：`pending` / `delivering` / `delivered` / `dead_letter` |
| **`locked_by`** | **varchar(64) NULL** | **v1.2.1 新增**：claim 该 event 的 worker 实例 ID（如 hostname + PID） |
| **`locked_until`** | **datetime NULL** | **v1.2.1 新增**：lease 截止时间（如 NOW() + 2min），超时即可被其他 worker 抢占 |
| **`delivery_attempts`** | **int NOT NULL DEFAULT 0** | **v1.2.1 新增**：投递重试次数 |
| **`last_pushed_at`** | **datetime NULL** | **v1.2.1 新增** |
| **`delivery_idempotency_key`** | **varchar(128) UNIQUE NULL** | **v1.2.1 新增**：业务侧用于去重的幂等键，通常 = `event_id` 或 UUID |
| `created_at` | datetime | |
| `retention_until` | datetime | 财务事件保留 ≥ 1 年；非财务保留 ≥ 30 天 |

**索引：** `(delivery_status, locked_until, event_id)` —— 扫描热路径。

**事件发布是事务的一部分（与 ledger 同事务写入，v1.2 强约束）：**

```text
BEGIN TX (on main DB)
  INSERT INTO business_account_ledger ...
  INSERT INTO webhook_event_outbox ...
COMMIT
  ↓ Asynq watcher 看到新 event 入队推送
```

事务边界仅由 ledger 与 outbox 限定；其他表（如 `logs`）可以异步写。

这样保证：**只要 ledger 有这笔流水，业务系统最终一定能拉到对应事件**（不会出现「网关扣了费但 webhook 永远没发出来」的情况）。

**拉取接口的认证授权（v1.2 新增）：**

- `GET /admin/api/events` 的 Admin Token 必须有 `event:read` scope（见 9bis.6）
- 拉取时**强制按 `business_system_name = token.business_system_name` 过滤**——业务系统 A 的 Token 无论传什么参数都拿不到业务系统 B 的事件
- 这条过滤在 SQL WHERE 层强加，不依赖应用层 if-else，避免漏过滤

**业务系统拉取接口（补偿用）：**

```http
GET /admin/api/events?since_id=12345&limit=100&event=billing.* HTTP/1.1
Authorization: Bearer <gateway_admin_token>

Response 200:
{
  "events": [
    {
      "event_id": 12346,
      "event": "billing.daily_summary",
      "data": { ... },
      "checksum": "sha256:...",
      "created_at": "2026-05-25T10:00:00Z"
    },
    ...
  ],
  "next_since_id": 12446,
  "has_more": true
}
```

业务系统建议在自己侧持久化「last_pulled_event_id」，每天定时拉一次差集（即使 webhook 全成功也拉一次做校验）。

**单条 / 批量重放接口（运维用）：**

```http
POST /admin/api/webhook-deliveries/{delivery_id}/replay
POST /admin/api/webhook-events/{event_id}/replay
POST /admin/api/webhook-events/replay-range?from_id=12345&to_id=12500&dry_run=true
```

**财务事件保留期与归档：**

- `account.recharge` / `billing.*` / `task.completed` / `task.failed` / `task.refunded` 标记 `is_financial=true`，保留 ≥ 1 年
- 1 年后归档到冷存储（OSS），保留 ≥ 7 年（合规要求）
- 非财务事件 30 天后清理

**完整性校验工具：**

业务系统可调 `GET /admin/api/events/integrity-check?from=2026-04-01&to=2026-04-30` 拿到「网关侧该业务系统在该时间段产生的事件总数 + 总 checksum」，与业务侧已接收的对账。

### 9bis.5 起步事件清单

| 事件 | 触发时机 | 关键 payload |
|---|---|---|
| `account.created` | `POST /business-accounts` 成功 | business_account_id, default_token |
| `account.activated` | 运营激活后 | business_account_id, channels_count |
| `account.quota_low` | 余额 < 阈值 (可配置,如 10%) | business_account_id, current_quota, threshold |
| `account.quota_exhausted` | 余额耗尽 | business_account_id, suspended_at |
| `account.suspended` | 因欠费 / 违规 / 手动暂停 | business_account_id, reason |
| `account.resumed` | 解冻 | business_account_id |
| `billing.daily_summary` | 每日 00:30 推送昨日账单 | business_account_id, date, total_quota, top_models |
| `billing.monthly_settle_provisional` | 每月 1 日推送上月暂估账单（v1.2.1 统一命名） | business_account_id, month, breakdown, is_provisional=true |
| `billing.monthly_adjustment` | 跨月任务终结时推送账单调整（v1.2.1 新增） | business_account_id, month, task_id, delta_quota, reason |
| `billing.monthly_settle_finalized` | 账期最终关账（v1.2.1 新增），此后该账期不可变 | business_account_id, month, final_breakdown |
| `channel.degraded` | 某 channel 健康度下降影响该企业可用性 | business_account_id, affected_models |
| `channel.recovered` | channel 恢复 | business_account_id, recovered_models |
| `task.completed` | 异步任务完成 (可选订阅,业务侧也可主动 poll) | task_id, business_account_id, output |
| `task.failed` | 异步任务失败 | task_id, business_account_id, error |

### 9bis.6 Admin Token 安全（v1.1 修订：scope 拆分 + IP allowlist + 额度阀门 + 强幂等）

> **v1.1 修订动因：** Codex Important 6 指出 v1 设计的 Admin Token 即使有 scope 和 100 QPS 限制，仍能在泄露后批量造账户 / 充值 / 消费造成大额损失；充值幂等只用 `external_ref` 忽略了不同 quota 复用同 ref 的攻击。本节加强 5 个维度。

**1）颁发与轮换（v1 已有，沿用）**

- 网关运营在管理后台「业务系统接入」页面创建 Admin Token，仅展示一次明文
- 平滑轮换：同一业务系统可同时持有 2 把活跃 Token，新 Token 接管 + 旧 Token 留 30 天过渡期 + 自动失效

**2）网络层准入（v1.1 新增）**

| 控制 | 配置 |
|------|------|
| **IP allowlist** | 每个 Admin Token 必须配置允许的源 IP 段（CIDR，可多条）；未在 allowlist 内的请求直接 401，不消耗限流配额 |
| **mTLS（可选）** | 高安全场景启用 mutual TLS，业务系统侧持有客户端证书（证书 fingerprint 绑定到 Token） |
| **强制 TLS 1.3** | 拒绝低于 TLS 1.3 的连接 |

**3）按动作拆 Scope（v1.1 新增）**

不再用一个 Token 包打所有动作，而是细粒度授权：

```
Scope                       默认包含动作
─────────────────────────────────────────────────────────
business_account:read       GET /business-accounts/*
business_account:create     POST /business-accounts
business_account:suspend    POST /business-accounts/{id}/suspend|resume
business_account:delete     DELETE /business-accounts/{id}
business_account:recharge   POST /business-accounts/{id}/recharge
business_account:refund     POST /business-accounts/{id}/refund
token:read                  GET /business-accounts/{id}/tokens
token:write                 POST/DELETE /business-accounts/{id}/tokens/*
webhook:manage              POST/GET/DELETE /webhooks/*
event:read                  GET /events
```

业务系统的**生产环境 Token** 通常只需要 `business_account:read + recharge + token:write + event:read`；**create / delete / refund** 应该用独立的运维 Token，仅授权给少数管理员脚本。

**4）额度阀门（v1.1 新增，最关键的 blast radius 控制）**

每个 Admin Token 配置硬阀门：

| 阀门 | 默认值 | 命中后行为 |
|------|--------|-----------|
| `daily_recharge_quota_limit` | 1,000,000（≈ 1000 元） | 超出该日额度的充值请求返回 `429 daily_quota_limit_exceeded`，告警邮件 |
| `daily_account_create_limit` | 10 | 当日创建超过 N 个账户拒绝 + 告警 |
| `single_recharge_max` | 500,000 | 单笔充值超出拒绝 |
| `requests_per_minute` | 600 | 全局 QPS，超出 429（细于 100 QPS） |
| `circuit_breaker` | 1 小时内 100 次 4xx/5xx | 自动暂停 Token 1 小时，邮件告警 |

阀门可由 Root 在管理后台为某 Token 调整（如批量数据迁移场景临时放宽）；任何调整动作进审计日志。

**5）充值幂等强化（v1.1 修订）**

v1 设计：用 `external_ref` 作幂等键。**v1.1 改进**：幂等键 = `sha256(external_ref + canonical_body)`：

| 场景 | 行为 |
|------|------|
| 同 `external_ref` + 同 body（含 quota / reason） | 命中幂等，返回原结果 200 |
| 同 `external_ref` + 不同 body | 返回 `409 idempotency_conflict`，详情含原请求摘要 + 当前请求摘要；**写审计 + 告警** |
| 新 `external_ref` | 正常处理 |

业务系统侧重试逻辑必须保证「相同业务 ref 不改金额」——这是与网关的合约。

**6）审计与监控（v1 + v1.1 增强）**

- 所有 Admin API 调用写审计日志：`token_id / business_system / source_ip / method / path / request_hash / response_status / duration_ms`，保留 ≥ 1 年
- 高敏感动作（`refund` / `delete` / 阀门调整）实时推送告警到管理员邮件
- `admin_api_request_total{token_id, scope, status}` 指标
- `admin_api_quota_exceeded_total{token_id, quota_type}` 指标

### 9bis.7 数据库变更

**新增 `business_account` 表（v1.2 修订：删除 quota 字段 + 加 isolation_required）：**

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | bigint PK | 网关内部 ID |
| `business_account_id` | varchar(64) UNIQUE | 业务系统侧的 ID（外键，单一真相源） |
| `business_account_type` | varchar(16) | enterprise / individual / internal |
| `display_name` | varchar(128) | 运营标签 |
| `status` | varchar(32) | pending_provision / active / suspended / deleted |
| ~~`quota` / `used_quota`~~ | ~~bigint~~ | **v1.2 删除**：余额唯一在 `business_account_balance`，本表不存余额 |
| **`isolation_required`** | **bool NOT NULL DEFAULT false** | **v1.2 新增**：是否启用企业隔离硬模式。true 时该账户的所有路由必须命中本企业专属 channel，禁止任何形式的跨企业降级（含 `global_pool` / `legacy_distributor`），仅允许 `strict` / 同企业 `next_rule` 两种 fallback_policy；详见 8.3.6 |
| **`break_glass_until`** | **datetime NULL** | **v1.2 新增**：临时允许跨企业降级（break-glass）的截止时间；非 NULL 且未过期时本次调用允许降级，但每次降级写 critical 审计日志 + 推送 webhook account.break_glass_used；break-glass 仅 Root 双人审批可设置，最长 24 小时 |
| `metadata` | text JSON | 业务侧可选标签 |
| `admin_token_id` | bigint | 创建者（Admin Token） |
| `created_at` / `updated_at` / `activated_at` / `suspended_at` | 时间字段 | |

**新增 `gateway_admin_token` 表：**

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | bigint PK | |
| `business_system_name` | varchar(64) | 例：`creator_platform` |
| `token_hash` | varchar(128) | Token 的 SHA256，原文不入库 |
| `status` | enum | active / rotating / revoked |
| `created_at` / `expires_at` / `revoked_at` | 时间字段 | |
| `last_used_at` | datetime | |

**新增 `webhook_subscription` 表：**

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | bigint PK | |
| `business_system_name` | varchar(64) | |
| `url` | varchar(512) | |
| `secret_cipher` | text | AES-GCM 加密的 secret |
| `events` | text JSON | 订阅的事件 pattern 列表 |
| `enabled` | bool | |

**新增 `webhook_delivery_log` 表（保留 7 天）：**

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | bigint PK | |
| `subscription_id` | bigint FK | |
| `event` | varchar(64) | |
| `delivery_id` | varchar(64) UNIQUE | |
| `payload` | text | |
| `response_status` | int | HTTP 状态码 |
| `attempts` | int | 重试次数 |
| `final_status` | enum | delivered / failed_retrying / dead_letter |

### 9bis.8 关于反向依赖的最后澄清

| 担忧 | 真相 |
|---|---|
| "业务系统调网关 = 业务系统依赖网关" | ✓ 是依赖，但**方向正确**——下层基础设施被上层应用调用，本来如此 |
| "网关调业务系统 = 反向依赖" | ✗ 网关不调业务系统的任何 API；Webhook 是**业务系统主动订阅**网关事件，协议由网关定义 |
| "我们想做更解耦怎么办" | 可选用消息队列（如 Kafka）让网关只发布事件、业务系统消费，但运维复杂度上一个台阶；Webhook 已经够用 |

---

## 十、计费数据流（完整版）

```
┌───────────────────────────────────────────────────────────────────────┐
│ 客户端请求 (POST /v1/chat/completions 或 /mj/submit/* 或 /suno/...)    │
└───────────────────────────────────────────────────────────────────────┘
                              │
       ┌──────────────────────┼─────────────────────┐
       ▼                      ▼                     ▼
   TokenAuth            ModelRateLimit         Distribute
   (现有)               (现有)                  (现有,选渠道)
       │                      │                     │
       └──────────────────────┴─────────────────────┘
                              ▼
                ┌─────────────────────────────┐
                │ Relay Handler                │
                │  (text / task / image / ...) │
                └─────────────────────────────┘
                              │
                              ▼
                ┌─────────────────────────────────────────────┐
                │ Pre-Consume (现有 + 扩展)                   │
                │  1. 读 ModelBillingMode/Expr                │
                │  2. 估算变量:                               │
                │     - LLM: 估 token                         │
                │     - 任务: 默认分辨率/时长(请求参数)        │
                │  3. RunExprWithRequest(v1/v2/vp)            │
                │  4. cost() / storage_cost() 查 Catalog,     │
                │     版本号写入 BillingSnapshot              │
                │  5. 冻结 Snapshot, 扣预估 quota             │
                └─────────────────────────────────────────────┘
                              │
                              ▼
            ┌────────────────────────────────────┐
            │ 调用上游 (同步 chat / 异步 task)    │
            └────────────────────────────────────┘
                              │
                              ▼
                ┌─────────────────────────────────────────────┐
                │ Settlement (现有 + 扩展)                    │
                │  1. 任务回执解析 → 填 dto.Usage.Multimedia  │
                │  2. 任务输出文件登记 oss_object_meta        │
                │  3. 用 Snapshot 表达式重算 (真实 usage)     │
                │  4. 差额结算 (TryTieredSettle 现有逻辑)     │
                │  5. 写 logs (新增字段:                      │
                │     provider_cost / storage_cost_estimated  │
                │     / charged / profit)                     │
                └─────────────────────────────────────────────┘
                              │
        ┌─────────────────────┴──────────────────────┐
        ▼                                            ▼
┌──────────────────┐                  ┌────────────────────────────┐
│ 实时仪表盘        │                  │ 月度对账 Job (新增)        │
│ (新增 profit 维度)│                  │  每月 1 号 04:00            │
└──────────────────┘                  │  1. 拉 OSS/TOS Inventory   │
                                      │  2. 算真实存储成本           │
                                      │  3. 与预扣冲销               │
                                      │  4. 写 storage_settlement_log│
                                      └────────────────────────────┘
```

---

## 十一、日志与看板扩展

### 11.1 调用日志新增字段（写入现有 `logs.other` JSON）

```json
{
  "billing_mode": "tiered_expr",
  "expr_version": "v2",
  "expr_b64": "...",
  "matched_tier": "1080p:10s",
  "provider": "bytedance_jimeng",
  "provider_cost_usd": 0.45,
  "provider_cost_catalog_ids": [123, 456],
  "storage_cost_usd_estimated": 0.002,
  "storage_file_ids": ["f_abc", "f_def"],
  "charged_quota": 230000,
  "profit_usd": 0.158
}
```

### 11.2 仪表盘新增看板

复用 `model/usedata*.go` 聚合机制，新增维度：

| 看板 | 指标 |
|------|------|
| 模型利润榜 | 按 model 聚合 `sum(charged) - sum(provider_cost) - sum(storage_cost)` |
| 渠道毛利 | 按 channel 聚合 |
| 租户消费 / 贡献利润 | 按 user_id 聚合 |
| 分辨率分布 | 按 `matched_tier` 聚合（视频 480p/720p/1080p、图像 1k/2k/4k） |
| 失败率 × 失败原因 | 按 channel × failure_code |
| OSS 月度对账 | 直接展示 `storage_settlement_log`，分租户/分 bucket/分 storage_class |

后期数据量上来，可按选型文档建议接 ClickHouse + Grafana。

---

## 十二、安全 / 合规要点

- **价目人工维护必须双人复核**：管理后台 `cost_catalog` 编辑操作触发 `middleware/secure_verification.go`（已有），即修改人必须重新二次认证；新版本默认 `draft`，需要第二个有权限的运营点击「批准」才生效。
- **变价告警必须人工处理后才允许保存新版本**：避免运营漏看告警直接覆盖。
- **`storage_cost()` 必须用 snapshot 引用**：避免月结时价目已变导致旧调用日志价格错乱。
- **跨币种换算**：USD ↔ CNY 汇率从配置项读取（手工维护，避免实时汇率接口风险），变价告警同样适用。

---

## 十三、分阶段实施路线

| 阶段 | 工作 | 产出 |
|------|------|------|
| **P0（3-4 周，v1.2 收敛）** | **四条工作流，E 是前置依赖：**<br>**E. [账本基础设施]** `business_account_ledger` + balance 严格投影 + 原子 CAS + drift 自动冻结 + 删除 business_account.quota + user/token.quota 全只读 + reconcile job（解 v1.1-C1）<br>**B-min. [账号路由 fail-closed]** `token.business_account_id` + Distributor allowed_channel_ids 强制约束 + `fallback_policy` 默认 strict + `isolation_required` 硬开关 + break-glass 流程（解 v1.1-C2）。**凭据加密、结构化 ChannelCredentials 编辑 UI 仅做最简版本，envelope encryption 推后到 P1**<br>**C-min. [outbox 主库事务]** `webhook_event_outbox` 部署主库 + 启动 fail-fast 校验 + 与 ledger 同事务发布事件（解 v1.1-C3）。**完整 webhook 拉取/重放 / Asynq 推送链路推到 D-min**<br>**D-min. [最小 Admin API]** `business_account` 表 + `gateway_admin_token`（带 scope/IP allowlist/阀门）+ `POST /admin/api/business-accounts` + `POST .../recharge`（含强幂等 hash）+ 最基础 Webhook 推送（无 outbox 拉取接口） | 账本真相源唯一 + 隔离硬约束 + 事件不丢失 + 业务系统可开通账户和充值 |
| **P1（3-4 周）** | 1. **Asynq 引入**：A 工作流（替换 DB 轮询，含 IsMasterNode 迁移）<br>2. **完整 Webhook 链路**：outbox 拉取接口 `GET /events?since_id=` + 重放接口 + asynq 推送 worker + DLQ<br>3. **凭据 envelope encryption**：完整 ChannelCredentials + KEK v1/v2 + 轮换 job + 结构化前端编辑器（解 v1.1-I1 + v1.1-I6）<br>4. **billingexpr v2/vp**：F 工作流（独立 runner + cost()/storage_cost()/usage() 内置函数 + 任务双快照 + 状态机实现）（解 v1.1-I2） | 多媒体计费跑通 + 完整 webhook 补偿 + 凭据安全 |
| **P2（3 周）** | 1. **`provider_cost_catalog` + 变价检测**：C 工作流完整版（管理后台 UI + 录入 + 变价 hash 告警）<br>2. **dto.Usage.Multimedia 扩展** + 各 task adaptor 标准化 + 试点 2-3 个模型<br>3. **前端编辑器扩展**（分辨率档位表 / 时长矩阵）<br>4. **日志新增字段** + 利润看板 | 覆盖主力多媒体模型 + 成本目录可审计 |
| **P3（3 周）** | 1. **OSS 月结**：`oss_object_meta` + `storage_billing_object_item` 对象级不可变明细（解 v1.1-I3）<br>2. `storage_cost()` 内置函数 + 月结 Job + provisional/adjustment 模式（解 v1.1-C5）<br>3. 对账看板 | OSS 成本完整入账 |
| **P4（持续）** | 1. 双人复核流程上线<br>2. 接入更多模型<br>3. 销售层（业务系统侧）按需启动 | 进入运营态 |

---

## 十四、风险与未决项

| 风险 / 未决项 | 说明 | 应对建议 |
|---|---|---|
| `dto.Usage.Multimedia` 是新结构，已接入的 LLM 代码会有大量 nil 检查 | | 全部走 `usage.GetMultimedia() *MultimediaUsage`，nil-safe getter |
| 各厂商任务回执的 usage 格式差异巨大（即梦没有 `video_seconds`，Kling 有；Sora 用 `duration` 字段） | | 各 channel adaptor 自己做标准化，文档化每家的字段映射 |
| `v2` 表达式的 USD → quota 换算依赖汇率 | | 汇率改为 catalog 的一类 SKU（`sku_key = "fx:usd_cny"`），同样走变价检测 |
| 月结 Job 跨月失败如何回滚 | **v1.2 已解** | 6.3 改用 `storage_billing_object_item` (`(job_run_id, provider, bucket, object_key)` 唯一) + `job_run` 分阶段 apply + ledger idempotency_key 三层防重，失败重跑无副作用 |
| 现有 `tiered_pricing` 编辑器是 jsx 写在 `web/classic/` 还是 `web/default/`？ | 已经分析过两套并存，主用 default | 新 UI 只开发 default 版本，classic 保持原样 |
| 加密：cost catalog 含商务合同价 | 部分 SKU 可能是商务保密 | catalog 表新增 `is_confidential` 字段，前端按操作员权限脱敏（root 可见、admin 脱敏） |

---

## 十五、与 new-api 强约束的合规检查（`CLAUDE.md`）

| 规则 | 本方案符合性 |
|------|--------------|
| Rule 1 JSON 包装 | 所有 JSON 操作走 `common.Marshal/Unmarshal` ✅ |
| Rule 2 三库兼容 | 新表设计已考虑 SQLite / MySQL / PG 类型差异 ✅ |
| Rule 3 前端用 Bun | UI 开发遵守 ✅ |
| Rule 4 新渠道 StreamOptions | 多媒体任务多为异步，不涉及 ✅ |
| Rule 5 保护品牌信息 | 不改 new-api / QuantumNous 任何标识 ✅ |
| Rule 6 上游 DTO 用指针 + `omitempty` | `MultimediaUsage` 用指针，子字段标量也用指针型 ✅ |
| Rule 7 改 billingexpr 必读 expr.md | 新增 `v2` / `vp` 严格按版本派发机制扩展，不破坏 v1 ✅ |

---

## 十六、附：第一阶段（P0）具体落地清单（v1.2 收敛：4 条工作流）

> **v1.2 修订动因：** Codex v1.1 评审 Important 3 + 准入标准 5 指出 5-6 周做 6 条工作流不现实，建议收缩到 ledger + routing fail-closed + outbox 主库事务 + 最小 Admin API 四项；Asynq、完整 webhook 链路、凭据 envelope encryption、billingexpr v2/vp、OSS 月结、完整 UI 后移 P1-P3。

P0 四条工作流：**E (账本) 是其余三条的前置依赖，必须最先完成或与其余并行但提早 1-2 周完成。**

**P0 不包含的内容（明确推后）：**
- ❌ Asynq 引入（推到 P1）—— 新 webhook 暂用同步推送 + 简单内存重试队列
- ❌ 完整 Webhook 拉取/重放接口（推到 P1）—— P0 仅做基础推送
- ❌ ChannelCredentials envelope encryption + 多版本 KEK 轮换（推到 P1）—— P0 用单一 AES-GCM 密钥过渡
- ❌ ChannelCredentials 结构化前端编辑器（推到 P1）—— P0 后台直接 SQL 配置即可
- ❌ billingexpr v2/vp + 任务双快照状态机（推到 P1，即原工作流 F）
- ❌ Provider Cost Catalog（推到 P2）
- ❌ OSS 月结（推到 P3）
- ❌ 完整管理后台 UI（推到 P1/P2）—— P0 暴露 RESTful API 即可

### 工作流 E：账本基础设施（v1.2 收敛：唯一真相源，**前置依赖**）

> 解 Codex v1 Critical 1 + v1.1 Critical 1：建立账本作为**唯一**真相源，删除 business_account.quota，balance 是严格投影 drift 即冻结，user/token.quota 全只读。

- [ ] `model/business_account_ledger.go`（新建）—— 不可变流水表 + GORM 模型
- [ ] `model/business_account_balance.go`（新建）—— **v1.2 严格投影**（不是缓存）+ `frozen` / `frozen_at` / `frozen_reason` 字段
- [ ] `service/ledger.go`（新建）—— `Recharge` / `Reserve` / `Commit` / `Release` / `Refund` 五个核心操作 + 原子 CAS
- [ ] `service/ledger_reconcile.go`（新建）—— **v1.2 drift 检测每 5 分钟运行，命中即冻结账户**（不仅告警）
- [ ] `service/balance_rebuild.go`（新建）—— 单账户从 ledger replay 重建投影（运营手工触发）
- [ ] **v1.2.1** `model/user.go` / `model/token.go` —— quota 字段迁移 `int → BIGINT`；**P0 阶段保留可写但内部转调 ledger**；T+60 天才真正 410 Gone；新增静态检查规则卡控（除 COMPAT 适配层外禁止新代码写）
- [ ] **v1.2.1** 现有 controller 改造为内部转调 ledger 适配层（`TopUpUser` / `EditToken` 等），加 `Deprecation` header
- [ ] **v1.2** sync job 每 5 分钟从 ledger 同步 user.quota / token.RemainQuota 供管理后台展示
- [ ] `service/pre_consume_quota.go` —— 重写：完全走账本 reserve
- [ ] `service/quota.go` —— 重写：完全走账本 commit / release / refund
- [ ] **v1.2** Data migration job —— user.quota 初始 recharge 入 ledger + balance；支持 `legacy_user_<id>` 占位 + 业务系统接管转账
- [ ] **v1.2** Drift 触发账户冻结路径单元测试
- [ ] 单元测试：高并发预扣的原子性测试（启 100 goroutine 同时扣，余额必须不超卖）
- [ ] 集成测试：充值 → 预扣 → 结算 → 退款全链路 + 跨节点并发

### 工作流 B-min：账号映射 + 路由 fail-closed（v1.2 P0 简化版）

> 解 v1.1-C2：仅做账号 ID 透传 + Distributor fail-closed + isolation_required 硬开关。**不做** ChannelCredentials 完整 envelope 加密（推 P1）。

- [ ] `model/token.go` —— 增加 `business_account_id` / `business_account_type` 字段及 migration
- [ ] `middleware/auth.go` 的 `TokenAuth` —— 注入 `ContextKeyBusinessAccountId`
- [ ] `model/channel.go` —— 增加 `restricted_business_accounts` / `channel_purpose` 字段
- [ ] `model/business_account.go` 增加 `isolation_required` / `break_glass_until` 字段
- [ ] `model/channel_routing_rule.go`（新建）—— GORM 模型 + `fallback_policy` 字段（**默认 strict**）
- [ ] `service/channel_routing.go`（新建）—— 路由规则匹配逻辑 + normalized context 抽取 + **v1.2 isolation_required 硬校验**
- [ ] `setting/routing_setting/whitelist.go`（新建）—— body / header 字段白名单（约 30 个核心字段先起步）
- [ ] `middleware/distributor.go` —— 前置路由判断 + `restricted_business_accounts` 过滤 + **v1.2 strict 不允许跨企业降级**
- [ ] `service/channel_affinity.go` —— `GetPreferredChannelByAffinity` 加 `allowed []int` 参数
- [ ] `model/channel_satisfy.go` —— `CacheGetRandomSatisfiedChannel` 加 `allowed []int` 参数
- [ ] `controller/channel_routing.go`（新建）—— RESTful CRUD（**仅 API，UI 推 P1**）
- [ ] **v1.2** Break-glass 流程：Root 双人审批 UI + critical 审计日志 + webhook
- [ ] 单元测试：fail-closed 路径 + isolation_required 校验拦截
- [ ] 集成测试：seedance 多企业多渠道场景

**P0 简化省略**（推到 P1）：
- ❌ ChannelCredentials envelope encryption + KEK/DEK + 多版本（P0 用单一 AES-GCM 过渡）
- ❌ ChannelCredentials 结构化前端编辑器（P0 后台直接 SQL 配置或临时 API）
- ❌ 路由表达式试算面板 / Channel Routing 完整管理 UI

### 工作流 C-min：outbox 主库事务（v1.2 P0 简化版）

> 解 v1.1-C3：仅做 outbox 表 + 与 ledger 同事务发布 + 启动校验。**不做** 完整 Asynq 推送链路与拉取/重放接口（推 P1）。

- [ ] `model/webhook_event_outbox.go`（新建）—— outbox 表 + 单调递增 `event_id` + `is_financial` / `retention_until` 字段
- [ ] `service/outbox_dispatch.go`（新建）—— 在 ledger 同事务内 INSERT outbox（提供 `PublishEventInTx(tx, event)` 接口给 ledger 操作复用）
- [ ] **v1.2** `main.go` 启动 fail-fast 校验：outbox 表必须在主库 + outbox/ledger 共享 DB 连接
- [ ] `docs/ops/database-layout.md`（新建）—— 明确主库与 LOG_DB 的表清单划分
- [ ] **P0 推送（v1.2.1 修订：DB claim/lease 模式，多节点安全）**：每节点起 1 个 dispatcher goroutine，每 5 秒扫一次：
  ```sql
  -- v1.2.1: 使用 SELECT FOR UPDATE SKIP LOCKED 防多节点重复推送 (PG/MySQL 8.0+)
  BEGIN;
    SELECT event_id, data FROM webhook_event_outbox
    WHERE delivery_status = 'pending'
       OR (delivery_status = 'delivering' AND locked_until < NOW())  -- 抢占超时 lease
    ORDER BY event_id
    LIMIT 100 FOR UPDATE SKIP LOCKED;
    UPDATE webhook_event_outbox
    SET delivery_status='delivering',
        locked_by=:worker_id,
        locked_until=NOW() + INTERVAL '2 minutes'
    WHERE event_id IN (...);
  COMMIT;
  -- 然后 POST 给业务系统,成功后 UPDATE delivery_status='delivered'
  -- 失败累计 delivery_attempts++,达到 10 次进 dead_letter
  ```
  - SQLite 不支持 `SKIP LOCKED`，降级为 CAS 模式（v1.2.2 补 expired 抢占）：
    ```sql
    -- v1.2.2: 单事务两步 CAS,既抢 pending 也抢 expired delivering
    BEGIN IMMEDIATE;  -- SQLite 写锁
      -- 步骤 1: 选候选 (pending 或 expired delivering)
      SELECT event_id FROM webhook_event_outbox
      WHERE delivery_status = 'pending'
         OR (delivery_status = 'delivering' AND locked_until < datetime('now'))
      ORDER BY event_id LIMIT 100;
      -- 步骤 2: 对每条 CAS, 同时校验 status 和 locked_until,影响 0 行说明被其他 worker 抢先
      UPDATE webhook_event_outbox
      SET delivery_status='delivering',
          locked_by=:worker_id,
          locked_until=datetime('now', '+2 minutes')
      WHERE event_id=:eid
        AND (delivery_status='pending'
             OR (delivery_status='delivering' AND locked_until < datetime('now')));
    COMMIT;
    ```
    SQLite 整库写锁意味着同节点单 dispatcher 即可（不会有多 dispatcher 并发问题），但多节点 SQLite 部署本身不推荐（生产场景请用 PG/MySQL）
- [ ] `delivery_idempotency_key` 在事件入 outbox 时填充（用 `event_id` 作为默认值即可），业务侧用该键去重
- [ ] worker_id = `os.Hostname() + "_" + strconv.Itoa(os.Getpid())`，便于排查
- [ ] DLQ 处理：`delivery_attempts >= 10` 标记 `dead_letter`，发邮件告警；P1 接 Asynq 时替换扫描逻辑
- [ ] 单元测试：事务回滚后 outbox 不写入（验证同事务）
- [ ] 集成测试：多节点并行扫描，验证无重复推送（用 mock webhook endpoint 计数）

**P0 简化省略**（推到 P1）：
- ❌ Asynq webhook 推送 worker（P0 用 goroutine 轮询，P1 换 Asynq）
- ❌ `GET /events?since_id=` 拉取接口（P0 业务系统暂只能等推送）
- ❌ `POST /webhook-events/{id}/replay` 重放接口
- ❌ `webhook_delivery_log` 完整版（P0 仅 outbox 表加 `last_pushed_at` / `push_attempts` 字段）

### 工作流 D-min：最小 Admin API（v1.2 P0 简化版）

> 解 v1.1-I3：仅做 business_account 开通 + 充值 + 基础查询 + Admin Token 安全 5 件套；完整 UI / 完整 webhook 订阅管理推 P1。

- [ ] `model/business_account.go`（新建）—— GORM 模型与 CRUD（**v1.2 不含 quota 字段**，含 `isolation_required` / `break_glass_until`）
- [ ] `model/gateway_admin_token.go`（新建）—— Admin Token + **v1.2 ip_allowlist / scopes / 阀门字段全部 P0 必做**
- [ ] `controller/admin/business_account.go`（新建）—— `POST /admin/api/business-accounts` / `POST .../recharge` / `POST .../suspend` / `POST .../resume` / `DELETE` / `GET .../balance`
- [ ] **v1.2** `POST .../recharge` 充值幂等键 = `sha256(external_ref + canonical_body)`；冲突返回 409
- [ ] **v1.2** `POST .../refund` 退款接口（走账本 refund + 推 webhook `account.refunded`）
- [ ] `middleware/admin_token_auth.go`（新建）—— Admin Token 鉴权 + scope 检查 + IP allowlist + 限流 + **v1.2 阀门**（daily_recharge_quota_limit / daily_account_create_limit / single_recharge_max / circuit_breaker）
- [ ] `router/api-router.go` —— 注册 `/admin/api/business-accounts/*` 路由
- [ ] 在关键业务点埋点发布事件到 outbox：`account.created` / `account.activated` / `account.recharged` / `account.refunded` / `account.suspended` / `account.frozen`（v1.2 新增 drift 冻结事件）
- [ ] **P0 文档**：`docs/api/admin-api.md` 最小版本（开通 + 充值 + 查询 + 退款 4 个接口规范）
- [ ] 单元测试：充值幂等冲突 / 阀门触发 / IP allowlist 拒绝 / scope 不足

**P0 简化省略**（推到 P1）：
- ❌ 完整 Webhook 订阅管理接口（P0 业务系统的回调 URL 走配置文件 / 环境变量）
- ❌ `controller/admin/webhook.go` 订阅 CRUD
- ❌ `web/default/src/features/system-settings/business-systems/` 管理 UI
- ❌ Admin Token UI 创建/轮换（P0 用启动命令行工具创建）

### 工作流 A：Asynq 任务队列基础设施（**推到 P1**）

- [ ] 引入依赖 `github.com/hibiken/asynq`，确认与现有 `go-redis/v8` 共存
- [ ] `service/task_queue.go` —— 封装 Client / Server / Mux，统一队列命名规则
- [ ] `main.go` —— 启动 Asynq Server（不再限制 IsMasterNode）
- [ ] `controller/midjourney.go` / `controller/task.go` / `controller/task_video.go` —— 改造为 `enqueue` 模式
- [ ] 各 task type handler 实现（`task:video:fetch` / `task:mj:fetch` / `task:suno:fetch` / `task:callback`）
- [ ] 下线 `UpdateMidjourneyTaskBulk` / `UpdateTaskBulk`（保留 1 个 release 周期的双跑灰度）
- [ ] 接入 `asynqmon` 管理面板到 `/admin/asynq/`（Root 权限）
- [ ] 队列 × 业务账户的并发配额管理 UI

### 工作流 B-full：账号映射 + 凭据加密 + 完整路由 UI（**推到 P1**，承接 P0 B-min）

- [ ] `model/token.go` —— 增加 `business_account_id` / `business_account_type` 字段及 migration
- [ ] `middleware/auth.go` 的 `TokenAuth` —— 注入 `ContextKeyBusinessAccountId`
- [ ] `model/channel.go` —— 增加 `restricted_business_accounts` / `channel_purpose` 字段
- [ ] `dto/channel_credentials.go`（新建）—— `ChannelCredentials` / `AKSK` / `StorageBinding` 结构定义
- [ ] **v1.1** `dto/channel_settings.go` —— 把 `Credentials` 正式加进 `ChannelOtherSettings` DTO
- [ ] **v1.1** `model/channel.go` —— `MergeOtherSettings()` 方法 read-modify-write 模式，避免字段丢失
- [ ] **v1.1** `pkg/envelope_crypto/`（新建）—— envelope encryption + key_version 多版本支持 + KEK/DEK 二级密钥
- [ ] **v1.1** 启动校验 `GATEWAY_KEK_V1` 环境变量存在、长度正确
- [ ] **v1.1** 密钥轮换 job 文档（写入 `docs/ops/key-rotation.md`）
- [ ] `model/channel_routing_rule.go`（新建）—— GORM 模型 + `fallback_policy` 字段（**默认 strict**）
- [ ] `service/channel_routing.go`（新建）—— 路由规则匹配逻辑 + normalized context 抽取
- [ ] **v1.1** `setting/routing_setting/whitelist.go`（新建）—— body / header 字段白名单
- [ ] **v1.1** `service/channel_routing.go` —— 表达式求值超时 + panic recover + 错误熔断
- [ ] `middleware/distributor.go` —— 接入前置路由判断 + `restricted_business_accounts` 过滤
- [ ] **v1.1** `service/channel_affinity.go` —— `GetPreferredChannelByAffinity` 加 `allowed []int` 参数
- [ ] **v1.1** `model/channel_satisfy.go` —— `CacheGetRandomSatisfiedChannel` 加 `allowed []int` 参数
- [ ] `controller/channel_routing.go`（新建）—— RESTful CRUD + 试算接口
- [ ] `router/api-router.go` —— 注册 `/api/channel-routing/*` 路由（Root/Admin）
- [ ] `web/default/src/features/system-settings/channel-routing/` —— Routing Rules 列表 + 编辑（含 `fallback_policy` UI）+ 试算面板（展示 allowed_channel_ids + 是否触发 fallback）
- [ ] `web/default/src/features/channels/` —— Channel 编辑页：结构化凭据表单（按 channel type 渲染 5 项配置）+ `restricted_business_accounts` 多选 + `channel_purpose` 输入框 + 列表页脱敏显示

### 工作流 C：成本目录（**推到 P2**）

- [ ] `model/provider_cost_catalog.go` —— GORM 模型与 CRUD
- [ ] `model/provider_cost_alert.go`
- [ ] `controller/cost_catalog.go` —— RESTful CRUD + 变价告警接口
- [ ] `router/api-router.go` —— 注册 `/api/cost-catalog/*` 路由（仅 root/admin）
- [ ] `service/cost_catalog_cache.go` —— 内存缓存（按 sku_key 索引，监听 SyncOptions）
- [ ] `service/cost_catalog_sync.go` —— 变价检测 Job（由 Asynq cron 触发）
- [ ] `service/user_notify.go` 扩展告警通道模板
- [ ] `web/default/src/features/system-settings/cost-catalog/` —— 列表 / 编辑 / 告警面板（React 19 + TanStack Table + Base UI）
- [ ] 录入 10-20 个主力 SKU 作为起步数据

### 工作流 D-full：业务系统接入协议完整版（**推到 P1**，承接 P0 D-min）

- [ ] `model/business_account.go`（新建）—— GORM 模型与 CRUD（依赖工作流 E 账本）
- [ ] `model/gateway_admin_token.go`（新建）—— Admin Token + 平滑轮换
- [ ] **v1.1** `gateway_admin_token` 增加 `ip_allowlist` / `scopes` / `daily_recharge_quota_limit` / `daily_account_create_limit` / `single_recharge_max` 字段
- [ ] `model/webhook_subscription.go`（新建）+ `model/webhook_delivery_log.go`
- [ ] **v1.1** `model/webhook_event_outbox.go`（新建）—— outbox 事件表 + 单调递增 event_id + financial / 非 financial 双轨保留期
- [ ] `controller/admin/business_account.go`（新建）—— `POST /admin/api/business-accounts` 等 RESTful 接口
- [ ] **v1.1** 充值幂等键改为 `sha256(external_ref + canonical_body)`，冲突返回 409
- [ ] `controller/admin/webhook.go`（新建）—— Webhook 订阅管理
- [ ] **v1.1** `controller/admin/events.go`（新建）—— `GET /admin/api/events?since_id=` 拉取 + `POST /admin/api/webhook-events/{id}/replay` 重放
- [ ] `service/webhook_dispatch.go`（新建）—— 事件发布（与 ledger 同事务写 outbox）+ HMAC 签名 + Asynq 入队
- [ ] `service/webhook_handler.go`（新建）—— Asynq worker 实际发送 HTTP + 重试 + 死信
- [ ] `middleware/admin_token_auth.go`（新建）—— Admin Token 鉴权中间件 + scope 检查 + IP allowlist + 限流 + 阀门
- [ ] `router/api-router.go` —— 注册 `/admin/api/*` 路由组
- [ ] 在关键业务点埋点发布事件：账户激活、充值、配额预警、日 / 月对账完成、channel 健康度变化、任务完成 / 失败
- [ ] `web/default/src/features/system-settings/business-systems/` —— Admin Token 管理（含 scope 选择 / IP allowlist / 阀门配置）+ Webhook 订阅管理 + outbox 事件查询 UI
- [ ] 文档：`docs/api/admin-api.md` + `docs/api/webhook-events.md`（供业务系统开发参考）

### 工作流 F：billingexpr 版本扩展 + 任务财务状态机（**推到 P1**）

> 解 Codex Important 1 + Important 3。这条工作流强依赖工作流 E（账本），可作为 P0 → P1 的过渡。

- [ ] `pkg/billingexpr/version.go`（新建）—— 版本前缀解析 + Runner 注册表
- [ ] `pkg/billingexpr/runner_v1.go` —— 现有 v1 逻辑迁移到独立 Runner（保持向后兼容）
- [ ] `pkg/billingexpr/runner_v2.go`（新建）—— v2 USD 单次计费
- [ ] `pkg/billingexpr/runner_vp.go`（新建）—— vp 直接积分计费
- [ ] `pkg/billingexpr/types.go` —— BillingSnapshot 扩展 CostRefs / StorageRefs / FxRate / UsageInputs
- [ ] `pkg/billingexpr/builtin_cost.go`（新建）—— `cost(sku_key)` 内置函数
- [ ] `pkg/billingexpr/builtin_storage.go`（新建）—— `storage_cost(file_id)` 内置函数
- [ ] `pkg/billingexpr/builtin_usage.go`（新建）—— `usage(key)` 内置函数
- [ ] 单元测试矩阵：v1 现有所有表达式 byte-for-byte 兼容；v2 / vp 独立测试
- [ ] `model/task.go` —— `TaskFinancialSnapshot` 嵌入 `TaskPrivateData`；新增 `accounting_month` 字段
- [ ] `service/task_billing.go` —— 重写：走账本 reserve；冻结双快照
- [ ] `service/task_settle.go`（新建）—— 终态结算（commit / release / refund）
- [ ] 各 `relay/channel/task/*/adaptor.go` 在 fetch 阶段标准化 usage 写入快照

### 横切

- [ ] `web/default/src/i18n/locales/{zh,en}.json` —— 新增文案
- [ ] 单元测试 + cross-db 集成测试（SQLite / MySQL / PG 三库跑通）
- [ ] **v1.1** 高并发账本测试（goroutine 并发扣减，验证无超卖）
- [ ] **v1.1** 加密密钥管理：`GATEWAY_KEK_V*` 环境变量约定 + 启动校验 + 密钥轮换流程文档（`docs/ops/key-rotation.md`）
- [ ] **v1.1** Redis 持久化与 HA 文档（`docs/ops/redis-config.md`）
- [ ] `CLAUDE.md` Rule 1-7 合规自检
- [ ] 上线 / 灰度方案与回滚预案（包含 Asynq 双跑期 + Routing Rule 灰度开关 + Webhook 灰度发布 + 账本初始化迁移）

---

*文档创建于 2026-05-25。*
*v1.1 修订于 2026-05-25，依据 Codex 独立评审（详见 `docs/reviews/codex-review-v1.md`）。*
*后续每个阶段实施前应回看本文档，必要时更新。*

# Codex 独立评审报告 — 多媒体 AI 网关设计 v1

> **评审时间：** 2026-05-25
> **评审对象：** `docs/multimedia-gateway-design.md`（v1，约 1170 行）
> **评审者：** Codex（独立架构师视角，对照 new-api 源码）
> **总评：** 7/10
> **处理动作：** 已据此评审产出 v1.1 修订（见主设计文档 changelog）

本报告为审计存档，未修改任何文件。修订后的设计文档以 `multimedia-gateway-design.md` 为准。

---

## 评审结论摘要

| 级别 | 数量 | 涉及问题 |
|------|------|---------|
| Critical | 2 | 余额账本缺失 / 路由 fail-open 风险 |
| Important | 6 | billingexpr 版本协议 / 凭据 DTO 兼容 / 任务财务状态机 / OSS 月结粒度 / Webhook 补偿 / Admin Token 风险 / 路由表达式安全 |
| Minor | 1 | Asynq 运维边界 |

**最值得立刻处理的 3 件事：**

1. 先补「余额账本 + 原子预占 + 幂等流水」设计，再谈充值、扣费、退款和 HMAC——这是所有计费正确性的基础。
2. 把企业路由改成默认 fail-closed，并让 affinity/random selection 全部受 `allowed_channel_ids` 约束——一旦上线后出故障，数据隔离和合规事故代价极高。
3. 写清异步任务、Webhook、OSS 月结的状态机和重放/重跑机制，确保任何失败都能审计和恢复——缺了这个，账期对账永远只能靠人工。

---

## Critical 问题

### [Critical] 网关余额模型缺少单一真相源和原子扣减边界

**文档定位：** 三bis.2、九bis.7、十；源码 `model/user.go`、`model/token.go`、`service/pre_consume_quota.go`、`service/billing_session.go`

**问题描述：** 文档新增 `business_account.quota/used_quota`，但 `new-api` 已有 `User.Quota`、`Token.RemainQuota`，且源码里仍是 `int`/`type:int`，不是文档假设的全链路 `int64`。现有预扣是"先读余额再扣减"，用户和 token 分开更新，缺少 `WHERE quota >= ?` 的原子约束和统一账本。

**潜在后果：** 高并发下可能超卖；用户余额、token 余额、business_account 余额三套数字漂移；业务系统拿 HMAC `charged_quota` 也只能证明消息没被改，不能证明网关算得对。

**建议：** 先设计不可变余额流水/预占账本，明确 `available/reserved/used/refunded` 不变量；所有扣减走单一账户账本的原子条件更新；再决定 `User/Token` 是兼容映射还是只做限流标签。同时补全所有 quota 字段到 `BIGINT/int64` 的迁移清单。

---

### [Critical] 企业专属路由存在 fail-open 到共享通道的风险

**文档定位：** 八.3、八.7；源码 `middleware/distributor.go`、`service/channel_affinity.go`、`model/channel_cache.go`

**问题描述：** 文档写"首个命中规则强制候选 channel 集合，未命中回退 new-api 原逻辑"，但现有 `Distributor` 会先查 channel affinity，再走 `CacheGetRandomSatisfiedChannel(group, model, retry)`，源码没有"候选 channel 集合"参数。若规则命中但候选通道全禁用/不可用，当前设计没有明确是 fail-closed、继续下一条规则，还是回退全局池。

**潜在后果：** seedance 这类企业专属凭据场景可能在专属通道不可用时落到共享凭据，造成数据隔离、成本归属和合规事故。

**建议：** 给规则增加 `strict/fallback` 策略。企业账户专属规则默认 fail-closed；只有显式允许才进入下一规则或全局池。路由结果应作为 `allowed_channel_ids` 进入上下文，affinity 和随机选择都必须被这个集合过滤。

---

## Important 问题

### [Important] `billingexpr` v2/vp 扩展低估了现有 v1 语义约束

**文档定位：** 五.1、五.4、五.6；源码 `pkg/billingexpr/compile.go`、`run.go`、`settle.go`、`types.go`、`expr.md`

**问题描述：** 文档计划增加 `v2`、`vp`、`usage()`、`cost()`、`storage_cost()` 和 `CostRefs`，但源码当前只识别 `v1:`，运行环境只注册 v1 函数，表达式结果要求 `float64`，快照里也没有 `CostRefs`。`expr.md` 还明确当前表达式是自包含、无隐藏外部价格表。

**潜在后果：** v2/vp 不是局部加函数，而是计费语义变化。实现不清会导致表达式编译失败、结算重放无法复现、`vp` 直接积分又被 `GroupRatio` 二次缩放。

**建议：** 把 v1/v2/vp 拆成显式版本协议：未知前缀直接拒绝；不同版本有独立 runner、返回类型和 quota conversion；快照必须保存价格目录版本、SKU 引用、汇率、输入 usage，而不是只保存表达式文本。

---

### [Important] 异步任务缺少账户状态变化和价格时点规则

**文档定位：** 九、九bis.5、十；源码 `model/task.go`、`service/task_billing.go`

**问题描述：** 文档定义 suspend/resume/delete，但没有说明任务提交后 business_account 被冻结、删除、API Key 吊销时，inflight 任务继续、取消还是只阻止新任务。源码 `TaskPrivateData` 只保留 TokenId、BillingContext 等，没有 business_account 状态快照、路由规则、价格目录快照。

**潜在后果：** 长视频跨天/跨月完成时，按提交价还是完成价、归属哪个账期、失败重试是否退预扣都会产生争议。删除账户后仍有余额和未结任务也会变成审计黑洞。

**建议：** 补一张任务财务状态机：提交时冻结授权快照和价格快照；suspend 只阻止新提交，delete 必须软删除且要求无 inflight/无未结余额；跨月归属、失败重试、上游退款、人工冲销全部走 ledger 事件。

---

### [Important] OSS/TOS 月结缺少可重跑的明细账和业务账户归属闭环

**文档定位：** 六.1、六.4、六.5、十四

**问题描述：** `oss_object_meta` 里写的是 `tenant_id (= user_id)`，但本设计核心对象是 `business_account_id`。月结按 object_key 匹配 Inventory，若命名不规范或对象迁移，就无法稳定归属。文档风险里说按 `(tenant_id, month)` 覆盖实现幂等，但结算字段还包含 provider、bucket、storage_class，且 `pre_consumed=sum(created_at in last_month)` 无法准确处理跨月持有、删除和生命周期变化。

**潜在后果：** 月结跑挂后难以无副作用重跑；同一企业多个 bucket/provider/class 会互相覆盖；存储成本账单和业务系统消费明细长期对不上。

**建议：** Inventory 对账产出不可变 line item，唯一键至少包含 `business_account_id/month/provider/bucket/object_key/storage_class`；月结采用 job_run + 分阶段 apply；对象归属同时写路径规范、对象 metadata、本地映射表，缺失归属进入人工待处理队列。

---

### [Important] Webhook 只有推送和死信，没有业务方自愈通道

**文档定位：** 九bis.1、九bis.8、九bis.9

**问题描述：** 文档有 webhook 重试、`dead_letter`、7 天 delivery log，但没有事件拉取、按游标补偿、单条重放、批量重放接口。业务侧幂等窗口仅建议 5 分钟，和财务事件长期可追溯要求不匹配。

**潜在后果：** 业务系统短暂故障或验签 bug 后，会永久错过充值、扣费、退款、任务完成事件；月底对不上账时无法判断谁漏了哪条事件。

**建议：** 按 outbox 模式建立 `event_id` 单调递增事件表，保留期按财务审计周期设计；提供 `GET /events?since_id=`、`POST /webhook-deliveries/{id}/replay`、DLQ 后台恢复和事件校验和。

---

### [Important] Admin Token 泄露后的 blast radius 仍然过大

**文档定位：** 九bis.3、九bis.6、九bis.7

**问题描述：** 文档给 Admin Token 设计了 scope、轮换、审计和 100 QPS 限制，但该 token 仍能创建 business_account、充值、冻结/删除账户。充值接口只说 `external_ref` 幂等，没有定义同一 `external_ref` 但 `quota/reason` 不一致时如何处理。

**潜在后果：** token 泄露后攻击者可批量造账户、充值并消费，100 QPS 仍足够造成大额损失；幂等冲突会让业务系统和网关各执一词。

**建议：** 增加 IP allowlist/mTLS、按动作拆 scope、单日充值额度和账户创建额度、异常冻结开关。幂等键唯一约束应包含请求 hash：相同 hash 返回原结果，不同 hash 返回 409 并审计告警。

---

### [Important] 凭据放入 `Channel.OtherSettings` 有兼容和密钥治理风险

**文档定位：** 八.1、十四；源码 `model/channel.go`、`dto/channel_settings.go`

**问题描述：** 现有 `OtherSettings` 反序列化到固定 `ChannelOtherSettings` DTO，`SetOtherSettings` 会重新 marshal 该 DTO；未纳入 DTO 的新 `ChannelCredentials` 字段容易被现有编辑路径丢弃。`GetOtherSettings` 解析失败还会把 settings 置为 `{}` 并保存。单一 `GATEWAY_CREDENTIAL_KEY` 也没有 key version、轮换和 envelope encryption。

**潜在后果：** 管理后台或旧代码保存 channel 时可能静默丢凭据；主密钥泄露会解开所有上游 AK/SK；透明 marshal/unmarshal 解密容易让明文出现在日志、panic、dump。

**建议：** 先把凭据字段正式加入 DTO 并做 merge-preserving 更新；密文保存 `key_version`，优先 envelope encryption/KMS；只在构造上游请求的最小作用域内解密，禁止日志打印和通用 JSON 序列化明文。

---

### [Important] 路由表达式读取请求体在多媒体场景下边界不清

**文档定位：** 八.5；源码 `relay/helper/billing_expr_request.go`、`common/body_storage.go`、`middleware/distributor.go`

**问题描述：** 文档复用 `billingexpr` 的 `param()` 读取请求参数；源码只对 JSON body 构造 `RequestInput`，且 `Bytes()` 会把 body 读回内存，默认请求体上限可到 128MB。视频/图像请求常见 multipart、base64 图片、大 prompt JSON，表达式遍历几十条规则时成本不可忽略。

**潜在后果：** 大请求路由阶段放大内存和 CPU；multipart 场景读不到关键参数；表达式写错路径时可能大量失败并触发错误降级。

**建议：** 路由前先提取小型 normalized context，只允许表达式访问白名单字段；禁止在规则里读取大字段/base64；规则求值设置超时、最大规则数、失败策略和基准测试。

---

## Minor 问题

### [Minor] Asynq 运维边界和现有后台任务迁移清单不够落地

**文档定位：** 九.2、九.4；背景材料 `new-api-technical-analysis.md` 的 `IsMasterNode` 说明

**问题描述：** 文档说 Asynq server 可在所有节点运行并移除 `IsMasterNode`，Redis Sentinel 降低队列丢失风险。但没有明确 Redis AOF/no-eviction 策略、任务唯一键、重复消费幂等、DB task 状态 CAS 作为最终真相，以及哪些旧 master-only job 迁移到 Asynq。

**潜在后果：** Redis 故障或 worker 重启时出现任务丢失/重复；月结、回调、成本探测这类任务重复执行会带来重复扣费或错账。

**建议：** 把 Asynq 定位为执行队列，DB/ledger 才是事实源；每类任务定义唯一键和幂等 apply；列出所有 `IsMasterNode`/定时任务迁移矩阵，并补 Redis 持久化、监控、DLQ 和 asynqmon 接入运维手册。

---

## 处理记录

| 问题 | 处理方式 | 落点 |
|------|---------|------|
| Critical 1 余额账本 | 新增「三ter、账本与原子扣减设计」章节 | v1.1 主文档 |
| Critical 2 路由 fail-open | 8.3 加 `fallback_policy` + `allowed_channel_ids` 强制约束 | v1.1 主文档 |
| Important 1 billingexpr 版本 | 5.4 明确版本协议 + snapshot 内容 | v1.1 主文档 |
| Important 2 凭据 DTO | 8.2 正式入 DTO + envelope encryption + key_version | v1.1 主文档 |
| Important 3 任务状态机 | 新增「九ter、任务财务状态机」章节 | v1.1 主文档 |
| Important 4 OSS 月结 | 6.3 改不可变 line item + job_run | v1.1 主文档 |
| Important 5 Webhook outbox | 9bis 扩展 outbox + 拉取/重放接口 | v1.1 主文档 |
| Important 6 Admin Token | 9bis.6 加 IP allowlist + scope 拆分 + 充值限额 | v1.1 主文档 |
| Important 7 路由表达式安全 | 8.7 normalized context + 字段白名单 + 求值超时 | v1.1 主文档 |
| Minor 1 Asynq 边界 | 9 章明确「队列是执行器、ledger 是真相源」+ 迁移矩阵 | v1.1 主文档 |

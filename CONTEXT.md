# CONTEXT.md — 多媒体 AI 网关项目术语表

> 本文档是项目的领域语言（glossary），不包含实现细节。实现细节请看 `docs/multimedia-gateway-design.md`；架构决策请看 `docs/adr/`。
>
> 当你在代码 / PR / 讨论中使用某个术语时，应当符合本表的定义。新增 / 变更术语时，请直接更新本文档。

---

## 业务实体

### business account（业务账户）
业务系统侧（多媒体创作平台）的企业账户。在网关侧表现为一个或多个 Token 的归属者。网关用 `business_account_id` 字符串唯一标识，**不感知**该账户的营业执照、法人、税号等业务细节。

### Token（API Key）
业务账户的对外调用凭据。一个 business account 可拥有多个 Token（如生产 / 测试 / 子部门各自分配）。Token 携带 `business_account_id`，由 TokenAuth 中间件注入请求 context。

### channel（渠道）
**网关侧**对**一组上游 provider 凭据**的抽象。一个 channel 含完整的上游访问能力（API key、AK/SK、project id、bucket 等）。多个 channel 可属于同一 provider（如火山 seedance 真人 / 仿真人 / 默认 三套独立 channel）。

### ChannelCredentials（结构化凭据）
channel 的凭据子结构，含五大字段族：
- `APIKey`：主调用凭据
- `UpstreamAKSK`：上游云服务 AK/SK（如 ARK、TOS）
- `Storage`：关联存储配置（bucket + path prefix）
- `ProjectID`：上游项目隔离 ID
- `ExtraKV`：未来扩展键值

SecretKey 字段必须 envelope encryption 加密存储，**永远不出现在日志**。

### channel_routing_rule（路由规则）
将「一个对外 Key + 一个 model + 请求参数」映射到「上游具体 channel」的规则。含 `condition_expr` 表达式、`target_channel_ids` 候选集、`fallback_policy` 降级策略。

### provider（上游服务商）
上游 AI 模型服务的统称（火山引擎 / 阿里 / Kling / OpenAI / Anthropic 等）。一个 provider 可对应多种 model（如火山下有 seedance 2.0、doubao 等）。

### task（异步任务）
长耗时上游调用的本地记录（如视频生成、Midjourney 出图）。task 持有 `TaskFinancialSnapshot`（授权快照 + 价格快照），用本地状态机驱动其生命周期。

### relay（中继）
网关核心动作：接收业务请求 → 鉴权 → 预扣费（reserve）→ 转发上游 provider → 按真实用量结算（commit/release）→ 透传响应。MVP 仅同步非流式 `POST /v1/chat/completions`（OpenAI 兼容）；流式 SSE / 异步任务推 P1+。代码见 `internal/relay/`。

### provider adapter（上游协议适配器）
抽象上游 provider 协议差异的接口（`ProviderAdapter`）。MVP 唯一实现 `OpenAICompatAdapter`（覆盖火山 ARK / DeepSeek / 通义 / Kimi 等 OpenAI 兼容端点）。新接非兼容 provider = 加一个 adapter 实现 + 注册到工厂，不改 relay handler 主流程（工厂模式，见 [CLAUDE.md §五](CLAUDE.md)）。

### model catalog（模型字典）
业务可见 `gateway_model_name` → 上游 `(provider_type / base_url / api_key / upstream_model_name / pricing / max_context)` 的映射。业务请求中的 `model` 字段查此字典定位上游。MVP 用 env 单条（`EnvCatalog`，8 个 `GATEWAY_RELAY_*` 字段，传任何 model 都路由到唯一条）；P1+ 升级为 YAML 文件多条或 DB 表（实现同 `Catalog` 接口，开闭原则）。

### business API key（业务 API Key）
业务系统调 relay 的对外身份凭据（`business_account_api_key` 表）。与 admin token 共享 HMAC pepper（`GATEWAY_TOKEN_PEPPER`，同 plaintext 算出同一 hash），但分属不同表、查询路径独立。MVP **无 scope**（一个 key 全权限，与 OpenAI / DeepSeek 风格一致）、**无 IP allowlist**、仅 RPM 限速。由 `admin-cli business-key create/list/revoke` 管理。注：上文「Token（API Key）」是历史泛指的业务凭据概念，business API key 是 F-min 的具体落地实现。

---

## 账本（Ledger）

### ledger
`business_account_ledger` 表，**不可变流水**。所有扣减、退款、对账的**唯一真相源**。任何与 ledger 不一致的字段都是 bug。

### balance（账户余额投影）
`business_account_balance` 表，ledger 的**严格投影**（不是缓存）。每次 ledger 写入同事务更新 balance；后台 reconcile job 每 5 分钟校验，drift 触发**账户冻结**（不只是告警）。

### available / reserved / used（三态余额）
账户余额的三态分离：
- `available`：可用余额，新请求 reserve 时扣减
- `reserved`：预占余额，inflight 任务持有
- `used_total`：累计已结算

### 账本不变量
任何时刻：`available + reserved + used_total = recharge_total`。
`refund_total` 仅作审计字段，**不进入不变量**（v1.2.1 数学校准后的最终形式）。

### ledger entry type（流水类型）
固定枚举：`recharge / reserve / commit / release / refund / cashout / recharge_reversal / adjust / expire`。
- `refund`（**返还额度**，credit refund）：把已扣的 quota 退回 available，最常见
- `cashout`（**退款出账**，cash refund）：彻底把钱退离系统，账户余额减少；与 `recharge_reversal` 配合用于订阅退订等罕见场景

### drift
balance 投影与按 ledger 重算结果不一致的状态。触发账户立即冻结，运营介入排查后从 ledger 重建投影。

---

## 计费

### billingexpr（计费表达式）
基于 `expr-lang/expr` 的动态计费 DSL。表达式从请求参数 / Usage / 价格目录 / 时间等输入计算 quota 输出。

### v1 / v2 / vp（表达式版本）
- `v1:`：LLM token 计费（USD per 1M tokens）
- `v2:`：多媒体 USD 单次计费（USD per request）
- `vp:`：直接积分计费（output 直接是 quota，无需 USD 换算；仅乘 groupRatio）

### BillingSnapshot（计费快照）
预扣时冻结的结算上下文：表达式文本 + 哈希 + group ratio + cost catalog 引用 + 汇率 + usage 输入 + 时间戳。结算时**永远以快照为准**，不查"最新值"。

### Provider Cost Catalog（上游成本目录）
上游 SKU 真实定价的事实表（含币种、生效区间、单价、catalog 版本）。`billingexpr` 通过 `cost("sku_key")` 函数引用，快照里固化引用的 catalog 版本号。

### provisional / adjustment / finalized（月结三态）
- `provisional`：账期内未终结任务的暂估账单（首次月结发出）
- `adjustment`：跨月任务终结后发出的调整账单
- `finalized`：账期最终关账，此后该账期不可变

---

## 路由与渠道

### isolation_required（企业隔离硬开关）
`business_account.isolation_required` 字段。启用后该业务账户**禁止任何形式的跨企业降级**（含 `global_pool` / `legacy_distributor` / 跨企业 `next_rule`）。

### fallback_policy（路由降级策略）
`channel_routing_rule.fallback_policy` 字段，枚举：
- `strict`：候选 channel 全不可用直接 503，**不降级**（默认）
- `next_rule`：求值下一条规则
- `global_pool`：降级到全局规则池
- `legacy_distributor`：降级到 new-api 风格的 model + group 选择

被 `isolation_required` 强制收窄：启用时仅 `strict` 和同企业 `next_rule` 可用。

### break-glass（紧急逃生门）
当 `isolation_required` 企业的专属 channel 全部故障时，允许临时绕过隔离的应急机制。需 Root 双人审批 + 时长上限 24 小时 + 全程审计 + Webhook 通知业务系统。

### allowed_channel_ids
路由规则求值结果，限定 distributor 下游可选 channel 范围。**全链路强制约束**（含 affinity 与 random selection），不可绕过。

### normalized routing context（路由上下文）
路由表达式可访问的字段白名单（约 30 个常用枚举 / 标量）。**禁止访问大字段**（prompt / image / messages）以避免性能问题。

---

## 任务状态机

### 任务状态枚举
固定 8 + 1 个状态（v1.2.4 → v1.2.2 演化后）：
```
SUBMITTED → UPSTREAM_SUBMITTING → UPSTREAM_SUBMITTED
                                  ↓
   COMPLETED / FAILED / CANCELLED / EXPIRED
                                  ↓
                              SETTLING → SETTLED
```

### UPSTREAM_SUBMITTING（中间态防孤儿）
Worker 已 CAS 抢占本地提交权但尚未拿到 upstream_task_id 的瞬态。
- `submit_locked_until` 字段记 lease 截止时间
- Cron job 每分钟检查超时（最多 3 次回退到 SUBMITTED，超出转 FAILED）

### TaskFinancialSnapshot（任务财务快照）
任务提交时冻结的"授权 + 价格"双快照：
- AuthSnapshot：business_account / token / 路由规则 / channel + 凭据 key_version
- PricingSnapshot：billingexpr 表达式 + cost catalog 版本 + 汇率 + usage 输入

跨月任务按 `submitted_at` 归属账期，永不切分。

---

## 凭据加密（Envelope Encryption）

### KEK（Key Encryption Key）
主密钥，加密 DEK 用。存储于环境变量（P0）或 KMS（P1+）。版本化（KEK v1 / v2 / ...），平滑轮换。

### DEK（Data Encryption Key）
数据密钥，每条 AKSK 独立一份。用 DEK 加密真实 SecretKey 内容；DEK 自己用 KEK 加密后存 DB（密文 = `key_version + enc_dek + IV + ciphertext + tag`）。

### key_version
密文里携带的 KEK 版本号，支持「新增 KEK v(N+1) → 后台 job 重加密 DEK → 旧 KEK 保留期 = max(任务最长执行 30 天, DLQ 保留, 财务审计窗口 1 年)」的平滑轮换。

---

## Webhook 与 Outbox

### outbox（事件出箱）
`webhook_event_outbox` 表。事件按 `event_id` 单调递增写入；**与 ledger 同事务提交**，保证"只要 ledger 有这笔流水，业务系统最终一定能拉到事件"。

**强制部署在主库**（与 ledger 同库），不受 `LOG_SQL_DSN` 分库影响。

### claim/lease（claim/lease 模式）
多节点扫描 outbox 时的并发控制：
- PG / MySQL 8.0+ 用 `SELECT FOR UPDATE SKIP LOCKED`
- 加 `delivery_status='delivering'`、`locked_by`、`locked_until` 字段
- 超时 `locked_until < NOW()` 可被其他 worker 抢占

### event_id（事件游标）
单调递增的事件 ID，业务系统侧用 `GET /events?since_id=` 拉取做补偿；与 `delivery_idempotency_key` 配合幂等。

### delivery_idempotency_key
业务侧 webhook 处理时的去重键，财务事件按此键长期去重（≥ 1 年），非财务事件 5 分钟窗口去重。

---

## Admin Token

### scope（按动作授权）
Admin Token 的细粒度权限范围，如：
- `business_account:read / create / suspend / delete / recharge / refund`
- `token:read / write`
- `webhook:manage`
- `event:read`

### IP allowlist
Token 级源 IP 白名单，CIDR 格式。未在 allowlist 内的请求直接 401，不消耗限流配额。

### 阀门（quota gates / throttle）
Token 级硬性额度限制（D-min Unit 3 落地）：
- `single_recharge_max`：单笔充值上限
- `daily_recharge_quota_limit`：当日累计充值上限（UTC day）
- `single_refund_max`：单笔退款上限（D-min document-review 添加，防 leaked refund-scope token 一次清空 used_total）
- `daily_refund_quota_limit`：当日累计退款上限（UTC day）
- `daily_account_create_limit`：当日创建账户上限
- `requests_per_minute`：RPM 上限（进程内 ring buffer；P0 单实例计数，P1 接 Redis）
- `circuit_breaker_enabled`：自动熔断（1 小时内 100 次 4xx/5xx 触发跳闸 1 小时）

**两步式语义（决策 D2 + D11）**：handler 流程分 `Check*` 预检 + LedgerService 调用 + `Record*` 累加；
LedgerService 返 `WriteOutcomeFreshlyWritten` 时才 Record（IdempotentReplay 不累加，避免业务系统重试膨胀配额）。

### 熔断器（circuit breaker）
Token 级跳闸状态机：1 小时滚动窗口内 error_count ≥ 100 → 写 `breaker_tripped_until = NOW() + 1h`，
中间件 `CheckCircuitBreaker` 命中后返 ErrCircuitOpen → 429。到期自动闸合（不走半开）；运维手工解锁路径见
`admintoken.ResetCircuitBreaker`。

### minor unit（最小货币单位）
所有 `amount` / `*_max` / `*_limit` 字段的单位：**1 元 = 100 分**（CNY）。
当前 P0 仅支持 CNY；多货币是 Phase 3+ 决策，引入时 schema 必须增 currency 字段，禁止复用同 `*_limit` 字段意义漂移。

### canonical body sha256
充值幂等键的真相值：`sha256(canonicalize(RechargeBody{account_id, amount, external_ref}))`。
service 层在 idempotency_key 命中既存 entry 时比对 body sha256；一致 → 返原 entry（`IdempotentReplay`）；
不一致 → `ErrIdempotencyConflict` 409 + Tier1 critical audit。

### Token Pepper（HMAC 主密钥）
环境变量 `GATEWAY_TOKEN_PEPPER`（hex 或 base64 编码 ≥ 32 字节随机串）。
token_hash 算法 = `HMAC-SHA-256(pepper, plaintext)` hex。决策依据见 D-min plan §决策 D1：
DB 全量泄露但 env 未泄时，攻击者拿到 hash 也无法离线穷举（需先突破 pepper）。
Pepper 丢失会让所有 token 立刻失效；与 KEK 同级关键密钥，必须列入 SOP 备份目录。

### Bearer Token
Admin API 鉴权方式：`Authorization: Bearer <plaintext>`。仅支持 header；不支持 query string / cookie
（避免明文 token 出现在 URL / referer / 日志）。失败路径不消耗限流配额（IP allowlist / token 不存在 → 401，throttle 不计）。

### audit Tier
Admin API 审计日志分层（决策 D3）：
- **Tier1**：refund / token lifecycle / idempotency_conflict / auth_failed → 同步 O_APPEND+O_SYNC 写本地文件，写失败让 `/readyz` 关闸
- **Tier2**：create / recharge / balance read 等 → 异步 slog stderr，best-effort

### whoami
任何已鉴权 token 都可调的自检端点 `GET /admin/v1/whoami`，返回 token 元数据 + 阀门快照 +
今日累计用量 + 熔断状态。**不**返回 token_hash / ip_allowlist 具体 CIDR 列表（防泄露后嗅探精确网段）。

---

## OSS / 存储

### storage_billing_object_item（对象级明细）
不可变事实表，Inventory 解析后每个 object 一行。**不直接扣费**，仅供审计 / 查询 / 归属修正。

### storage_billing_line_item（聚合记账层）
从 `object_item` 派生的聚合视图（按 business_account × provider × bucket × storage_class）。**只有 line_item 与 adjustment 写 ledger**。

### storage_unassigned_object（归属缺失）
三重归属（本地映射 → 对象 metadata → 路径前缀）全部失败的对象，待运营人工处理。

---

## reimplement 纪律

### reimplement-only
本项目的法律 / 协作姿态（详见 ADR-0001）：不基于 new-api 二次开发，代码 100% 自写，仅参考其架构思路。

### idea / expression dichotomy
版权法基础原则：保护具体表达（代码、注释、字面文字），不保护抽象思想（架构模式、算法思路、协议规格）。本项目据此参考 new-api 架构思路是合法的。

### third-party reference（只读参考）
`third-party/new-api/` 目录保留作只读参考，**永不 commit**（`.gitignore` 已生效）、**永不 import**（`go.mod` 不引用）、**永不照搬代码片段**（PR review 把关）。

---

## 系统外部依赖

### Asynq + Redis
Go 任务队列（v1.2.4 选定，沿用至 v1.3）。承担：
- 异步任务调度（task:submit / fetch / settle）
- Webhook outbox 投递 worker
- cron jobs（月结、变价检测、reconcile）

### PostgreSQL（唯一数据库）
v1.3 决定（ADR-0002）。开发 / 测试 / 生产全部 PG（≥ 15）。

### sqlc（SQL 即真相）
数据访问层（ADR-0003）。所有 query 写在 `sql/queries/*.sql`，sqlc 生成类型安全 Go 代码。

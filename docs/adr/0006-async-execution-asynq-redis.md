# ADR-0006: 异步执行基座与配套基础设施（Asynq/Redis + 并发上限 + TOS 接入 + KEK 轮换 + 上游幂等缺失应对）

- **状态：** Accepted
- **日期：** 2026-05-29
- **决策人：** sunxin
- **相关文档：** `docs/multimedia-gateway-design.md` §9 / §9.4 / §9.5 / §9ter；`docs/plans/2026-05-28-001-feat-async-video-relay-mvp-plan.md`（Unit 1）；ADR-0001（reimplement-only）、ADR-0002（PG 单选）、ADR-0003（sqlc）
- **适用范围：** Phase 2「异步视频中继 MVP」。本 ADR 是计划 Unit 1 解锁 `go.mod` 变更的**闸门**——通过前不引入任何异步/存储依赖。

## 背景

计划要把首个异步多媒体能力（火山 Seedance 2.0 `text_to_video`）端到端打通：`提交 → 鉴权/entitlement/能力校验 → reserve 预扣 → 提交上游 → 回调优先/轮询兜底 → 按真实 token 结算 → TOS 落盘签名 URL → 业务轮询取结果`。

在动 `go.mod` 之前，有 **5 项跨单元的基础设施决策**必须先定稿（计划 Unit 1 把它们列为 `go.mod` 闸门），避免推到实现期再临时做架构选型：

1. 异步执行基座选型（新依赖）
2. R15 并发硬上限由谁承载
3. TOS 接入方式（新依赖）
4. 凭据 KEK 轮换语义
5. 上游缺幂等键时的双提交防护与崩溃恢复策略

本 ADR 一次性定稿这 5 项。调研基础（**遵循 ADR-0001：不读 new-api，只看官方文档 + 自有生产参考实现**）：

- **火山方舟 Seedance API 官方文档**逐字核对（提交体字段 / status 枚举 / 任务列表过滤参数 / usage 字段）
- **火山 TOS Go SDK 官方文档 + CHANGELOG**
- **生产参考实现** storyboard-assistant（`backend/app/providers/seedance.py`、`backend/app/services/tos_storage.py`）交叉验证
- **本项目现有 schema 与 config**：`channel.credentials_encrypted bytea` + `key_version int DEFAULT 1` 已就绪；`GATEWAY_KEK_V1` 已 fail-fast 必填校验、装载为 `cfg.GatewayKEKV1`，但**尚无任何消费方**（relay 上游凭据仍是 env 明文 MVP），`internal/crypto` 包尚不存在。

## 决策

### 决策 1：异步执行基座 = Asynq + Redis（**仅作执行器**）

提交 / 结算 / 对账 / 崩溃恢复等异步动作走 **Asynq**（后端 **Redis**），复用其原生重试、指数退避、定时调度、可重入 handler。理由：与设计文档 §9 原计划一致；Redis 容器（`api-gateway-redis`）已在运行；自研「进程内 goroutine + DB 轮询」要重造重试/退避/调度且跨副本协调复杂。

**硬约束：Asynq 不承载 R15 并发硬上限。** Asynq 的 concurrency 是 **server 进程级**、跨副本不共享；队列级配置只有 priority/weight，**不是硬上限**。并发上限的权威承载见决策 2。Asynq 队列仅用于执行隔离与优先级。

- Redis 连接复用 `cfg.RedisAddr`；启动 ping **fail-fast**（与 pgxpool 同风格）。
- 队列命名、优雅停机封装在 `internal/asyncq/`。

### 决策 2：R15 并发硬上限 = **DB 原子 claim（权威）**

并发上限用 **DB 专用并发计数行**（账户 × 模型粒度）原子占位，**不靠队列 concurrency**。DB 是单一真相源（设计文档 §9.5），无跨副本漂移、无 check-then-act TOCTOU。

**为什么不是队列承载？** 队列（Asynq）的 concurrency 是 **worker 进程池大小**——「此刻有几个 job 正在被执行」——与我们要的并发上限不是同一个量：

1. **进程级、不跨副本**：Asynq `Concurrency` 是单进程 goroutine 数；跑 N 副本总并发 = `N × Concurrency`，无全局上限。队列级配置只有 priority/weight（挑选频率），**不是 in-flight 硬上限**。
2. **粒度对不上**：R15 是「账户 A 在模型 M 上最多同时 C 个任务在上游」——按 (账户×模型) **动态分桶的业务规则**；用队列实现要为每个 (账户×模型) 开队列（成千上万条），且队列本身仍无法设硬上限。
3. **最致命——并发槽生命周期 ≠ job 执行时间**：一个视频任务从提交到上游终态历时数分钟至数十分钟，其间大部分时间在等回调/等轮询，**占着上游并发槽却占用 0 个 worker goroutine**，且跨越 submit→poll→settle 多个离散 job 与大段空闲等待。worker 池只统计「正在 CPU 上跑的 job」，量错了对象。**并发槽是被「任务在上游存活的整个时长」占用的，必须记在持久状态（DB）里，不能记在瞬时 worker 池里。**

Asynq 的 concurrency 仍保留，但只管**执行层吞吐/资源**（worker 池一次跑几个、哪个队列优先，防打爆 Redis/上游），与业务硬上限**正交**：DB claim 裁决「能不能进上游」，Asynq 裁决「准入后多快被执行」。

**机制：**

- **占位（提交前，与 task 落库同事务）**：实现用条件 UPSERT（覆盖首次 lazy-init），等价于条件 `UPDATE...RETURNING`：
  ```sql
  INSERT INTO account_model_concurrency AS amc (business_account_id, model, inflight, updated_at)
  SELECT $1, $2, 1, NOW()
  WHERE $3::int >= 1                  -- cap=0 守卫（ce-review #1）：让首次插入路径也受 cap 约束
  ON CONFLICT (business_account_id, model) DO UPDATE
      SET inflight = amc.inflight + 1, updated_at = NOW()
      WHERE amc.inflight < $3
  RETURNING inflight;
  ```
  `RETURNING` 拿到行 → 占位成功，继续落 task + 入队；无行 → 占不到 → **429**。单条原子 UPSERT（PG 行锁串行化），规避幻读插入式 TOCTOU。**`WHERE $3::int >= 1` 守卫不可省**：`ON CONFLICT ... WHERE` 仅作用于 DO UPDATE 分支，省略守卫则「行不存在 + cap=0（禁用模型）」会经 INSERT 直接写 inflight=1 绕过上限。
- **释放（CAS 进入上游终态时）**：`UPDATE ... SET inflight = inflight - 1 WHERE (...) AND inflight > 0`。
- **claim = 上游并发槽**：只数 `SUBMITTED / UPSTREAM_SUBMITTING / UPSTREAM_SUBMITTED` 三态；进上游终态（`COMPLETED / FAILED / CANCELLED / EXPIRED`）立即释放；`settle_failed` **不持 claim**。
- **同事务杜绝泄露**：占位增量与 task 落库同事务（要么都成要么都不），杜绝「占了位但 task 没落库」。
- **防永久占槽兜底**：万一释放路径漏减，`expire` sweep（`execution_expires_after` 到点）+ reconciler 最终把卡住任务推到终态从而释放（故须并发 + 崩溃恢复测试，见「后果」）。
- **资金敞口**：未结算 reserve（上游终态 → SETTLED 窗口、及 settle_failed）不受 claim 约束，其敞口由 reserve 时的 available 余额单独约束。

### 决策 3：TOS 接入 = **官方 Go SDK** `ve-tos-golang-sdk/v2`

结果转存 + 限时签名 URL 采用火山官方 Go SDK：`github.com/volcengine/ve-tos-golang-sdk/v2/tos`（Apache-2.0、维护活跃、Go 1.13+，满足我方 1.25/1.26）。

- 上传：`PutObjectV2`（结果对象，建议 `ForbidOverwrite` 防覆盖）。
- 限时下载：`PreSignedURL(&PreSignedURLInput{HTTPMethod: GET, Bucket, Key, Expires: <秒>})` → `SignedUrl`；**TTL 最小化为业务取回所需**（非整个轮询窗口）。
- 凭据来自 channel（Unit 3 的 TOS AK/SK + bucket + endpoint + region），即用即弃；签名 URL **不入 audit/日志/span**。
- 选 SDK 而非手动签名：符合总纲原则 6「稳定 > 优雅」；生产参考实现亦用官方 SDK（Python `tos.TosClientV2` + `pre_signed_url`）；后续「输入媒体代理上传」（分片/重试/校验）可复用 SDK。
- **去依赖 fallback 已验证可行备查**：TOS 用 `TOS4-HMAC-SHA256`（与 AWS Signature V4 同构、前缀 `X-Tos-`），手动预签名 GET 约 80–150 行 stdlib、零第三方依赖。若未来需削减依赖树，可切回此方案（需补签名向量对拍测试）。

### 决策 4：凭据 KEK 轮换语义（stdlib AES-GCM，KMS 推后）

凭据加密用标准库 **AES-256-GCM**（P0 单层，KEK 直接作数据密钥；P1 再接 KEK/DEK 分层），**无新依赖**。轮换语义：

- **多版本 KEK 环境变量**：`GATEWAY_KEK_V1` / `GATEWAY_KEK_V2` / ...（base64/hex 编码原始字节，调用方解码）。装载入按版本号索引的 KEK map。
- **加密恒用当前最高版本** KEK，并把版本号写入记录的 `key_version` 列。
- **解密按记录的 `key_version` 选对应 KEK**；KEK 缺失或 GCM 认证失败 → **fail-closed**（返 error，绝不返回明文，绝不降级）。
- **轮换流程**：上线新 `GATEWAY_KEK_V{n+1}`（旧版本保留用于解密）→ 新写入自动用新版本 → 旧密文经 admin-cli **重加密命令**批量「解旧写新」收敛到新版本。
- **安全红线**：明文绝不入日志；密钥类字段（ARK Secret Key / TOS Secret Key）一律显示固定占位（如「已设置」），**绝不回显任何明文片段**；末 N 位掩码仅用于非机密标识符（bucket / project_id / APIKey 前缀）。明文仅 `GetCredentialsForUpstream` 返回，即用即弃。
- KMS 托管推后（P1+），但**轮换语义现在定死**，使 P0 实现不会锁死未来迁移。

### 决策 5：上游缺幂等 → 我方双提交防护 + recover **fail-closed**

官方文档确认：Seedance 提交 API **无任何幂等键**（`safety_identifier` 是「终端用户标识」用于违规检测，**不是去重键**，重复提交各自新建并计费）；任务列表/查询**无法按我方自定义标识反查**（`filter` 仅支持 `status / task_ids（上游 id）/ model / service_tier`）。生产参考实现亦未使用幂等键、未给任务打我方标识，与官方一致。

因此：

- **双提交防护我方自做**：我方 `task_id` 唯一约束 + 提交前 DB claim 占位（决策 2）拦住「我方入口重复提交」。
- **维护「我方 task_id ↔ 上游 task_id」映射**：即 task 行的 `upstream_task_id` 列；**不依赖上游列表反查**。
- **崩溃恢复 fail-closed**：崩溃若发生在「上游 Submit 成功 → 我方持久化 `upstream_task_id`」之间，因上游既无幂等键、又无法按我方 key 反查，该任务**无法安全判定是否已生成**。故 `UPSTREAM_SUBMITTING` lease 过期**不自动重投**；超阈值直接 CAS → `FAILED` + release + 告警人工介入（**宁可漏生成，不可双扣**，符合失败优先原则）。
- **结算口径**：`usage.completion_tokens`（视频模型 `total_tokens == completion_tokens`，输入 token 计 0）；并须处理 **Seedance 2.0 的最低 token 计费下限**（结算与 reserve 上界都要覆盖该下限）。此为 Unit 7 实现输入。

> 注：决策 5 把计划里的「fail-closed」从「上游无幂等时的兜底分支」**升级为确定路径**——「反查重投」分支经官方文档确认不可行，已彻底排除。

## 新增依赖（本 ADR 解锁 `go.mod`）

| module | 版本（落地前 `go get` 核对） | License | 用途 | go.mod 位置 |
|---|---|---|---|---|
| `github.com/hibiken/asynq` | v0.26.0（Unit 1 落地） | MIT | 异步任务执行器（提交/结算/对账/恢复） | **direct** |
| `github.com/redis/go-redis/v9` | v9.20.0 | BSD-2-Clause | Asynq 的 Redis 后端 | indirect（asynq 传递） |
| `github.com/robfig/cron/v3` | v3.0.1 | MIT | asynq scheduler 传递依赖 | indirect |
| `github.com/spf13/cast` | v1.10.0 | MIT | asynq 传递依赖 | indirect |
| `golang.org/x/time` | v0.14.0 | BSD-3-Clause | asynq 限速器传递依赖 | indirect |
| `github.com/volcengine/ve-tos-golang-sdk/v2` | ~v2.9.x（**Unit 9 才引**） | Apache-2.0 | TOS 结果转存 + 预签名 GET URL | 待引（direct） |

> **依赖数（ce-review #project-standards）**：Units 1-3 实际只引入 asynq（direct）+ 4 个传递依赖（go-redis/robfig-cron/spf13-cast/x-time，均 `// indirect`，license MIT/BSD 宽松无合规风险）。另有 bsm/ginkgo、bsm/gomega 等仅测试期传递依赖（go-redis 测试用），不进生产二进制。TOS SDK 列在表中但 **Unit 9 才进 go.mod**。
> **启动 ping fail-fast 实现说明（Unit 1）**：未单独引 go-redis 客户端做 ping，改用 asynq 自带的 `Server.PingContext()`（同一 RedisConnOpt + 超时，等价探活），少一个直接依赖。故 go-redis 为 asynq 的**传递依赖（indirect）**。
> KEK 轮换（决策 4）**不引入依赖**（stdlib `crypto/aes` + `crypto/cipher`）。
> 落地前执行 `go get` 并以 `go doc` 核对 `PreSignedURLInput` 等字段名（文档为二手印证）。

## 后果

### 变得更容易

- ✅ 异步链路的重试/退避/调度/可重入由 Asynq 兜底，不重造轮子。
- ✅ 并发上限由 DB 单一真相源裁决，跨副本一致、无 TOCTOU。
- ✅ TOS 上传/预签名一行调用；后续输入媒体上传可复用同一 SDK。
- ✅ 凭据加密轮换语义现在定死，P0 用 stdlib 零依赖，P1 迁 KMS 不被锁死。
- ✅ 双提交/崩溃恢复策略有官方文档背书，金钱安全（不双扣）路径确定。

### 变得更难 / 代价

- ⚠️ 引入 1 个直接依赖（asynq）+ 4 个传递依赖（go-redis/robfig-cron/spf13-cast/x-time，见上表）+ 运行期强依赖 Redis 可用性（启动 fail-fast，运维须保障 Redis）。
- ⚠️ 并发计数行的 `inflight` 必须与上游终态 CAS 严格配对增减，**少减即永久占槽**——须并发 + 崩溃恢复测试覆盖（CLAUDE.md：涉状态机/并发必测）。
- ⚠️ fail-closed 意味着极端崩溃下「可能已生成但被判 FAILED」的任务需人工介入；这是为「绝不双扣」付出的代价。
- ⚠️ TOS 官方 SDK 的传递依赖树需在 PR 评审时过一遍（避免引入重型/不合规传递依赖）。

### 备选方案为什么被拒绝

| 备选 | 拒绝理由 |
|---|---|
| **进程内 goroutine + DB 轮询**（替代 Asynq） | 要自研重试/退避/调度/跨副本协调；设计文档 §9 已选 Asynq；Redis 已在运行 |
| **Asynq 队列 concurrency 作并发上限** | 进程级、跨副本不共享、非硬上限——无法满足 R15 账户×模型硬上限 |
| **`SELECT COUNT(*) FOR UPDATE` 再判**（替代条件 UPDATE） | 幻读插入式 TOCTOU；条件 `UPDATE ... RETURNING` 原子且无此问题 |
| **TOS 手动 V4 签名**（替代官方 SDK） | 可行且零依赖，但 canonical 拼接易踩坑、上传分片/重试要自写；首选稳定，保留为去依赖 fallback |
| **按 reserve 上界全额 commit**（缺 usage 时） | 系统性多收；改为 `settle_failed` + 对账队列（见计划） |
| **recover 自动重投上游**（崩溃后） | 上游无幂等键 + 无法按我方 key 反查 → 会双扣；改 fail-closed |
| **凭据明文 env / 不轮换** | 商业平台不可接受；schema 已预留 `key_version`，本就为轮换设计 |

## 实施清单（落在 Unit 1 + 下游单元）

- [ ] `go.mod` 引入 `hibiken/asynq` + `redis/go-redis/v9` + `ve-tos-golang-sdk/v2`，`go mod tidy`
- [ ] `internal/asyncq/`：Asynq client + server 封装、队列命名、优雅停机
- [ ] `internal/config/config.go`：`REDIS_ADDR` 读取 + production fail-fast；新增 Asynq 队列/并发配置键
- [ ] `main.go`：装配 Asynq server goroutine + Redis ping fail-fast + cleanup defer
- [ ] `internal/crypto/envelope.go`：AES-GCM 加解密、多版本 KEK map、`key_version` 标记、解密 fail-closed（Unit 3）
- [ ] admin-cli 凭据重加密命令（解旧 KEK 写最高版本；带 `--dry-run`）（Unit 3 / Unit 11）
- [ ] `account_model_concurrency` 并发计数行 schema + 条件 `UPDATE ... RETURNING` 占位 / 上游终态释放（Unit 2 / Unit 6 / Unit 8）
- [ ] task 表 `upstream_task_id` 映射列；submit worker 先持久化提交意图再调上游；recover lease 过期 fail-closed（Unit 2 / Unit 6）
- [ ] `internal/storage/tos.go`：`PutObjectV2` + `PreSignedURL(GET, Expires)`；签名 URL 不入日志（Unit 9）

## 验证

- 启动时 Redis 不可达 → fail-fast 退出（与 PG 同风格）。
- 并发提交超 cap → 部分 429；`inflight` 计数与上游终态 CAS 配对，无永久占槽（并发 + 崩溃恢复测试）。
- 凭据：错误 KEK / 密文被篡改 → 解密返 error 不返明文；密钥类字段视图永不含明文片段。
- TOS：mock/真实 endpoint 上传成功 → 返回限时 GET 签名 URL；日志/audit 不含签名 URL。
- 崩溃注入：Submit 成功后、存 `upstream_task_id` 前崩溃 → recover 不自动重投 → 超阈值 FAILED + release + 告警（验证不双扣）。

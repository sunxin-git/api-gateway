# ADR-0007: 运维配置 DB 化 + 在线生效（catalog / pricing / 并发 cap）

- **状态：** Accepted
- **日期：** 2026-05-31
- **决策人：** sunxin
- **相关文档：** `docs/brainstorms/2026-05-31-gateway-admin-console-config-requirements.md`；`docs/plans/2026-05-31-001-feat-admin-console-config-plan.md`；ADR-0006（异步基座 + DB 原子 claim）、ADR-0004（P0 含 React UI）、ADR-0003（sqlc）
- **取代：** `docs/plans/2026-05-28-001-feat-async-video-relay-mvp-plan.md` 原 Scope Boundaries 的「catalog DB 化 / 多 model 推后」与 Unit 11 的 admin-cli 配置方案

## 背景

异步视频中继 MVP（计划 `2026-05-28-001`）把视频 catalog / pricing / 账户×模型并发上限实现为 **env 配置**（`GATEWAY_VIDEO_RELAY_*` 一组键，装配在 `main.go:buildVideoTaskService`），并把「catalog DB 化 / 多 model」与「运维配置面（Unit 11）」推后，Unit 11 原拟用 admin-cli 子命令补。

但用户明确：**网关的所有功能配置都通过前端管理后台（UI）完成，不做 admin-cli 配置工具**（见 brainstorm Key Decisions）。UI 在线配置的前提是配置可在线写、运行中即时生效——env + 重启无法满足。因此 catalog / pricing / 并发 cap 必须 **DB 化**。

这推翻了原计划「catalog DB 化推后」的范围边界，按 CLAUDE.md §八（修改约束须先开 ADR）记录于此。

现有可复用基础（**遵循 ADR-0001：不读 new-api**）：

- `internal/relay/video/` 的 `VideoCatalog` 是**接口**（`Lookup`/`DefaultEntry`/`All`），`*ConcurrencyLimits` 与 `VideoModelEntry`/`Pricing`/`ResolutionTier`/`Capability` 是**值类型**——配置来源可从 env 换成 DB 而**不动** `task.Service`/handler（依赖接口）。
- `account_model_concurrency` 计数行表（0005）已就绪；ADR-0006 的 `ClaimConcurrencySlot` 条件 UPSERT 以 `@cap` 为查询参数（cap 当前来自 Go 侧 env 解析）。
- `channel` / `business_account_model_entitlement` 表已 DB 化（凭据 write-only / entitlement 已具 sqlc）。
- 结算读 `financial_snapshot` 冻结价（`internal/task/snapshot.go`），**不读 catalog**。

## 决策

### 决策 1：catalog / pricing / 并发 cap 全部 DB 化（推翻 catalog 推后）

新增三张配置表（详见计划 Unit 5/6）：

- `gateway_model_catalog`：gateway model → 上游 provider/model + 绑定 channel + 能力档（duration/fps/ratio/resolution）+ 模型级计费参数（safety_factor_bp / min_token_floor / result_url_ttl_seconds）+ enabled。**多模型多条**（不再单条）。
- `model_resolution_pricing`：每 (model, resolution, has_input_video) → {W×H, CNY/百万 token 单价, 倍率 bp, 计费单位 unit}。`unit` 字段保留设计 §5.1 多单位（token / video_second）。
- `account_model_concurrency_override`：per-(account, model) 并发 cap 覆写（与计数行表 `account_model_concurrency` 分离，覆写可先于任何 inflight 存在）。

`channel`（凭据 write-only）与 `business_account_model_entitlement`（授权）已 DB 化，本 ADR 不新增表，仅补 Admin API 配置面。

### 决策 2：运行时读取策略（在线生效，分三类）

「即时生效」按配置类型采用不同机制，权衡一致性与提交热路径开销：

| 配置 | 读取机制 | 生效时延 |
|---|---|---|
| **并发 cap** | 折进 `ClaimConcurrencySlot` 的 `COALESCE((SELECT cap FROM override WHERE ...), @default)` | **即时**（每次占位查 DB，零额外往返） |
| **catalog / pricing / capability + 模型级 safety/floor/ttl** | 提交热路径用**短 TTL 内存缓存**包裹 DB 读（TTL 可配，默认约 15s），过期惰性刷新 | TTL 窗口内（秒级） |
| **entitlement check** | 每请求 DB 查（已是现状，单次索引查询） | **即时** |

pg LISTEN/NOTIFY 即时失效作未来优化，本期不做。

### 决策 3：fail-closed —— DB 无配置拒绝提交

切到 DB 后，**DB 无对应 model catalog / pricing 档 → 提交直接拒绝（明确 4xx）**，绝不回落 env、绝不静默放行（符合 CLAUDE.md §四 #5 失败优先）。env 配置在切换后**仅作一次性种子导入**来源，不再是运行时真相源。

### 决策 4：保存期校验取代启动期校验

原 `EnvVideoCatalog` 的 fail-fast 校验（每分辨率档须有 W×H + 定价，否则启动失败）迁移为**保存配置时校验自洽**：Admin API 写 catalog/pricing 时校验，不自洽则拒绝保存（4xx）。运行时读 DB 不再重复全量校验。

### 决策 5：settle 侧零改动（价格快照不变量）

定价 DB 化**只影响 submit 侧 reserve 估算**；inflight 任务用 reserve 时冻结进 `financial_snapshot` 的快照价结算。**调价只影响新提交**，inflight 不受影响，维持 `settle ≤ reserve` 不变量与账本不变量（对齐 codex-review-v1 计费正确性教训）。

### 决策 6：admin-cli 配置命令作废，运维敏感命令保留

计划 `2026-05-28-001` 的 Unit 11（admin-cli channel/entitlement/video_model 配置子命令）**作废**——配置面由管理后台承载。**保留** `cmd/admin-cli/` 既有的运维敏感命令（migrate / drift-check 等），与 ADR-0004「admin-cli 保留用于紧急冻结 / break-glass / 一次性迁移 / 启动校验」一致。本 ADR 不取消 admin-cli 二进制本身，只取消其**功能配置**职责。

## 后果

### 变得更容易

- ✅ 运维经 UI 在线配模型/价/授权/并发，不碰 env/SQL/重启。
- ✅ 多模型/多档天然支持（catalog 多条）。
- ✅ task.Service / 结算逻辑零改动（接口不变 + 读快照）。
- ✅ 并发 cap 覆写即时生效且无 TOCTOU（折进原子 claim SQL）。

### 变得更难 / 代价

- ⚠️ 新增 3 张表 + 对应迁移 + 运行时读取/缓存逻辑。
- ⚠️ catalog/pricing 短 TTL 缓存引入「秒级生效」语义（非毫秒强一致）；须文档明确。
- ⚠️ `ClaimConcurrencySlot` 加 COALESCE 后须重新 EXPLAIN ANALYZE（保原子语义 + 索引命中）。
- ⚠️ env→DB 切换是触及提交/资金热路径的改造，须 fail-closed + 视频流全链回归测试。

### 备选方案为什么被拒绝

| 备选 | 拒绝理由 |
|---|---|
| **保持 env + 重启**（不 DB 化） | UI 在线配置无法实现；用户已明确要 UI 配置 |
| **admin-cli 配置命令**（原 Unit 11） | 用户明确否决，配置面走 UI |
| **DB 无配置时回落 env** | 违反失败优先；env 与 DB 双源易漂移、难对账；改 fail-closed |
| **catalog/pricing 每请求查 DB（不缓存）** | 提交热路径多行读放大延迟；短 TTL 缓存足够，cap/entitlement 才需即时 |
| **pg LISTEN/NOTIFY 即时失效** | 本期过度工程；秒级 TTL 满足运维改配频率，留作未来优化 |

## 验证

- 运维经 Admin API 建模型+定价+授权+cap → 运行中网关对**新提交**按 DB 配置鉴权/校验/计费/限并发。
- 调价后 inflight 任务用快照价 settle，新提交用新价；账本不变量不变。
- DB 无该 model/档 → 提交 fail-closed 4xx（不回落 env）。
- `ClaimConcurrencySlot` 加 COALESCE 后 EXPLAIN ANALYZE 附 PR；并发 TOCTOU 不超卖。
- 迁移 0011–0013 up/down 往返；schema.md 同步补登 0008–0013。

---
date: 2026-05-31
topic: gateway-admin-console-config
---

# 网关管理后台配置线（取代异步视频计划 Unit 11 的 admin-cli 方案）

## Problem Frame

网关的运维配置——开通上游渠道凭据、登记 gateway 模型与上游绑定、设定积分/计费规则、给业务账户授权模型、设账户×模型并发上限——目前**只能改 env + 重启**或**手刷 SQL**；其中 channel 凭据与 entitlement 连任何配置面都没有（计划 Unit 11 原拟用 admin-cli 补，已被否决）。

运维人员需要一个**前端管理后台**在线完成全部配置，改动**即时对运行中的网关生效**，无需重启、无需碰数据库。消费者是内部运维（非公众用户）；业务系统仍走 Key 鉴权，不受影响。

参与方与数据流：

```
运维(浏览器)
   │  用户名/密码会话登录
   ▼
React 管理后台 (web/admin, 经 embed.FS 嵌入单二进制)
   │  会话 Cookie 调 /admin/v1/*
   ▼
Admin API (internal/admin, 复用 /admin/v1 中间件链 + scope)
   │  读写
   ▼
PostgreSQL  ◀── 运行中的 relay + Asynq workers 读取生效配置
（catalog / pricing / channel 凭据密文 / entitlement / 并发 cap 覆写 / 运维账户）
```

## Requirements

**认证与运维账户**
- R1. 运维通过**用户名/密码会话登录**进入管理后台（不面向公众，无自助注册）。
- R2. 系统存在一个**初始管理员**（首次启动种子化，不依赖 CLI 配置命令）；初始管理员可在后台**开通/停用其余运维登录账户**。
- R3. 已登录运维默认拥有全部配置能力（单一运维角色）；精细 RBAC、二阶段认证（2FA）、密码自助找回**推后**，真有需要再加。
- R3a. 程序化/自动化访问仍可用既有 Admin Token Bearer 调 /admin/v1（与会话登录**并存**，对应设计文档「Session Cookie (UI) + Admin Token Bearer (API)」）。

**模型 catalog 配置**
- R4. 运维可在后台**新增/编辑/启停多个 gateway 模型**（catalog 由 env 单条改为 DB 多条）：gateway model 名 → 上游 provider/上游 model → 绑定 channel → 能力描述符（task_type 支持集 + 参数档：duration/resolution/ratio/fps 等）。
- R5. 能力描述符驱动的请求校验以 DB 配置为准；保存配置时 fail-fast 校验自洽（如每个分辨率档须有对应 W×H 与定价，否则拒绝保存）。
- R6. 后台可查看每个模型当前生效配置（含掩码后的绑定 channel 标识）。

**计费/积分规则配置**
- R7. 计费规则用**结构化字段**配置（非表达式 DSL）：每 (model, resolution, has_input_video) → {W×H, CNY/百万 token 单价, 倍率, 最低 token 下限, 计费单位}；保留设计 §5.1 多单位（token / video_second）以 unit 字段表达。
- R8. 调价只影响**新提交**任务；已 reserve 的 inflight 任务用其价格快照结算（保持账本不变量与 settle ≤ reserve）。

**渠道凭据配置（write-only）**
- R9. 运维可在后台**新建/轮换渠道凭据**（5 段：APIKey / ARK AK-SK / TOS AK-SK / bucket 等 / project_id）；明文经 Admin API 提交后 AES-GCM 加密入库（复用 crypto.Keyring 当前最高版本 KEK），明文**绝不回显、绝不入日志**。
- R10. 列表/详情只显示**掩码视图**（密钥类固定占位「已设置」，半公开标识前缀 + ***）；可启停渠道（软下线优先于硬删）。

**entitlement 开通**
- R11. 运维可在后台给业务账户**授权/撤销可用 gateway 模型**（account×model grant / revoke / list；复用既有 entitlement 表与 sqlc）。

**账户×模型并发上限**
- R12. 运维可在后台**设/改 per-(account,model) 并发上限覆写**（cap 由 env 改为 DB 持久化覆写 + 默认值兜底）；可查看当前 inflight 占用。

**配置生效与集成**
- R13. 所有配置改动**即时对运行中的网关与 Asynq workers 生效**，无需重启（运行时从 DB 读取，必要时带短 TTL 缓存/失效）。
- R14. 现有异步视频中继（计划 Units 4/6/8/10）从 env 配置切到 DB 配置读取；保留安全兜底——DB 无配置时**明确 fail-closed**（拒绝提交），而非静默放行。

**前端管理后台**
- R15. React 管理后台（web/admin，当前仅 .gitkeep）从零脚手架，技术栈遵循 CLAUDE.md §三（React 19 / Vite v6 / shadcn/ui / Tailwind v4 / TanStack / i18next zh+en），经 Go embed.FS 嵌入单二进制。
- R16. 后台 UI 文案走 i18next zh+en 双语；表单含客户端 + 服务端双重校验；关键操作（删渠道、撤 entitlement、改价）有二次确认。

**可观测/审计**
- R17. 配置类写操作（建/改/删渠道、改价、grant/revoke、改并发、建运维账户）记审计事件（actor / 对象 / 动作；**不记凭据明文、不记签名 URL**），复用既有 AdminAudit 链。

## Success Criteria
- 运维仅通过后台 UI 即可走完「开通渠道 → 登记模型 + 定价 → 授权账户 → 设并发」全流程，把一个新模型/新业务接入网关，**全程不碰 env / SQL / 重启**。
- 配置改动后，运行中的网关对**新提交**任务按新配置鉴权/校验/计费/限并发。
- 凭据 write-only：任何列表/详情/日志/审计都不含明文；DB 内为密文。
- 业务侧 Key 鉴权与同步 relay 行为不变；账本不变量 `available + reserved + used_total = recharge_total` 不变。

## Scope Boundaries
- **不做 admin-cli 功能配置命令**（Unit 11 CLI 方案作废；唯一例外是初始管理员的非交互种子化）。
- **不做 billingexpr DSL 求值器**（结构化字段够用；异形计费 SKU 出现时再开 ADR）。
- **不做精细 RBAC / 2FA / 公开注册 / 密码自助找回**（推后）。
- **不做输入媒体上传、人像库、多 provider 适配**（沿用原异步视频 MVP 边界；本线只做「多模型/多档配置」的数据与 UI，上游适配器仍只 seedance）。

## Key Decisions
- 配置面 = 前端管理后台 + DB-backed Admin API，取代 admin-cli（用户决策 2026-05-31）。
- 计费 = 结构化 DB 定价表，DSL 推后（用户决策 2026-05-31）。
- 交付 = 后端 Admin API + React 前端全栈一体（用户决策 2026-05-31）。
- 登录 = 用户名/密码会话登录 + 初始管理员开通账户；2FA/RBAC 推后（用户决策 2026-05-31）。
- catalog / pricing / 并发 cap 全部 DB 化——**推翻**异步视频计划原 Scope Boundaries 的「catalog DB 化推后」（须 ADR 记录）。

## Dependencies / Assumptions
- 复用：`internal/admin` + `/admin/v1` 中间件链 + `internal/httpapi/middleware/admin_scope.go`；`channel.Service`（write-only 掩码完备）；`crypto.Keyring`（多版本 KEK）；`channel`/`entitlement` sqlc；`internal/relay/video` 的 catalog/billing/concurrency 现有 Go 类型（值解析逻辑可复用，配置来源从 env 改 DB）。
- 前端技术栈已在 CLAUDE.md §三预批，框架本身**无需**新开 ADR。
- 业务账户已有 `business_account` 表 + Admin API（5 端点）作为参照实现范式。

## ADRs to open
- **ADR-0007（运维配置 DB 化 + 在线生效）**：定 catalog/pricing/并发 cap 的 DB 化与运行时读取/失效策略；显式推翻「catalog DB 化推后」。
- **ADR-0008（管理后台会话认证）**：用户名/密码 + 初始管理员开通账户 + 会话/CSRF；明确**不做** RBAC/2FA 的边界与未来扩展位。
- 计费仍为结构化字段 → 无需 DSL ADR；前端栈已预批 → 无需 ADR。

## Outstanding Questions

### Deferred to Planning
- [影响 R13][技术] 运行时读 DB 配置的方式：每请求查 / 带 TTL 缓存 + 失效（pg LISTEN/NOTIFY 或版本号轮询）——由规划阶段评估，权衡一致性与热路径开销。
- [影响 R2][技术] 初始管理员种子化的具体机制（env 种子 vs 迁移 seed vs 首启向导），硬约束：不引入 CLI 配置命令。
- [影响 R9][技术] ADR-0006 清单的「KEK 凭据重加密（解旧写新 + --dry-run）」是否纳入本线；若纳入，后台触发还是后端批处理任务。
- [影响 R12][技术] 并发 cap 覆写表结构与 claim 查询如何 `COALESCE(覆写, 默认)`，且不破坏现有原子占位语义。
- [影响 R7][需调研] 多单位（token / video_second）结构化定价表如何统一表达，对齐设计 §5.1 与参考实现 storyboard-assistant 定价表。
- [影响 R14][技术] env→DB 切换的迁移/灰度路径：是否保留 env 作为种子导入一次，还是直接以 DB 为唯一真相源。

## Next Steps
→ `/ce:plan`：把异步视频计划的 Unit 11 重写为本配置线的多单元分解（后端 Admin API + 4 域 DB 化 + 会话认证 + React 前端 + 集成切换 + ADR-0007/0008）。

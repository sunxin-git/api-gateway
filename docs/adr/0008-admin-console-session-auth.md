# ADR-0008: 管理后台会话认证（用户名/密码 + bcrypt + PG 会话 + 初始管理员种子）

- **状态：** Accepted
- **日期：** 2026-05-31
- **决策人：** sunxin
- **相关文档：** ADR-0004（P0 含 React UI，已定「Session Cookie 鉴权 + CSRF token 在前端架构里就位」）；`docs/brainstorms/2026-05-31-gateway-admin-console-config-requirements.md`；`docs/plans/2026-05-31-001-feat-admin-console-config-plan.md`；`docs/api/admin-api.md`（D10：token 仅 Bearer 头、禁 cookie/query 传 token）
- **适用范围：** 管理后台（运维）登录与会话；不涉及业务系统鉴权（业务走 Key，不变）。

## 背景

ADR-0004 已**接受** P0 引入完整 React UI 且「Session Cookie 鉴权 + CSRF token 在前端架构里就位」，但**未定后端机制**。当前代码只有 **Admin Token Bearer**（`internal/httpapi/middleware/admin_token_auth.go`），无任何 session / cookie / login / 口令哈希基础设施。管理后台（计划 `2026-05-31-001`）需要运维用账号登录，故须落地会话认证。

用户对认证的明确要求（brainstorm，2026-05-31）：**登录就用最简单的**——用户名/密码（或手机号+验证码），必要时加二阶段认证；不面向公众用户；运维是**初始管理员授权的预设账户**；不搞复杂权限系统。

本 ADR 是对 ADR-0004 既有方向的**机制具体化**，不是新方向决策。

现状确认：

- 无 session/cookie 基础设施（须新建）。
- `golang.org/x/crypto`（含 `bcrypt`）**已在 go.mod 依赖树**（indirect，asynq/其他传递）——bcrypt **非新引入依赖**，`go mod tidy` 提为 direct 即可。
- `internal/admintoken` 的 HMAC+pepper 适用**高熵 token**，**不适用低熵口令**。

## 决策

### 决策 1：用户名/密码会话登录（最简模型）

运维经 `POST /admin/login`（用户名 + 口令）登录，成功下发会话 Cookie。**单一运维角色**：所有登录运维默认拥有全部配置能力。**不做** RBAC 细分、2FA、公开注册、密码自助找回（推后，真有需要再加，留扩展位）。手机号+验证码作未来可选登录方式（不引入 SMS 依赖，本期不做）。

### 决策 2：口令哈希 = bcrypt（非 HMAC）

用户口令是**低熵**，必须用慢哈希。采用 `golang.org/x/crypto/bcrypt`（已在依赖树，非新依赖；cost factor 按目标登录延迟基准取值）。**不**照搬 admintoken 的 HMAC-SHA-256（那是给高熵随机 token 的）。是否叠加 pepper 预哈希：默认不叠（简单优先），留为未来增强。口令明文/哈希**绝不**回显、绝不入日志。

### 决策 3：会话存 PostgreSQL

会话存 PG（`admin_session` 表，存 `session_token_hash` 而非明文）。理由：admin-only 部署可能不带 Redis（Redis 是异步执行基座依赖，非 admin 必需）；会话存 PG 与「DB 单一真相源 / fail-closed」一致，且与既有 pgxpool 装配同源。会话明文 token 仅写入 Cookie，库内只存其 HMAC（查会话热路径，复用 `GATEWAY_TOKEN_PEPPER` 或独立 pepper，落地定）。

### 决策 4：AdminPrincipal 双通道归一化（会话 + Bearer 并存）

`/admin/v1/*` 同时接受两种鉴权，互不混用（对齐 admin-api.md D10：Bearer token 绝不经 cookie/query 传）：

- **会话 Cookie 通道（UI）**：cookie → 查活跃 `admin_session` → 注入 `AdminPrincipal{type: operator, id, caps: 全部配置}`。
- **Bearer Token 通道（自动化/既有）**：现有 `AdminTokenAuth` → 注入 `AdminPrincipal{type: admin_token, id, scopes: token.scopes}`。

下游 Throttle / Scope / Audit 统一读 `AdminPrincipal`：operator 放行全部配置 scope；admin_token 仍按 token scope 校验（既有 5 端点**完全向后兼容**）。审计 actor 扩 `operator:<id>`。

### 决策 5：CSRF + Cookie 安全属性

会话 Cookie 的**状态变更请求**（POST/PUT/DELETE）须带 CSRF token（登录时下发，前端后续请求带 header；double-submit cookie 或同步器 token，落地定）。Bearer 通道无 ambient auth，**豁免 CSRF**。会话 Cookie 设 `HttpOnly + Secure + SameSite`。

### 决策 6：初始管理员 env 种子（幂等，不引 CLI）

鸡生蛋问题：首个管理员经**环境变量种子**——启动时若 `operator_account` 表空且提供 `GATEWAY_ADMIN_BOOTSTRAP_USERNAME` / `GATEWAY_ADMIN_BOOTSTRAP_PASSWORD`，建初始管理员（`created_by="seed"`），**幂等**（表非空跳过）。其余运维账户由初始管理员经后台开通。production 下表空且无 bootstrap env → fail-fast 或明确告警（落地定）。**不引入 admin-cli 配置命令**（与 ADR-0007 一致；种子是非交互启动逻辑，非 CLI 子命令）。

## 后果

### 变得更容易

- ✅ 运维用账号登录管理后台，无需持 Bearer token。
- ✅ 会话/Bearer 双通道并存，既有自动化与新 UI 互不影响。
- ✅ 口令 bcrypt + 禁用账户拒登 + 防枚举，符合商业平台底线。
- ✅ 零新第三方依赖（bcrypt 已在树）。

### 变得更难 / 代价

- ⚠️ 新增 operator/session 两表 + 登录/登出端点 + 会话中间件 + CSRF——纯新建子系统（虽小）。
- ⚠️ 鉴权链改造（AdminPrincipal 归一化）须保证既有 5 个 Bearer 端点回归不破。
- ⚠️ 初始管理员种子须谨慎（弱口令风险）；production 须强口令 + 首登改密（首登改密本期可选，推后）。

### 备选方案为什么被拒绝

| 备选 | 拒绝理由 |
|---|---|
| **复用 Admin Token Bearer 作 UI 登录**（贴 token） | 运维体验差（无账号/口令概念）；token 落浏览器存储；与 ADR-0004 已定 Session 方向不符 |
| **口令用 HMAC-SHA-256**（套 admintoken） | 口令低熵，HMAC 抗暴力不足；必须 bcrypt/argon2 慢哈希 |
| **会话存 Redis** | admin-only 部署可能无 Redis；PG 更稳、与 fail-closed 一致 |
| **精细 RBAC / 2FA / 公开注册（本期）** | 用户明确「别搞复杂权限系统」；非公众面向；单一运维角色够用，留扩展位 |
| **会话 token 经 query/cookie 与 Bearer 混用** | 违反 admin-api.md D10；两通道须独立 |

## 验证

- 登录正确口令 → Set-Cookie（HttpOnly+Secure+SameSite）+ CSRF token；带 cookie+CSRF 调 `/admin/v1` 通过（operator principal）。
- 既有 Bearer 端点回归通过（双通道并存不破）。
- 错口令/禁用账户 → 401（不区分「不存在 vs 错口令」防枚举）；会话过期 → 401；缺 CSRF 的状态变更 → 403。
- 口令哈希/明文不出现在任何返回/日志/审计；会话明文 token 不入库。
- 初始管理员种子幂等：表空+env → 建账户；再启动表非空 → 不重复建。

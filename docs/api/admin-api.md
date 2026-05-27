---
title: Admin API v1 — 业务系统接入契约
audience: 业务系统接入工程师
status: active
phase: Phase 2 工作流 D-min
last-updated: 2026-05-27
---

# Admin API v1（业务系统接入契约）

> **本文档是业务系统接入 api-gateway 的唯一对外契约。**
> 任何字段 / 错误码 / 路径变更必须先改本文档再改代码，且变更需保持向后兼容（破坏性变更必须发版本号 v2）。
>
> 计划文档：[Phase 2 工作流 D-min plan](../plans/2026-05-27-003-feat-workflow-d-min-admin-api-plan.md)
> 设计文档（架构上下文）：[多媒体网关设计](../multimedia-gateway-design.md) §9bis.6

---

## 1. 综述

Admin API 是业务系统操作业务账户的最小闭环：开通账户 / 充值 / 退款 / 查余额 / 自检 token。

| 维度 | 当前 P0 | 备注 |
|---|---|---|
| 协议 | HTTP/1.1 + JSON | TLS 由部署侧负责（见 §6 部署约束） |
| 版本 | `v1` | URL 前缀 `/admin/v1/*`；破坏性变更走 `/admin/v2/*` |
| 鉴权 | Bearer Token | 仅 header；不支持 query / cookie |
| 货币 | **仅 CNY** | 所有 amount / limit 单位 = 分（minor unit，1 元 = 100 分）；多货币 Phase 3+ |
| 时区 | **UTC** | daily 阀门按 UTC 计算日窗口；业务系统看到的"今日"可能与配额"今日"有 8h 偏差（CN+8）|
| 编码 | UTF-8 | request / response body 默认 UTF-8 |

**5 个 endpoint**：

| Method | Path | 用途 | 所需 scope |
|---|---|---|---|
| `POST` | `/admin/v1/business-accounts` | 创建业务账户 | `business_account:create` |
| `POST` | `/admin/v1/business-accounts/:id/recharge` | 为账户充值 | `business_account:recharge` |
| `POST` | `/admin/v1/business-accounts/:id/refund` | 退款 | `business_account:refund` |
| `GET` | `/admin/v1/business-accounts/:id/balance` | 查询余额 | `business_account:read` |
| `GET` | `/admin/v1/whoami` | Token 自检 | （无 scope 要求）|

**P0 不实装（推 P1+）**：webhook 订阅 CRUD / outbox 拉取重放 / business_account 列表查询 / 账户 suspend·resume·delete / Admin Token 管理 UI / Admin Token 平滑轮换。

---

## 2. 鉴权五件套

每次请求必须穿过以下五层防护，任一层失败即拒绝：

### 2.1 Bearer Token
```http
Authorization: Bearer sk-<base64url-43chars>
```

- token plaintext 由 `admin-cli token create` 一次性返回；**不再可追回**
- 网关存的是 `HMAC-SHA-256(GATEWAY_TOKEN_PEPPER, plaintext)` 的 hex，DB 全量泄露也无法离线穷举
- 失败：401 `unauthorized`（缺 header / 非 Bearer scheme / hash 无匹配 / 已 revoked / 已 expired）

### 2.2 Scope（按动作授权）
每个 endpoint 要求特定 scope；token.scopes 不含即 403。

完整 scope 清单：
- `business_account:create` / `business_account:read` / `business_account:recharge` / `business_account:refund`

**P0 启用范围**：上表 4 个；P1 接 UI 时扩展 `business_account:suspend` / `business_account:delete` / `token:read` / `token:write` / `webhook:manage` 等。

### 2.3 IP allowlist（源 IP 白名单）
Token 持有 CIDR 列表，源 IP 不命中即 401（**不**消耗限流配额）。

- 空 allowlist = fail-closed 拒全部（admin-cli 创建时校验至少 1 个 CIDR）
- 源 IP 取自 `c.ClientIP()`，由 Gin 按 `GATEWAY_TRUSTED_PROXIES` 解析 X-Forwarded-For（生产模式强制配置，见 §6）

### 2.4 阀门（throttle）
Token 持有 7 个阀门字段（NULL = 无限制）：

| 字段 | 触发 | 响应 |
|---|---|---|
| `single_recharge_max` | 单笔充值 amount 超阈 | 429 `single_recharge_exceeded` |
| `daily_recharge_quota_limit` | 当日累计充值（UTC day）超阈 | 429 `daily_recharge_quota_exceeded` |
| `single_refund_max` | 单笔退款 amount 超阈 | 429 `single_refund_exceeded` |
| `daily_refund_quota_limit` | 当日累计退款（UTC day）超阈 | 429 `daily_refund_quota_exceeded` |
| `daily_account_create_limit` | 当日创建账户数超阈 | 429 `daily_create_exceeded` |
| `requests_per_minute` | 60s 滑动窗口请求数超阈 | 429 `rate_limited` |
| `circuit_breaker_enabled` | 1h 窗口内 100 次 4xx/5xx → 跳闸 1h | 429 `circuit_open` |

**两步式语义**（决策 D2 + D11）：失败的请求 / 幂等重放命中**不**累加 daily 配额。

### 2.5 Audit（审计）
每次请求自动 emit 一行 audit JSON：

```json
{
  "event": "admin_audit",
  "tier": 1,
  "request_id": "019e68bf-7267-79b3-9ee0-c849ebbb8a7d",
  "timestamp_utc": "2026-05-27T09:24:30.123Z",
  "token_id": 811,
  "token_description": "smoke-test",
  "actor": "admin_token:811",
  "source_ip": "127.0.0.1",
  "method": "POST",
  "path": "/admin/v1/business-accounts/:id/refund",
  "request_hash": "abcdef0123456789abcdef0123456789",
  "body_size_bytes": 123,
  "status": 200,
  "duration_ms": 12,
  "outcome_code": "ok"
}
```

**Tier 分级**（决策 D3）：
- **Tier1（同步落盘 O_APPEND+O_SYNC）**：refund 路径、auth_failed (401/403)、idempotency_conflict
- **Tier2（异步 stderr）**：create / recharge / balance read / whoami

Tier1 写失败时 `/readyz` 立刻返 503 摘流，恢复需重启进程。

---

## 3. 通用契约

### 3.1 请求

| 头/字段 | 必填 | 说明 |
|---|---|---|
| `Authorization: Bearer <token>` | 是 | Token plaintext |
| `Content-Type: application/json` | POST 必填 | request body 一律 JSON |
| `X-Request-Id` | 否 | 业务系统可携带 trace id；缺失时网关生成 UUIDv7 |
| body size | ≤ **64 KiB** | 超出立刻 413 `payload_too_large`（不进鉴权） |

### 3.2 响应

成功 → `200 OK` / `201 Created` + handler 定义的 JSON。

失败 → 统一错误响应：

```json
{
  "error": {
    "code": "<sentinel_code>",
    "message": "<中文说明>",
    "request_id": "019e68bf-7267-79b3-9ee0-c849ebbb8a7d"
  }
}
```

### 3.3 错误码 → HTTP status 映射

| HTTP | error.code | 触发场景 |
|---|---|---|
| **400** | `invalid_request_body` | JSON 解析失败 / 入参校验失败（字段缺失 / 越界）|
| **400** | `invalid_amount` | LedgerService 拒绝 amount ≤ 0 |
| **401** | `unauthorized` | Token 缺失 / 格式非法 / 不存在 / 已 revoked / 已 expired |
| **401** | `ip_not_allowed` | 源 IP 不在 token 白名单 |
| **403** | `insufficient_scope` | Token 缺所需 scope |
| **404** | `account_not_found` | 业务账户不存在 |
| **409** | `account_already_exists` | 创建账户重名 |
| **409** | `account_frozen` | 账户已冻结（运营或 reconciler drift 触发）|
| **409** | `idempotency_conflict` | 同 external_ref 但 body 不一致（攻击 / 业务 bug 信号）|
| **409** | `insufficient_used` | 退款金额超已结算金额 |
| **413** | `payload_too_large` | request body > 64 KiB |
| **429** | `single_recharge_exceeded` | 单笔充值超阀门 |
| **429** | `daily_recharge_quota_exceeded` | 当日充值额度用尽 |
| **429** | `single_refund_exceeded` | 单笔退款超阀门 |
| **429** | `daily_refund_quota_exceeded` | 当日退款额度用尽 |
| **429** | `daily_create_exceeded` | 当日创建账户数超阀 |
| **429** | `rate_limited` | RPM 滑动窗口超限 |
| **429** | `circuit_open` | Token 熔断中 |
| **503** | `version_conflict` | 余额 CAS 冲突 / 短暂高并发；**业务系统应自带重试预算** |
| **500** | `internal_error` | 网关内部异常（DB 暂断 / panic）；联系网关运维 |

### 3.4 重试策略建议

| HTTP | 是否重试 | 建议 |
|---|---|---|
| 4xx（429 除外）| ❌ 不重试 | 入参问题 / 鉴权问题；修复后重发 |
| 429 | ⏱ 退避后重试 | 指数退避（base=1s, max=60s）；若是 `circuit_open`，等 1h 或联系运维 |
| 503 `version_conflict` | ✅ 立即重试 | 网关侧 CAS 冲突，下次大概率成功；建议 3 次重试 + 0.1s/0.3s/1s 退避 |
| 5xx 其他 | ⏱ 退避后重试 | 同 429；同时报警 |
| 网络超时 | ⏱ 退避后重试 | + 客户端超时建议 ≥ 30s |

**关键不变量**：充值 / 退款用同 external_ref（recharge）或同 correlation_id（refund）重试是幂等的，网关返回原 ledger entry，余额**不会**重复累加。

---

## 4. Endpoint 详细规约

### 4.1 `POST /admin/v1/business-accounts` — 创建业务账户

| 项 | 值 |
|---|---|
| Scope | `business_account:create` |
| Idempotency | 无；同 id 重复创建返 409 |
| Audit Tier | 2 |

**Request body**：

```json
{
  "id": "creator-platform-tenant-001",
  "isolation_required": false,
  "metadata": {"plan": "enterprise"}
}
```

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `id` | string | 是 | 1 ≤ len ≤ 64；字符集 `[A-Za-z0-9_-]` |
| `isolation_required` | bool | 否 | provider 隔离开关（默认 false）|
| `metadata` | object | 否 | 业务侧透传 JSON；网关不解析 |

**Response 201**：

```json
{
  "id": "creator-platform-tenant-001",
  "status": "active",
  "isolation_required": false,
  "created_at": "2026-05-27T09:24:30Z"
}
```

**错误**：400 `invalid_request_body` / 409 `account_already_exists` / 429 `daily_create_exceeded`

---

### 4.2 `POST /admin/v1/business-accounts/:id/recharge` — 充值

| 项 | 值 |
|---|---|
| Scope | `business_account:recharge` |
| Idempotency | `external_ref` 复合 UNIQUE on `(entry_type='recharge', idempotency_key)` |
| Audit Tier | 2（idempotency_conflict 升级为 Tier1）|

**Request body**：

```json
{
  "amount": 100000,
  "external_ref": "topup-order-abc-20260527-001",
  "reference_type": "topup_order",
  "reference_id": "abc-20260527-001",
  "metadata": {}
}
```

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `amount` | int64 | 是 | minor unit，> 0；CNY 分（1000 = 10 元）|
| `external_ref` | string | 是 | 业务系统幂等键；1 ≤ len ≤ 128；同 ref + 同 body → 幂等命中 |
| `reference_type` | string | 否 | 反查路径标签，如 `topup_order`；len ≤ 64 |
| `reference_id` | string | 否 | 反查路径 id；len ≤ 64 |
| `metadata` | object | 否 | 透传 |

**Response 200**：

```json
{
  "id": 380869,
  "entry_type": "recharge",
  "amount": 100000,
  "available_delta": 100000,
  "used_delta": 0,
  "correlation_id": "topup-order-abc-20260527-001",
  "idempotency_key": "topup-order-abc-20260527-001",
  "created_at": "2026-05-27T09:24:31Z",
  "idempotent": false
}
```

- `idempotent: false` → 本次首次入账，账户余额已 +amount
- `idempotent: true` → 重放命中，返原 entry，余额**未变化**

**错误**：400 / 401 / 403 / 404 / 409 `account_frozen` / 409 `idempotency_conflict`（同 ref 不同 amount）/ 429 各阀门 / 503

**幂等流程**：

```
业务系统第 1 次 → 网关写 ledger entry → 200 + idempotent=false
业务系统重试   → 网关命中 idempotency_key + body 一致 → 200 + idempotent=true（**余额未变**）
业务系统重试 但 amount 改了 → 409 idempotency_conflict（**Tier1 audit + 告警**）
```

---

### 4.3 `POST /admin/v1/business-accounts/:id/refund` — 退款

| 项 | 值 |
|---|---|
| Scope | `business_account:refund` |
| Idempotency | `correlation_id` 复合 UNIQUE on `(business_account_id, correlation_id, entry_type='refund')` |
| Audit Tier | **1（始终同步落盘）** |

**Request body**：

```json
{
  "amount": 5000,
  "correlation_id": "manual-refund-2026-05-27-001",
  "reference_type": "manual_refund",
  "reference_id": "RFD-001",
  "metadata": {"reason": "用户投诉退款"}
}
```

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `amount` | int64 | 是 | minor unit，> 0；不能超过当前 `used_total` |
| `correlation_id` | string | 是 | 1 ≤ len ≤ 128；网关侧幂等键 |
| `reference_type` / `reference_id` | string | 否 | 反查路径，各 ≤ 64 |
| `metadata` | object | 否 | 透传 |

**Response 200**：同 `recharge` shape，`entry_type: "refund"`，`available_delta: amount`，`used_delta: -amount`。

**错误**：400 / 401 / 403 / 404 / 409 `insufficient_used`（退款金额 > used_total）/ 429 refund 阀门 / 503

**安全**：退款是高危动作。每条记录都同步 fsync 到 Tier1 audit；若审计写失败，`/readyz` 立刻关闸摘流。

---

### 4.4 `GET /admin/v1/business-accounts/:id/balance` — 查询余额

| 项 | 值 |
|---|---|
| Scope | `business_account:read` |
| Idempotency | n/a（read-only）|
| Audit Tier | 2 |

**Response 200**：

```json
{
  "account_id": "creator-platform-tenant-001",
  "available": 95000,
  "reserved": 0,
  "used_total": 5000,
  "recharge_total": 100000,
  "refund_total": 0,
  "frozen": false,
  "version": 3,
  "updated_at": "2026-05-27T09:30:00Z"
}
```

**账本不变量**：`available + reserved + used_total = recharge_total`（refund_total 不进等式）。

`frozen=true` + `frozen_reason` 表示账户被冻结（reconciler drift / 运营手动）；此时所有写操作返 409 `account_frozen`。

**错误**：401 / 403 / 404

---

### 4.5 `GET /admin/v1/whoami` — Token 自检

| 项 | 值 |
|---|---|
| Scope | **无要求**（任何已鉴权 token 可调）|
| Idempotency | n/a |
| Audit Tier | 2 |

**用途**：业务系统启动时验证 token 是否有效 + 查看自己阀门配置 + 今日已用配额。

**Response 200**：

```json
{
  "token_id": 811,
  "description": "creator-platform-prod",
  "scopes": ["business_account:read", "business_account:recharge"],
  "ip_allowlist_cidr_count": 2,
  "expires_at": null,
  "throttle_limits": {
    "single_recharge_max": 500000,
    "daily_recharge_quota_limit": 10000000,
    "single_refund_max": null,
    "daily_refund_quota_limit": null,
    "daily_account_create_limit": 50,
    "requests_per_minute": 600,
    "circuit_breaker_enabled": true
  },
  "today_usage_utc": {
    "recharge_total_minor": 1500000,
    "refund_total_minor": 0,
    "account_create_count": 2
  },
  "circuit_state": {
    "open": false,
    "tripped_until": null,
    "error_count_in_window": 3
  },
  "server_time_utc": "2026-05-27T09:30:00Z"
}
```

**不返回**：
- `token_hash`（绝不暴露）
- `ip_allowlist` 具体 CIDR 列表（仅返数量；防泄露后嗅探精确网段）
- `created_by`（运维内部信息）

---

## 5. 金额与时区语义

### 5.1 货币
**P0 仅支持 CNY**。所有 `amount` / `*_max` / `*_limit` 字段单位 = 分（minor unit）：

| 业务直觉 | API 传值 |
|---|---|
| 1 元 | `100` |
| 100 元 | `10000` |
| 10000 元 | `1000000` |
| 100 万元（百万级单笔上限）| `100000000` |

**禁止**传小数 / 字符串金额；禁止假设其他货币。

多货币是 Phase 3+ 决策；引入时 schema 必须增 currency 字段，**禁止**复用同 `*_limit` 字段意义漂移（详见 [D-min plan §决策 D9](../plans/2026-05-27-003-feat-workflow-d-min-admin-api-plan.md)）。

### 5.2 时区
**daily 阀门按 UTC 计算**（决策 D9）：

- UTC 0 点切日；`daily_recharge_quota_limit` / `daily_refund_quota_limit` / `daily_account_create_limit` 在 UTC 0 点重置
- 业务系统 CN+8 时区看到的"今日"（CST 00:00–24:00）与配额"今日"（UTC 00:00–24:00）有 8h 偏差：
  - CST 08:00 = UTC 00:00 → 配额刚重置
  - CST 00:00 = UTC 16:00 → 配额已用大半（前 8h 在新 UTC day，后 16h 在旧 UTC day）

时区对照图：

```
CST: |------ 旧日 (08:00–24:00) ------|---- 新日 (00:00–08:00) ----|...
UTC: |--- 旧日 (00:00–16:00) ---|---- 新日 (16:00–24:00 + 0:00–8:00) ----|...
                                ↑
                            配额重置点（CST 08:00 = UTC 00:00）
```

业务系统若需要"按 CN+8 自然日核账"，应在自己业务侧维护对账表；网关侧仅按 UTC 给出权威用量数据（`whoami.today_usage_utc`）。

---

## 6. 部署约束（业务系统接入前必读）

### 6.1 TLS
**生产模式两种合法部署**（决策见 Unit 7 §1）：

- **Option A**：网关进程自带 TLS（`GATEWAY_LISTEN_TLS=true` + cert/key）
- **Option B**：前端反代终止 TLS，后端 HTTP（`GATEWAY_FRONT_TLS_ACK=true`）

无论哪种，所有 `/admin/v1/*` 响应**始终带** `Strict-Transport-Security: max-age=63072000; includeSubDomains`。

### 6.2 TrustedProxies
生产模式 `GATEWAY_TRUSTED_PROXIES` 必须显式配置为反代实际 CIDR（如 `10.0.0.0/8,127.0.0.1/32`）。

- 空 / 含 `0.0.0.0/0` → 进程拒启动
- 业务系统调用方应通过该反代访问；直连网关 IP 时 `c.ClientIP()` 取 RemoteAddr，可能不在 token allowlist 内

### 6.3 RPM 限速精度
P0 单实例部署；RPM 限制在进程内 ring buffer 内计算。

- 多实例部署时 RPM 按实例数倍放大（已知偏离）
- 进程重启即清零（OOM / liveness 重启会刷掉 RPM 状态；运维通过 `gateway_admin_throttle_rpm_cold_start_total{instance_id}` metric 看到）
- P1 接 Redis 后 RPM 跨实例统一

### 6.4 Audit Retention
网关侧**不**持久化 audit 到 DB；行为：

- Tier1 同步落盘到 `ADMIN_AUDIT_HIGH_VALUE_LOG_PATH`（生产强制配置）
- Tier2 走 stderr

**Retention 周期由部署侧 log shipper 决定**。建议保留 ≥ 1 年，依据：

- **工程实践**：refund / 攻击信号事故复盘窗口
- **合规边际**：网络安全法第二十一条要求"网络日志留存不少于 6 个月"（参考依据；正式合规仍由部署侧上游决定）
- **PCI-DSS / GDPR**：本网关不直接处理银行卡 / EU 个人数据；如业务涉及需按对应法规独立评估

---

## 7. Quickstart — 业务系统首次接入

### 7.1 运维侧
```bash
# 1. 运维通过 admin-cli 创建 token（明文一次性返回）
./bin/admin-cli token create \
    --description "creator-platform-prod" \
    --scope business_account:create,business_account:recharge,business_account:read \
    --ip-allowlist "10.0.0.0/8,203.0.113.5/32" \
    --single-recharge-max 1000000 \
    --daily-recharge-limit 100000000 \
    --rpm 600 \
    --circuit-breaker \
    --expires-in 8760h \
    --out /run/secrets/gateway-token.txt   # 0600 写入文件，stdout 不含 plaintext

# 2. 通过密钥管理器（age / 1Password / Vault）把 token 交付给业务系统
#    严禁 chat / wiki / email 粘贴
```

### 7.2 业务系统侧

**Step 1 — Whoami 自检**：

```bash
curl -sS -H "Authorization: Bearer sk-xxx" \
    https://gateway.example.com/admin/v1/whoami | jq
```

**Step 2 — 开通账户**：

```bash
curl -sS -X POST \
    -H "Authorization: Bearer sk-xxx" \
    -H "Content-Type: application/json" \
    -d '{"id":"tenant-001"}' \
    https://gateway.example.com/admin/v1/business-accounts
```

**Step 3 — 充值**：

```bash
curl -sS -X POST \
    -H "Authorization: Bearer sk-xxx" \
    -H "Content-Type: application/json" \
    -d '{"amount":100000,"external_ref":"topup-001"}' \
    https://gateway.example.com/admin/v1/business-accounts/tenant-001/recharge
```

**Step 4 — 查余额**：

```bash
curl -sS \
    -H "Authorization: Bearer sk-xxx" \
    https://gateway.example.com/admin/v1/business-accounts/tenant-001/balance
```

**Step 5 — 退款（需 refund scope 的独立 token）**：

```bash
curl -sS -X POST \
    -H "Authorization: Bearer sk-refund-only-xxx" \
    -H "Content-Type: application/json" \
    -d '{"amount":5000,"correlation_id":"RFD-001"}' \
    https://gateway.example.com/admin/v1/business-accounts/tenant-001/refund
```

### 7.3 SDK 重试模板（伪代码）

```python
def call_gateway(method, path, body=None, max_retries=3):
    for attempt in range(max_retries + 1):
        resp = http.request(method, BASE_URL + path,
                            headers={"Authorization": f"Bearer {TOKEN}"},
                            json=body, timeout=30)
        if resp.status_code < 500 and resp.status_code != 429:
            return resp  # 2xx 直接返回；4xx（429 除外）业务错误不重试

        if resp.status_code == 429:
            code = resp.json()["error"]["code"]
            if code == "circuit_open":
                raise CircuitOpenError("等 1h 或联系运维")
            if code in ("rate_limited", "daily_recharge_quota_exceeded", ...):
                if attempt == max_retries:
                    raise
                backoff = min(2 ** attempt, 60)  # 1, 2, 4, ..., 60s
                time.sleep(backoff)
                continue

        if resp.status_code == 503 and resp.json()["error"]["code"] == "version_conflict":
            time.sleep(0.1 * (3 ** attempt))  # 0.1, 0.3, 0.9s
            continue

        if resp.status_code >= 500:
            time.sleep(min(2 ** attempt, 60))
            continue

    raise GatewayError(resp)
```

---

## 8. 变更与版本管理

| 变更类型 | 行为 |
|---|---|
| 新增字段（请求/响应）| ✅ 允许；业务系统应忽略未知字段 |
| 新增 endpoint | ✅ 允许 |
| 新增 error.code | ✅ 允许；业务系统遇未知 code 按 HTTP status 大类处理 |
| 修改字段语义 | ❌ 禁止；走 `/admin/v2/*` |
| 删除字段 | ❌ 禁止；走 `/admin/v2/*` |
| 修改 HTTP status 映射 | ❌ 禁止；走 `/admin/v2/*` |

**当前版本**：`v1`（Phase 2 D-min 首发）。

---

## 9. 相关文档

- 设计文档：[多媒体网关设计 §9bis.6](../multimedia-gateway-design.md)
- 实施计划：[Phase 2 D-min plan](../plans/2026-05-27-003-feat-workflow-d-min-admin-api-plan.md)
- Schema 演化：[docs/db/schema.md §0003 演化](../db/schema.md#0003-演化2026-05-27)
- 术语表：[CONTEXT.md](../../CONTEXT.md)
- 部署运维：[docs/dev-setup.md](../dev-setup.md)（待 D-min 合并后 docs sync PR 补充 admin-cli token 示例）

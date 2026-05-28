---
title: Business API v1 — 业务系统调用契约（OpenAI 兼容 Relay）
audience: 业务系统接入工程师
status: active
phase: Phase 2 工作流 F-min
last-updated: 2026-05-28
---

# Business API v1（OpenAI 兼容 Relay 调用契约）

> **本文档是业务系统调用 api-gateway 上游模型能力的唯一对外契约。**
> 任何字段 / 错误码 / 路径变更必须先改本文档再改代码，且变更需保持向后兼容（破坏性变更必须发版本号 v2）。
>
> 计划文档：[Phase 2 工作流 F-min plan](../plans/2026-05-27-004-feat-workflow-f-min-openai-compat-relay-plan.md)
> 账户 / 充值 / 余额管理（运维侧）：[Admin API v1](admin-api.md)
> 设计文档（架构上下文）：[多媒体网关设计](../multimedia-gateway-design.md)

---

## 1. 综述

Business API 让业务系统用 **OpenAI 兼容协议**调用网关背后的上游模型；网关负责鉴权 → 预扣费 → 转发上游 → 按真实用量结算 → 透传响应。

与 [Admin API](admin-api.md) 的分工：
- **Admin API**（`/admin/v1/*`，Bearer admin token）：运维侧开户 / 充值 / 退款 / 查余额。
- **Business API**（`/v1/*`，Bearer business key）：业务侧调模型、花余额。两者凭据体系独立。

| 维度 | 当前 MVP | 备注 |
|---|---|---|
| 协议 | OpenAI Chat Completions（JSON over HTTP/1.1） | 业务可直接用 openai-python / openai-node SDK，改 `base_url` + `api_key` 即接入 |
| 版本 | `v1` | URL 前缀 `/v1/*`（OpenAI 兼容根路径）；破坏性变更走 `/v2/*` |
| 鉴权 | Bearer Business Key | 仅 header；无 scope（一个 key 全权限，与 OpenAI / DeepSeek 风格一致）|
| 模式 | **同步非流式** chat completions | `stream=true` 暂不支持（见 §9）|
| 货币 | **仅 CNY** | 计费单位 = 分（minor unit，1 元 = 100 分）|
| 时区 | **UTC** | 与 Admin API 一致 |
| 编码 | UTF-8 | request / response body 默认 UTF-8 |

**1 个 endpoint**：

| Method | Path | 用途 |
|---|---|---|
| `POST` | `/v1/chat/completions` | 同步 chat completions（OpenAI 兼容）|

**MVP 不实装（推 P1+）**：流式 SSE（`stream=true`）/ 异步任务（视频生成等）/ embeddings / images / 多 channel 路由 / 动态计费 DSL / channel 凭据 HTTP 管理。

**启用开关**：`/v1` 路由仅在 `GATEWAY_RELAY_ENABLED=true` 时注册；默认 `false`（admin-only 部署），此时调 `/v1/chat/completions` 返 404。

---

## 2. 鉴权

### 2.1 Bearer Business Key
```http
Authorization: Bearer biz-sk-<base64url-43chars>
```

- key plaintext 由 `admin-cli business-key create` 一次性返回；**不再可追回**。
- 网关存的是 `HMAC-SHA-256(GATEWAY_TOKEN_PEPPER, plaintext)` 的 hex；与 admin token 共享同一 pepper（同 plaintext 在两套体系算出同一 hash），但分属不同表、查询路径完全独立。
- 失败：401 `invalid_api_key`（缺 header / 非 Bearer scheme / 空 token / hash 无匹配 / 已 revoked）。出于安全，"未知 key" 与 "已吊销 key" 不区分返回。

### 2.2 与 admin token 的差异（MVP 简化）

| 维度 | Admin Token | Business Key |
|---|---|---|
| Scope | 细粒度（`business_account:recharge` 等）| **无**（一个 key 全权限）|
| IP allowlist | 强制（fail-closed）| **无**（业务系统常多 region 接入）|
| 错误响应格式 | `{"error":{"code","message","request_id"}}` | OpenAI 兼容 `{"error":{"message","type","code","request_id"}}` |
| 阀门 | 7 个（充值 / 退款 / 创建 / RPM / 熔断）| 仅 RPM（per key）|

### 2.3 RPM 限速
每个 business key 可选配 `requests_per_minute`（创建时 `--rpm`，NULL = 不限速）：
- 60s 滚动窗口超限 → 429 `rate_limit_exceeded`。
- 按 `key.id` 维度计数；P0 单实例进程内计算（多实例 / 重启语义见 §10.3）。

---

## 3. 通用契约

### 3.1 请求

| 头/字段 | 必填 | 说明 |
|---|---|---|
| `Authorization: Bearer <biz-key>` | 是 | Business key plaintext |
| `Content-Type: application/json` | 是 | request body 一律 JSON |
| `X-Request-Id` | 否 | 业务系统可携带 trace id；缺失时网关生成 UUIDv7，并在错误响应的 `error.request_id` 回带 |
| body size | ≤ **1 MiB** | 超出立刻 413 `payload_too_large`（含长上下文 / 多模态 base64 留余量）|

**协议透传规则**（决策 D3）：
- 业务 request body 按 OpenAI Chat Completions schema 提交；网关**仅改写 `model` 字段**为上游真实 model 名，其他字段（`messages` / `temperature` / `tools` / `response_format` / `top_p` / …）原样转发上游。
- 网关用字典中的上游凭据重写 `Authorization` 调上游；**业务侧任何 header 不转发上游**（PII / 凭据防护）。
- 上游响应 body 整体透传给业务（含 `id` / `choices` / `usage` / `model` 等）；上游 response header **不**透传（可能含敏感信息）。

### 3.2 响应

成功 → `200 OK` + 上游 OpenAI 兼容响应 body 原样透传（`Content-Type: application/json`）。

失败 → 统一 OpenAI 兼容错误响应：

```json
{
  "error": {
    "message": "账户余额不足，请联系运营充值",
    "type": "insufficient_quota",
    "code": "insufficient_quota",
    "request_id": "019e68bf-7267-79b3-9ee0-c849ebbb8a7d"
  }
}
```

业务方 SDK（openai-python / openai-node）依赖此 shape 解析报错；字段命名严格遵循 OpenAI 协议。

### 3.3 错误码 → HTTP status 映射

| HTTP | `error.type` | `error.code` | 触发场景 |
|---|---|---|---|
| **400** | `invalid_request_error` | `invalid_request_body` | JSON 解析失败 |
| **400** | `invalid_request_error` | `missing_model` | 缺 `model` 字段 |
| **400** | `invalid_request_error` | `empty_messages` | `messages` 为空 |
| **400** | `invalid_request_error` | `streaming_not_supported` | `stream=true`（MVP 限制）|
| **400** | `invalid_request_error` | `max_tokens_exceeds_context` | `max_tokens` 超模型 context 上限 |
| **400** | `invalid_request_error` | `model_not_found` | 未知 model（MVP 单条字典永远命中，实际不触发）|
| **401** | `invalid_api_key` | `missing_api_key` | 缺 Authorization / 非 Bearer / 空 token |
| **401** | `invalid_api_key` | `invalid_api_key` | key 无效 / 已吊销 |
| **401** | `invalid_api_key` | `account_not_found` | key 关联的业务账户不存在（FK 异常防御）|
| **402** | `insufficient_quota` | `insufficient_quota` | 账户余额不足以覆盖本次 reserve |
| **402** | `insufficient_quota` | `account_frozen` | 账户已冻结（运营 / reconciler drift）|
| **413** | `invalid_request_error` | `payload_too_large` | request body > 1 MiB |
| **429** | `rate_limit_exceeded` | `rate_limit_exceeded` | RPM 滚动窗口超限 |
| **4xx** | （上游透传）| （上游透传）| 上游返 4xx：原样透传上游 status + body（如 OpenAI 协议的 `context_length_exceeded` 等）|
| **502** | `upstream_error` | `upstream_5xx` | 上游返 5xx（不透传 5xx，改 502 明示"上游问题"）|
| **502** | `upstream_error` | `upstream_unreachable` | 无法连接上游（DNS / 拒连 / TLS）|
| **502** | `upstream_error` | `upstream_malformed` | 上游返 200 但 body 非合法 JSON |
| **503** | `server_error` | `temporarily_unavailable` | 余额 CAS 冲突 / 短暂高并发；业务应重试 |
| **504** | `upstream_timeout` | `upstream_timeout` | 上游超过 60s 未响应 |
| **500** | `api_error` | `internal_error` | 网关内部异常；联系网关运维 |

### 3.4 重试策略建议

| HTTP | 是否重试 | 建议 |
|---|---|---|
| 400 / 413 | ❌ 不重试 | 入参问题；修复请求后重发 |
| 401 | ❌ 不重试 | key 问题；检查凭据 / 联系运维 |
| 402 | ❌ 不重试 | 余额不足或账户冻结；充值后（见 [Admin API recharge](admin-api.md#42-post-adminv1business-accountsidrecharge--充值)）再试 |
| 429 | ⏱ 退避后重试 | 指数退避（base=1s, max=60s）|
| 上游 4xx 透传 | ❌ 不重试 | 上游协议错误（如超 context）；按上游 body 修复 |
| 502 / 504 | ⏱ 退避后重试 | 上游不稳定；退避 + 报警；504 注意客户端超时设 ≥ 65s |
| 503 `temporarily_unavailable` | ✅ 立即重试 | CAS 冲突，建议 3 次重试 + 0.1s/0.3s/1s 退避 |
| 500 | ⏱ 退避后重试 | 同时报警 |

**关键不变量**：网关对失败请求**自动 Release 预扣**，余额不会因失败请求被吞。MVP **不支持幂等键**（与 OpenAI 一致）；重试 = 一次全新请求，会重新预扣 / 结算。

---

## 4. Endpoint 详细规约

### 4.1 `POST /v1/chat/completions` — 同步 chat completions

| 项 | 值 |
|---|---|
| 鉴权 | Bearer business key |
| Idempotency | 无（MVP 不做；OpenAI 也不支持）|
| Audit Tier | 2（401 / 402 / 5xx 升 Tier1，见 §6）|
| 客户端超时 | 上游 60s；建议业务侧 timeout ≥ 65s |

**Request body**（OpenAI 兼容；透传上游）：

```json
{
  "model": "gw-default",
  "messages": [
    {"role": "system", "content": "你是助手"},
    {"role": "user", "content": "你好"}
  ],
  "max_tokens": 1024,
  "temperature": 0.7
}
```

| 字段 | 类型 | 必填 | 网关行为 |
|---|---|---|---|
| `model` | string | 是 | 业务可见 model 名（如 `gw-default`）；网关查字典改写为上游真实 model 名。MVP 单条字典：传任何值都路由到唯一字典记录 |
| `messages` | array | 是 | 非空；原样透传上游；用于 input token 估算 |
| `stream` | bool | 否 | **必须 false 或省略**；`true` → 400 `streaming_not_supported` |
| `max_tokens` | int | 否 | 缺省取字典默认 `max_context_tokens`；超过该上限 → 400 `max_tokens_exceeds_context`；用于 reserve 上界 |
| 其他 OpenAI 字段 | - | 否 | `temperature` / `top_p` / `tools` / `response_format` / … 原样透传上游 |

**Response 200**（上游响应原样透传）：

```json
{
  "id": "chatcmpl-xxx",
  "object": "chat.completion",
  "created": 1737000000,
  "model": "doubao-1-5-pro-32k-250115",
  "choices": [
    {"index": 0, "message": {"role": "assistant", "content": "你好！"}, "finish_reason": "stop"}
  ],
  "usage": {"prompt_tokens": 12, "completion_tokens": 5, "total_tokens": 17}
}
```

> 注：`model` 字段是**上游真实 model 名**（透传），不是业务传入的 `gw-default`。这是上游响应原样透传的结果；P1+ 可选改写回 gateway model 名。

**错误**：见 §3.3 完整映射表。

---

## 5. 计费与结算语义

### 5.1 两步式扣费（reserve → settle）

```
1. 估算 input tokens（保守上界）
2. reserve = ceil((input_est × in_price + max_tokens_or_default × out_price) / 1_000_000)  minor
3. ledger.Reserve(business_account_id, reserve, correlation=request_id)
   └─ 余额不足 → 402 insufficient_quota（不调上游）
4. 调上游
   ├─ 200 + usage → commit actual = ceil((prompt_tokens × in_price + completion_tokens × out_price)/1M)
   │                （actual 越界则 cap 到 reserve）
   ├─ 200 缺 usage → 兜底 commit reserve 全额（防 orphan reserve；运维信号 metric）
   ├─ 4xx → release reserve + 透传上游响应
   ├─ 5xx → release reserve + 502
   └─ timeout / unreachable / malformed → release reserve + 504/502
```

- **input token 估算**：`len(json(messages)) / 4`（保守上界，MVP 不引 tokenizer 依赖）。对中文偏保守（高估），但 reserve 是上界；结算阶段用上游真实 `usage` 退回多扣部分。
- **单价**：来自 env 字典（`GATEWAY_RELAY_PRICE_INPUT_PER_1M_MINOR` / `..._OUTPUT_...`），单位 = 每 1M token 的分数。
- **结算精度**：成功请求最终按上游真实 `usage` 计费；估算误差仅影响预扣额度（占用余额），不影响最终扣费金额。

### 5.2 余额查询
业务侧无独立余额接口；余额由运维通过 [Admin API balance](admin-api.md#44-get-adminv1business-accountsidbalance--查询余额) 查询。`available` 不足时本接口返 402。

### 5.3 货币与时区
**仅 CNY**，单位分（minor unit）；时区 UTC。与 [Admin API §5](admin-api.md#5-金额与时区语义) 完全一致。

---

## 6. 审计（Audit）

每次请求自动 emit 一行 `business_relay` audit JSON：

```json
{
  "event": "business_relay",
  "tier": 2,
  "request_id": "019e68bf-7267-79b3-9ee0-c849ebbb8a7d",
  "timestamp_utc": "2026-05-28T09:24:30.123Z",
  "business_account_id": "creator-platform-tenant-001",
  "api_key_id": 42,
  "actor": "business_key:42",
  "source_ip": "203.0.113.5",
  "method": "POST",
  "path": "/v1/chat/completions",
  "status": 200,
  "duration_ms": 1840,
  "outcome_code": "ok",
  "gateway_model": "gw-default",
  "upstream_model": "doubao-1-5-pro-32k-250115",
  "input_tokens": 12,
  "output_tokens": 5,
  "cost_minor": 1,
  "upstream_status": 200,
  "upstream_duration_ms": 1790
}
```

**不记录**（PII / prompt 敏感）：
- `messages` body（请求 / 响应正文均不入审计）。
- 业务侧 header。

**Tier 分级**（决策 D8）：
- **Tier1（同步落盘 fsync）**：401 `auth_failed`（攻击信号）、402 `insufficient_quota`（资金信号）、5xx（故障信号）。
- **Tier2（异步 stderr）**：2xx 成功 + 其他 4xx（400 / 413 / 429 / 上游 4xx 透传 —— 高频低安全意义，避免拖慢 fsync）。

Tier1 写失败时 `/readyz` 立刻返 503 摘流（与 admin audit 共享同一 sink 与关闸逻辑）。

---

## 7. 可观测性指标

业务调用相关 Prometheus 指标（运维 dashboard 用）：

| 指标 | 类型 | 标签 | 含义 |
|---|---|---|---|
| `gateway_relay_request_total` | Counter | `model`, `upstream_status` | 每次 relay 完成（含成功 / 上游 4xx/5xx / timeout / unreachable / malformed）|
| `gateway_relay_reserve_failed_total` | Counter | `reason` | Reserve 失败（`insufficient_balance` / `account_frozen` / …）|
| `gateway_relay_settle_failed_total` | Counter | `phase`, `reason` | Commit/Release 永久失败（orphan reserve 信号）|
| `gateway_relay_token_cost_minor_total` | Counter | `model` | 累计计费（minor）；对账用 |
| `gateway_relay_upstream_duration_seconds` | Histogram | `model`, `status` | 上游端到端耗时 |
| `gateway_relay_upstream_missing_usage_total` | Counter | `model` | 上游 200 缺 usage 兜底次数（provider 协议异常信号）|
| `gateway_business_api_auth_failed_total` | Counter | `reason` | 业务鉴权失败 |
| `gateway_business_api_rate_limited_total` | Counter | `key_id` | RPM 限速触发 |
| `gateway_business_api_body_too_large_total` | Counter | - | body 超 1 MiB 拒绝 |
| `gateway_business_throttle_rpm_cold_start_total` | Counter | `instance_id` | 业务 RPM 冷启（进程重启 +1）|

---

## 8. Quickstart — 业务系统首次接入

### 8.1 运维侧：创建 business key
```bash
# 1. 先有业务账户（admin-cli account create 或 Admin API）
./bin/admin-cli account create --id creator-platform-tenant-001

# 2. 创建 business key（明文一次性返回，写入 0600 文件）
./bin/admin-cli business-key create \
    --description "creator-platform-prod-key-1" \
    --business-account-id creator-platform-tenant-001 \
    --rpm 600 \
    --out /run/secrets/biz-key.txt

# 3. 通过密钥管理器（age / 1Password / Vault）交付业务系统；严禁 chat / wiki / email 粘贴

# 4. 充值（Admin API recharge）让账户有余额
```

### 8.2 业务系统侧：Python openai SDK

```python
from openai import OpenAI

client = OpenAI(
    base_url="https://gateway.example.com/v1",   # 网关 /v1 根路径
    api_key="biz-sk-xxx",                         # business key plaintext
)

try:
    resp = client.chat.completions.create(
        model="gw-default",                        # 业务可见 model 名
        messages=[{"role": "user", "content": "你好"}],
        max_tokens=1024,
        # stream=True 暂不支持
    )
    print(resp.choices[0].message.content)
    print("usage:", resp.usage)
except Exception as e:
    # SDK 会把 OpenAI 兼容错误解析为异常；402 = 余额不足，429 = 限速
    print("调用失败:", e)
```

### 8.3 业务系统侧：curl

```bash
curl -sS -X POST https://gateway.example.com/v1/chat/completions \
    -H "Authorization: Bearer biz-sk-xxx" \
    -H "Content-Type: application/json" \
    -d '{
      "model": "gw-default",
      "messages": [{"role": "user", "content": "你好"}],
      "max_tokens": 1024
    }'
```

### 8.4 错误处理模板（伪代码）

```python
def call_chat(messages, max_retries=3):
    for attempt in range(max_retries + 1):
        try:
            return client.chat.completions.create(
                model="gw-default", messages=messages, max_tokens=1024, timeout=65)
        except RateLimitError:                        # 429
            if attempt == max_retries: raise
            time.sleep(min(2 ** attempt, 60)); continue
        except APIStatusError as e:
            if e.status_code == 402: raise              # 余额不足 / 冻结：不重试，去充值
            if e.status_code == 503:                    # CAS 冲突：立即重试
                time.sleep(0.1 * (3 ** attempt)); continue
            if e.status_code in (502, 504):             # 上游不稳定：退避重试
                if attempt == max_retries: raise
                time.sleep(min(2 ** attempt, 60)); continue
            raise                                       # 400/401/413：不重试
```

---

## 9. 不支持的 OpenAI 能力（MVP）

| 能力 | MVP 状态 | 说明 |
|---|---|---|
| `stream=true`（SSE 流式）| ❌ 400 `streaming_not_supported` | P1+：边 stream 边累计 token 结算 |
| function calling / `tools` | ⚠️ 透传 | 网关不拦截；上游支持则可用，但计费仍按 usage |
| `response_format`（JSON mode）| ⚠️ 透传 | 同上 |
| embeddings / images / audio | ❌ 无路由 | MVP 仅 `/v1/chat/completions` |
| 多模型选择 | ❌ | MVP 单条字典；传任何 `model` 都路由到唯一上游模型 |
| 异步任务（视频生成等）| ❌ | P1+ 独立 task 接口 |

> ⚠️ "透传" 字段：网关原样转发上游，行为取决于上游 provider 是否支持；网关不保证语义。

---

## 10. 部署约束（业务系统接入前必读）

### 10.1 启用开关
`/v1` 路由仅在 `GATEWAY_RELAY_ENABLED=true` 时注册。需同时配置 8 个 `GATEWAY_RELAY_*` 字段（model 名 / 上游 base_url / 上游 key / 上游 model / input·output 单价 / max_context），任一非法或缺失 → 进程拒启动（fail-fast）。

### 10.2 TLS
- **上游连接**：production 模式强制上游 `base_url` 为 `https`（防明文上游连接泄露凭据；进程启动校验）。
- **业务连接**：与 Admin API 一致（进程自带 TLS 或前端反代终止 TLS）；所有 `/v1/*` 响应始终带 `Strict-Transport-Security` header。

### 10.3 RPM 限速精度
P0 单实例进程内计算（与 admin RPM 同实现模式）：
- 多实例部署时 RPM 按实例数倍放大（已知偏离）。
- 进程重启即清零（`gateway_business_throttle_rpm_cold_start_total{instance_id}` 可观测）。
- P1 接 Redis 后跨实例统一。

### 10.4 上游凭据
MVP 上游 API key 经 env 明文注入（`GATEWAY_RELAY_UPSTREAM_API_KEY`）；P1 升级为 envelope encryption（`GATEWAY_KEK_V*`）。env 文件权限须 0600，禁止入 VCS / log。

---

## 11. 变更与版本管理

| 变更类型 | 行为 |
|---|---|
| 新增字段（请求透传 / 响应透传）| ✅ 允许（OpenAI 新增字段自动透传）|
| 新增 `error.code` | ✅ 允许；业务遇未知 code 按 HTTP status 大类 + `error.type` 处理 |
| 新增 endpoint（embeddings 等）| ✅ 允许 |
| 修改 `error.type` / 字段语义 | ❌ 禁止；走 `/v2/*` |
| 修改 HTTP status 映射 | ❌ 禁止；走 `/v2/*` |
| 修改计费公式（涨价 / 改单价）| ⚠️ 改 env 字典即可（不算契约破坏，但应提前通知业务）|

**当前版本**：`v1`（Phase 2 F-min 首发）。

---

## 12. 相关文档

- 运维侧账户管理：[Admin API v1](admin-api.md)
- 实施计划：[Phase 2 F-min plan](../plans/2026-05-27-004-feat-workflow-f-min-openai-compat-relay-plan.md)
- Schema 演化：[docs/db/schema.md §0004 演化](../db/schema.md#0004-演化2026-05-27)
- 术语表：[CONTEXT.md](../../CONTEXT.md)
- 设计文档：[多媒体网关设计](../multimedia-gateway-design.md)

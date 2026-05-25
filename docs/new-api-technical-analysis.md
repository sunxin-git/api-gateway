# New API 项目技术实现分析报告

> 分析对象：`third-party/new-api/`
> 上游仓库：<https://github.com/QuantumNous/new-api>
> 分析时间：2026-05-25
> 分析依据：源码、`README.zh_CN.md`、`CLAUDE.md`、`AGENTS.md`、`go.mod`、两个前端 `package.json`、Dockerfile、`router/` 与 `relay/` 源码

---

## 一、项目定位

**New API** 是由 **QuantumNous** 维护的「新一代大模型网关与 AI 资产管理系统」，前身派生自 `Calcium-Ion/one-api`。它在企业 / 私域场景下解决两件事：

1. **统一接入** —— 将 40+ 个上游大模型厂商（OpenAI、Anthropic、Google、AWS、Azure、阿里、百度、腾讯、智谱、Moonshot、DeepSeek、xAI、Mistral、Cohere、Perplexity、Ollama、Replicate、SiliconFlow、Vertex、火山引擎、讯飞、Cloudflare、Coze、Dify、MiniMax、即梦、Suno、Midjourney 等）封装为一套**与 OpenAI / Claude / Gemini 兼容的对外 API**，下游客户端无需感知差异。
2. **AI 资产管理** —— 提供令牌（API Key）管理、用户与分组、配额 / 充值 / 计费 / 订阅、模型定价、数据看板、运营审计、合规与限流，以及一个面向管理员和最终用户的 Web 控制台。

定位上属于 **AI 网关 + 计费运营平台**，而不是单纯的反代。

---

## 二、整体架构

### 2.1 单体进程，分层组织

`main.go` 启动一个 Gin HTTP 服务，按下面的分层组织代码（与 `CLAUDE.md` 描述一致）：

```
Router ──► Controller ──► Service ──► Model (GORM)
                                ▲
                                └── Relay (上游厂商适配层)
```

| 目录 | 职责 |
|------|------|
| `router/` | HTTP 路由装配：`api-router.go`（管理 API）、`dashboard.go`（数据看板）、`relay-router.go`（中继）、`video-router.go`（视频任务）、`web-router.go`（静态前端） |
| `controller/` | 70+ 个文件的请求处理器：渠道、用户、令牌、计费、订阅、支付（易支付 / Stripe / Creem / Waffo / Pancake）、OAuth（GitHub / Discord / Linux.do / Telegram / WeChat / OIDC / 自定义）、Passkey、2FA、模型同步、Midjourney / 视频任务等 |
| `service/` | 业务逻辑：分发选择（`channel_select.go`、`channel_affinity.go`）、配额预扣 / 结算（`pre_consume_quota.go`、`tiered_settle.go`）、计费（`text_quota.go`、`tool_billing.go`、`task_billing.go`）、Codex OAuth 凭据自动刷新、订阅额度重置、Tokenizer、邮件 / Webhook 通知等 |
| `model/` | GORM 实体与数据访问：`user.go`、`channel.go`、`token.go`、`ability.go`、`option.go`、`log.go`、`subscription.go`、`pricing.go`、`twofa.go`、`passkey.go` 等，并带有 `channel_cache.go`、`user_cache.go`、`token_cache.go` 的内存缓存层 |
| `relay/` | **核心中继层**：`relay_adaptor.go` 是 API Type → Adaptor 的工厂；`relay/channel/<provider>/` 下是每家上游的适配器；`*_handler.go` 是按业务形态（chat / responses / claude / gemini / image / audio / embedding / rerank / realtime / mjproxy）拆分的入口处理器 |
| `middleware/` | 22 个中间件：`auth.go`（Token / User / Admin 多级鉴权）、`distributor.go`（渠道分发）、`rate-limit.go`、`model-rate-limit.go`、`cache.go`、`cors.go`、`gzip.go`、`i18n.go`、`turnstile-check.go`、`stats.go`、`logger.go` 等 |
| `setting/` | 运行时配置：`ratio_setting/`（倍率）、`model_setting/`（模型元数据）、`billing_setting/`（计费）、`system_setting/`、`performance_setting/`、`payment_*.go`、`reasoning/`（推理类专属配置）等，配合 `model/option.go` 在线热更新 |
| `common/` | 共享工具：JSON 包装（`json.go`，强制全项目通过此封装调用）、Redis、env、限流器、HTTP 工具、SSRF 防护（`ssrf_protection.go`、`url_validator.go`）、系统监控、自定义事件总线、`gopool` 等 |
| `dto/` | 请求 / 响应 DTO：`openai_request.go`、`openai_response.go`、`claude.go`、`gemini.go`、`openai_image.go`、`openai_video.go`、`realtime.go`、`midjourney.go`、`task.go` 等 |
| `constant/` | API Type、Channel Type、Context Key、Cache Key、任务类型、Finish Reason、Azure 端点等枚举常量 |
| `types/` | 中继格式枚举（`RelayFormatOpenAI`、`RelayFormatClaude`、`RelayFormatGemini`、`RelayFormatOpenAIResponses`、`RelayFormatOpenAIRealtime` 等）、文件源、错误类型 |
| `i18n/` | 后端国际化（`go-i18n/v2`），目前 `en` / `zh` 两套 |
| `oauth/` | OAuth Provider 抽象与注册表，内置 GitHub / Discord / Linux.do / OIDC，并支持运行时从数据库加载自定义 Provider |
| `pkg/` | 内部包：`cachex`、`ionet`、`perf_metrics`、`billingexpr`（基于 `expr-lang/expr` 的动态计费表达式） |
| `web/default/` 与 `web/classic/` | 两套前端主题（详见第六节） |

### 2.2 启动流程（`main.go`）

`InitResources()` 顺序：

1. `godotenv.Load(".env")` → `common.InitEnv()`
2. 日志、模型倍率（`ratio_setting`）、HTTP 客户端、Tokenizer 初始化
3. 主库 `model.InitDB()` → `CheckSetup()` → 选项映射 `InitOptionMap()`
4. 旧磁盘缓存清理、模型定价加载
5. 日志库 `InitLogDB()`（与主库可分离）
6. Redis 初始化
7. 启动 perf metrics、系统监控（`StartSystemMonitor`）
8. i18n、用户语言加载器、自定义 OAuth Provider 加载

`main()` 中并发拉起后台 Goroutine（统一通过 `bytedance/gopkg` 的 `gopool` 复用）：

- `model.SyncChannelCache` / `model.SyncOptions`：渠道与选项热更新
- `model.UpdateQuotaData`：看板配额聚合
- `controller.AutomaticallyUpdateChannels` / `AutomaticallyTestChannels`：渠道自动可用性测试
- `service.StartCodexCredentialAutoRefreshTask`：Codex OAuth 凭据每 10 分钟检查、过期前 1 天刷新
- `service.StartSubscriptionQuotaResetTask`：订阅额度按日 / 周 / 月 / 自定义重置
- `controller.StartChannelUpstreamModelUpdateTask`：上游模型清单巡检
- `controller.UpdateMidjourneyTaskBulk` / `UpdateTaskBulk`：异步任务回执拉取（仅主节点）
- 可选：`BATCH_UPDATE_ENABLED` 打开批量写入器、`ENABLE_PPROF` 打开 `:8005` 的 pprof、`StartPyroScope` 上报火焰图

Gin 引擎使用 `CustomRecovery` 兜底 panic、自定义 `RequestId`、`PoweredBy`、`I18n` 中间件，以及 `cookie` session 存储（30 天，`SameSiteStrictMode`）。`InjectUmamiAnalytics` / `InjectGoogleAnalytics` 在 embed 的 `index.html` 占位符里替换出统计脚本。

---

## 三、技术栈

### 3.1 后端

| 类别 | 选型 | 备注 |
|------|------|------|
| 语言 | **Go 1.25.1**（`go.mod`）；`heroku goVersion` 注释为 1.18 | `CLAUDE.md` 写 1.22+，以 `go.mod` 为准 |
| Web 框架 | `gin-gonic/gin` 1.9.1，配合 `gin-contrib`（cors、gzip、sessions、static） | |
| ORM | `gorm.io/gorm` v2 + MySQL / PostgreSQL / SQLite 驱动；`glebarez/sqlite`（纯 Go SQLite） | **强约束**三库同时兼容（`CLAUDE.md` Rule 2） |
| 缓存 | `go-redis/redis/v8` + 内存缓存；`samber/hot`（LFU/LRU），`samber/lo` / `samber/go-singleflightx` | Redis 开启时强制开启 memory cache 兼容旧版 |
| 鉴权 | `golang-jwt/jwt/v5`、`go-webauthn/webauthn`（Passkey）、`pquerna/otp`（TOTP 2FA）、自实现 OAuth | 详见第五节 |
| HTTP / 上游 | `aws-sdk-go-v2` + `bedrockruntime`、`gorilla/websocket`（Realtime）、`andybalholm/brotli` | |
| Tokenizer | `tiktoken-go/tokenizer` + 自实现 `service/tokenizer.go` / `token_estimator.go` | |
| 业务工具 | `tidwall/gjson` / `sjson`（JSON 流式处理）、`jinzhu/copier`、`shopspring/decimal`（精确额度计算）、`expr-lang/expr`（动态计费表达式）、`anknown/ahocorasick`（敏感词） | |
| 多媒体 | `abema/go-mp4`、`yapingcat/gomedia`、`go-audio/{wav,aiff}`、`mewkiz/flac`、`jfreymuth/oggvorbis`、`tcolgate/mp3`、`golang.org/x/image` | 用于音视频请求体的解析与计费 |
| 支付 | `Calcium-Ion/go-epay`（易支付）、`stripe-go/v81`、`waffo-com/waffo-go`（Waffo / Pancake）、Creem | |
| 国际化 | `nicksnyder/go-i18n/v2`，`en` / `zh` | |
| 观测 | `grafana/pyroscope-go`、`prometheus/client_golang`、`shirou/gopsutil`、`net/http/pprof` | 可选开启 |
| 并发 | `bytedance/gopkg/util/gopool`（协程复用池）、`golang.org/x/sync` | |
| 邮件 | 自实现 SMTP + Outlook OAuth（`common/email-outlook-auth.go`） | |
| 监控告警 | `Uptime-Kuma` Webhook 推送（`controller/uptime_kuma.go`） | |

### 3.2 配置与运行时

- 入口：`.env` → `common.InitEnv()`；运行时由 `model.SyncOptions` 周期性回写到内存。
- 关键环境变量（节选）：`PORT`、`GIN_MODE`、`SQL_DSN`、`LOG_SQL_DSN`、`REDIS_CONN_STRING`、`SESSION_SECRET`、`FRONTEND_BASE_URL`、`CHANNEL_UPDATE_FREQUENCY`、`BATCH_UPDATE_ENABLED`、`ENABLE_PPROF`、`UMAMI_WEBSITE_ID` / `UMAMI_SCRIPT_URL`、`GOOGLE_ANALYTICS_ID`。
- 多节点：`common.IsMasterNode` 控制只在主节点运行的后台任务（异步任务拉取、`FRONTEND_BASE_URL` 重定向忽略）。

---

## 四、中继（Relay）层 —— 核心实现

### 4.1 路由格式映射

`router/relay-router.go` 把 OpenAI / Claude / Gemini 三种生态的常见路径统一接入 `controller.Relay`，并通过 `types.RelayFormat*` 标识请求形态：

| 路径 | RelayFormat | 说明 |
|------|-------------|------|
| `POST /v1/chat/completions`、`POST /v1/completions`、`POST /v1/moderations` | `RelayFormatOpenAI` | OpenAI Chat 经典格式 |
| `POST /v1/responses`、`POST /v1/responses/compact` | `RelayFormatOpenAIResponses` / `…ResponsesCompaction` | OpenAI 新版 Responses API + 压缩变体 |
| `POST /v1/messages` | `RelayFormatClaude` | Anthropic 原生 messages 格式 |
| `POST /v1/images/generations`、`/v1/images/edits`、`/v1/edits` | `RelayFormatOpenAIImage` | 图片生成 / 编辑 |
| `POST /v1/audio/{transcriptions,translations,speech}` | `RelayFormatOpenAIAudio` | 语音转写 / 翻译 / TTS |
| `POST /v1/embeddings`、`POST /v1/engines/:model/embeddings` | `RelayFormatEmbedding` / `RelayFormatGemini` | 嵌入向量 |
| `POST /v1/rerank` | `RelayFormatRerank` | 重排序 |
| `GET /v1/realtime` (WebSocket) | `RelayFormatOpenAIRealtime` | 实时语音 / 多模态 |
| `POST /v1beta/models/*path` | `RelayFormatGemini` | Gemini 原生 API |
| `POST /mj/...`、`POST /:mode/mj/...` | Midjourney 异步任务 | 单独的 `RelayMidjourney` 控制器 |
| `POST /suno/submit/:action`、`POST /suno/fetch` | Suno 异步任务 | `RelayTask` 通用任务调度 |
| `POST /pg/chat/completions` | Playground | 控制台直连，走用户鉴权 |

`/v1/models` 还会按请求头智能切换：带 `x-api-key + anthropic-version` → 返回 Claude 渠道模型列表；带 `x-goog-api-key` 或 `?key=` → Gemini；否则 OpenAI。

### 4.2 适配器工厂

`relay/relay_adaptor.go` 的 `GetAdaptor(apiType int)` 是 `switch case` 工厂，按 `constant.APIType*` 分发到具体厂商的 `Adaptor`：

```
APITypeOpenAI / OpenRouter / Xinference / Submodel  → openai.Adaptor
APITypeAnthropic / Moonshot                         → claude.Adaptor / moonshot.Adaptor
APITypeGemini                                       → gemini.Adaptor
APITypeAws                                          → aws.Adaptor (Bedrock)
APITypeVertexAi                                     → vertex.Adaptor
APITypeAli / Baidu / BaiduV2 / Tencent / Xunfei /
Zhipu / ZhipuV4 / VolcEngine / DeepSeek / MiniMax /
Coze / Dify / Mistral / Cohere / Perplexity /
Ollama / Replicate / Cloudflare / SiliconFlow /
Xai / Jina / Jimeng / MokaAI / PaLM ...              → 各自 Adaptor
```

每个适配器目录内（如 `relay/channel/claude/`）通常包含：
- `adaptor.go`：实现 `channel.Adaptor` 接口（请求构造、URL 拼装、签名、发送、流式 / 非流式响应转换、配额清算）
- `relay.go` / `relay_*.go`：协议级转换细节
- `dto.go`：上游专属 DTO
- `constants.go`：模型清单、默认基址

**异步任务**渠道（视频 / 音乐 / 绘图）单独放在 `relay/channel/task/{ali,doubao,gemini,hailuo,jimeng,kling,sora,suno,vertex,vidu}`，通过 `service.GetTaskAdaptorFunc` 在 `main.go` 中以函数指针注入，避免 `service → relay` 的循环依赖。

### 4.3 中继处理流程（典型 chat 路径）

1. **中间件链**（`relay-router.go` 的 `relayV1Router`）：
   `CORS → DecompressRequestMiddleware → BodyStorageCleanup → StatsMiddleware → RouteTag("relay") → SystemPerformanceCheck → TokenAuth → ModelRequestRateLimit → Distribute`
2. **`TokenAuth`**：解析 `Authorization: Bearer <token>` 或 `x-api-key`，加载令牌、用户、分组、模型白名单。
3. **`ModelRequestRateLimit`**：按用户 / 模型 / 分组层级限流。
4. **`Distribute`** (`middleware/distributor.go`)：根据请求模型在可用渠道中选择一个（支持优先级、权重、亲和缓存 `service/channel_affinity.go`、自动拉黑），把渠道注入 context。
5. **`controller.Relay`**：根据 `types.RelayFormat*` 进入对应的 `*_handler.go`：构造上游请求 → 调用 Adaptor → 流式 SSE / WebSocket 透传或聚合 → 解析 usage → 计费结算（`service/pre_consume_quota.go` + `text_quota.go` + `tiered_settle.go`）→ 写日志。
6. **失败处理**：失败时通过 `service/error.go` 与 `relay/common/` 内的重试策略尝试切换渠道；持久化错误写入 `model/log.go`。

### 4.4 OpenAI / Claude / Responses 互转

- `relay/chat_completions_via_responses.go`：以 Responses API 实现 chat/completions 兼容。
- `relay/responses_handler.go` + `dto/openai_responses_compaction_request.go`：支持新的「压缩」请求形态。
- `service/openai_chat_responses_compat.go` / `openai_chat_responses_mode.go` / `convert.go`：在 OpenAI 经典 chat 与 Responses 之间互转，保证不同形态客户端都可命中同一上游。

### 4.5 计费体系

| 模块 | 作用 |
|------|------|
| `setting/ratio_setting/` | 模型倍率（输入 / 输出 / 缓存命中），可热更新 |
| `service/pre_consume_quota.go` | 请求前预扣额度 |
| `service/text_quota.go` / `tool_billing.go` / `task_billing.go` | 文本 / 工具调用 / 异步任务的成本核算 |
| `service/tiered_settle.go` | 分档（tiered）结算 |
| `pkg/billingexpr/` | 基于 `expr-lang/expr` 的**动态计费表达式**（参见 `pkg/billingexpr/expr.md`，`CLAUDE.md` Rule 7 要求改动前必读）；支持 `p` / `c` 自动排除、表达式版本化 |
| `service/funding_source.go` / `subscription_reset_task.go` | 资金源（充值 / 赠送 / 订阅）与订阅周期重置 |
| `service/violation_fee.go` | 违规扣费 |
| `controller/topup*.go` / `subscription_payment_*.go` | 充值与订阅支付：易支付、Stripe、Creem、Waffo / Pancake |

`shopspring/decimal` 全程用于额度精确计算，避免浮点漂移。

---

## 五、鉴权与安全

- **多层鉴权中间件**（`middleware/auth.go`）：未登录、Token、User、Admin、Root 五个层级。
- **认证方式**：
  - 邮箱 / 用户名 + 密码（带验证码、`turnstile-check.go` Cloudflare Turnstile 防机器人）
  - JWT
  - **WebAuthn / Passkey**（`go-webauthn/webauthn` + `model/passkey.go` + `controller/passkey.go`）
  - **2FA TOTP**（`pquerna/otp` + `controller/twofa.go`）
  - OAuth：GitHub / Discord / Linux.do / Telegram / WeChat / OIDC / 自定义（`oauth/registry.go` + `controller/custom_oauth.go`，运行时从 DB 加载）
- **二次安全验证**：`middleware/secure_verification.go` + `controller/secure_verification.go`，敏感操作（修改密码、绑定 OAuth、删除等）需重新校验。
- **SSRF 防护**：`common/ssrf_protection.go` + `url_validator.go`，限制上游 URL 解析以阻止内网穿透。
- **限流**：通用 `rate-limit.go`（IP / 全局）、`model-rate-limit.go`（模型粒度）、`email-verification-rate-limit.go`、`GlobalWebRateLimit`。
- **敏感词**：`setting/sensitive.go` + `service/sensitive.go` + Aho-Corasick 多模式匹配。
- **请求体清理**：`middleware/body_cleanup.go` 与 `common/body_storage.go` 处理大请求体（图片 / 音视频）的磁盘暂存与生命周期。
- **统一日志**：`middleware/request-id.go` 注入 trace id；`middleware/stats.go` 写请求统计。

---

## 六、前端 —— `default` 与 `classic` 的对比

`web/` 下并存两套前端，构建结果（`dist/`）被 Go 通过 `//go:embed` 一起打进可执行文件，运行时按 `common.GetTheme()` 选择主题：

```go
// main.go
//go:embed web/default/dist
var buildFS embed.FS
//go:embed web/default/dist/index.html
var indexPage []byte
//go:embed web/classic/dist
var classicBuildFS embed.FS
//go:embed web/classic/dist/index.html
var classicIndexPage []byte
```

```go
// router/web-router.go
if common.GetTheme() == "classic" {
    c.Data(http.StatusOK, "text/html; charset=utf-8", assets.ClassicIndexPage)
} else {
    c.Data(http.StatusOK, "text/html; charset=utf-8", assets.DefaultIndexPage)
}
```

### 6.1 主用判定：**主要使用 `web/default`**

依据：

| 维度 | default | classic | 结论 |
|------|---------|---------|------|
| `main.go` embed 顺序 | 第一组（`buildFS` / `indexPage`） | 第二组（带 `classic` 前缀） | default 是默认绑定 |
| `web-router.go` 默认分支 | `else` 分支即 default | 仅 `GetTheme()=="classic"` 时使用 | default 是默认主题 |
| `README.zh_CN.md` 引用资源 | `/web/default/public/logo.png` | 未引用 | default 是项目门面 |
| 项目根 `CLAUDE.md` 描述 | "Default frontend (React 19, Rsbuild, Base UI, Tailwind)" | "Classic frontend (React 18, Vite, Semi Design)" | default 是新版主题 |
| 前端 i18n 语言数 | en / zh / fr / ru / ja / vi（6 种） | i18next 23 + i18next-cli，语种较少 | default 持续扩展 |
| 是否有自己的 `AGENTS.md` 开发规范 | **有**（详细规范，第一节即「技术栈」） | 无 | default 是主开发分支 |
| 依赖现代性 | React 19、TanStack Router v1、Base UI 1.4、Tailwind 4 | React 18、react-router-dom v6、Semi Design 2 | default 是新一代 UI |
| 目录组织 | `features/` + `routes/`（基于文件的路由树 `routeTree.gen.ts`） | `pages/` 经典结构 | default 架构更新 |

`classic` 保留是为了向后兼容老用户的 UI 习惯（Semi Design 风格），并不是主开发目标。

### 6.2 `web/default`（主用，新一代主题）

**构建工具**：`@rsbuild/core` 2 + `@rsbuild/plugin-react`（基于 Rspack 的 Webpack 兼容打包），脚本通过 **Bun** 执行（`bunfig.toml`、`bun.lock`、Dockerfile 中 `RUN bun install` / `bun run build`）。

**技术栈**：

| 类别 | 依赖 |
|------|------|
| 框架 | React 19.2、React DOM 19.2、TypeScript 5.9 |
| 路由 | `@tanstack/react-router` 1.x（文件式路由，`routes/` 目录 + 自动生成 `routeTree.gen.ts`）、`@tanstack/router-plugin`、`@tanstack/react-router-devtools` |
| 状态 / 数据 | `@tanstack/react-query` 5、`zustand` 5、`axios` 1.13 |
| 表格 / 虚拟化 | `@tanstack/react-table` 8、`@tanstack/react-virtual` 3 |
| 表单 | `react-hook-form` 7 + `@hookform/resolvers` + `zod` 4 |
| UI | `@base-ui/react`（Radix 系，Headless）、`tailwindcss` 4 + `@tailwindcss/postcss`、`class-variance-authority`、`clsx`、`tailwind-merge`、`tw-animate-css`、`next-themes`（明暗主题）、`cmdk`、`vaul`、`input-otp`、`sonner`（toast）、`motion`（动画）、`react-resizable-panels`、`embla-carousel-react` |
| 图标 | `@hugeicons/react`、`lucide-react`、`react-icons`、`@lobehub/icons` |
| 图表 | `@visactor/react-vchart` 2 + `@visactor/vchart` 2、`recharts` 3、`@xyflow/react`（流程图） |
| 富文本 / Markdown | `react-markdown` 10、`remark-gfm`、`rehype-raw`、`shiki`（语法高亮）、`streamdown`、`auto-skeleton-react` |
| 流式 / SSE | `sse.js` 2、`ai` 6（Vercel AI SDK）、`use-stick-to-bottom` |
| i18n | `i18next` 25 + `react-i18next` 16 + `i18next-browser-languagedetector` 8，6 种语言 |
| 工程 | `eslint` 10、`prettier` 3、`knip` 6（死代码扫描）、`shadcn` 3（CLI）、`@trivago/prettier-plugin-sort-imports` |
| 时间 | `dayjs` 1.11、`date-fns` 4、`react-day-picker` 9 |
| 其它 | `qrcode.react`、`nanoid`、`tokenlens` |

**目录结构**：

```
web/default/src/
├── main.tsx              # 入口
├── routeTree.gen.ts      # TanStack Router 自动生成
├── routes/               # 文件式路由
│   ├── __root.tsx
│   ├── (auth)/  (errors)/ _authenticated/
│   ├── console/ oauth/ setup/ rankings/ pricing/
│   └── index.tsx privacy-policy.tsx user-agreement.tsx ...
├── features/             # 按业务域组织
│   ├── about auth channels chat dashboard errors home keys
│   ├── legal models performance-metrics playground pricing
│   ├── profile rankings redemption-codes setup subscriptions
│   ├── system-settings usage-logs users wallet
├── components/  hooks/  stores/  lib/  config/  context/  assets/  styles/
└── i18n/locales/{en,zh,fr,ru,ja,vi}.json
```

`AGENTS.md` 中明确的开发约束（节选）：
- 所有面向用户的文案必须 `useTranslation()`；非 React 环境用 `import { t } from 'i18next'`。
- 禁止 2 层及以上嵌套三元；禁止 `any`；不解构 `props`。
- Zustand 必须用选择器订阅（如 `useAuthStore((s) => s.auth.user)`）。
- 大列表使用 `@tanstack/react-virtual`；按需 `React.lazy` + 动态 `import`。
- 单文件 ~200 行考虑拆分。

### 6.3 `web/classic`（兼容主题）

**构建工具**：`vite` 5 + `@vitejs/plugin-react`，`@douyinfe/vite-plugin-semi`。

**技术栈**：

| 类别 | 依赖 |
|------|------|
| 框架 | React 18.2 + JavaScript（`jsconfig.json`，非 TS 强约束）、TypeScript 4.4（仅 devDep） |
| 路由 | `react-router-dom` 6 + `history` 5 |
| UI | `@douyinfe/semi-ui` 2.69 + `@douyinfe/semi-icons`、`tailwindcss` 3 |
| 图表 | `@visactor/react-vchart` 1.8 + `@visactor/vchart` 1.8 + `vchart-semi-theme` |
| Markdown / 数学公式 | `react-markdown`、`marked` 4、`mermaid` 11、`katex` + `rehype-katex` + `remark-math`、`rehype-highlight`、`remark-breaks` |
| 文件上传 | `react-dropzone` 14 |
| 富交互 | `react-toastify` 9、`react-fireworks`、`react-telegram-login`、`react-turnstile` |
| SSE / 工具 | `sse.js` 2、`use-debounce` 10、`unist-util-visit` |
| i18n | `i18next` 23 + `react-i18next` 13 + `i18next-cli` 1.10（脚本：`i18n:extract` / `status` / `sync` / `lint`） |
| 代理 | `package.json` 内 `"proxy": "http://localhost:3000"`（开发时直接代理到后端） |

**目录结构**：

```
web/classic/src/
├── App.jsx  index.jsx  index.css
├── pages/        # 经典 react-router 页面
├── components/  context/  contexts/  helpers/  hooks/
├── constants/   services/   i18n/
```

### 6.4 一句话总结

> **`web/default` 是当前主用、积极迭代、面向最终用户的现代化前端**（React 19 + TypeScript + Rsbuild + TanStack 全家桶 + Base UI + Tailwind 4 + 6 语言），**`web/classic` 是保留给老用户的 Semi Design 经典主题**（React 18 + Vite + JS），二者并行编译并由后端按 `Theme` 配置切换。

---

## 七、部署与发布

### 7.1 Docker 多阶段构建

`Dockerfile` 用三阶段交叉构建，最终镜像极小且无构建残留：

1. **`builder`**（`oven/bun:1`）：构建 `web/default`，注入 `VITE_REACT_APP_VERSION`。
2. **`builder-classic`**（`oven/bun:1`）：构建 `web/classic`。
3. **`builder2`**（`golang:1.26.1-alpine`，`CGO_ENABLED=0`，`GOEXPERIMENT=greenteagc`）：拉入两份 `dist`，`go build -ldflags "-s -w -X 'github.com/QuantumNous/new-api/common.Version=$(cat VERSION)'"` 生成单一二进制。
4. **运行镜像**（`debian:bookworm-slim`）：仅复制二进制与 `ca-certificates`、`tzdata`、`libasan8`、`wget`，`EXPOSE 3000`，`ENTRYPOINT ["/new-api"]`，工作目录 `/data`（持久化挂载）。

### 7.2 推荐部署方式

`README.zh_CN.md` 给出的部署矩阵（节选）：

```bash
# Docker Compose（推荐）
docker-compose up -d

# SQLite（最简）
docker run --name new-api -d --restart always \
  -p 3000:3000 -e TZ=Asia/Shanghai -v ./data:/data \
  calciumion/new-api:latest

# MySQL
docker run ... -e SQL_DSN="root:123456@tcp(localhost:3306)/oneapi" ...
```

另带 `new-api.service` systemd 单元、`Dockerfile.dev`、`docker-compose.dev.yml`、`electron/`（桌面端壳）目录可选。

### 7.3 数据库兼容（`CLAUDE.md` Rule 2）

代码必须**同时**兼容 SQLite / MySQL ≥ 5.7.8 / PostgreSQL ≥ 9.6：

- 全部走 GORM 抽象；保留字列 `group`、`key` 用 `commonGroupCol`、`commonKeyCol`；布尔值用 `commonTrueVal` / `commonFalseVal`。
- 三种数据库各自的特性函数（`GROUP_CONCAT` / `STRING_AGG` / JSONB 操作符 / `ALTER COLUMN`）必须有跨库 fallback；JSON 列统一用 `TEXT`。
- 通过 `common.UsingPostgreSQL` / `UsingSQLite` / `UsingMySQL` 分支。

### 7.4 JSON 规范（`CLAUDE.md` Rule 1）

所有 JSON 序列化必须通过 `common.Marshal` / `Unmarshal` / `UnmarshalJsonStr` / `DecodeJson` / `GetJsonType`，**不允许**业务代码直接 import `encoding/json`（类型如 `json.RawMessage` 例外），以便后续替换为更快的实现。

### 7.5 上游 DTO 零值（`CLAUDE.md` Rule 6）

向上游转发的请求 DTO，所有可选标量必须用**指针 + `omitempty`**（`*int`、`*float64`、`*bool`），区分「字段缺失」（`nil`，省略）与「显式为 0 / false」（非 nil，照发）。直接用值类型 + `omitempty` 会丢失 `0` / `false` / `""`，影响上游语义。

---

## 八、关键能力一览

| 能力 | 实现位置 |
|------|----------|
| 多模型统一接口（OpenAI / Claude / Gemini） | `router/relay-router.go` + `types/RelayFormat*` + `relay/*_handler.go` |
| 40+ 上游厂商适配 | `relay/channel/<provider>/` + `relay/relay_adaptor.go` |
| 渠道分发与亲和性 | `middleware/distributor.go` + `service/channel_select.go` + `service/channel_affinity.go` |
| 渠道健康检测 | `controller/channel-test.go` + `AutomaticallyTestChannels` |
| 上游模型自动同步 | `controller/channel_upstream_update.go` + `controller/model_sync.go` |
| 令牌 / 分组 / 模型限制 | `model/token.go` + `model/ability.go` + `middleware/auth.go` |
| 配额预扣 / 结算 / 动态计费 | `service/pre_consume_quota.go` + `service/text_quota.go` + `service/tiered_settle.go` + `pkg/billingexpr/` |
| 充值与订阅 | `controller/topup*.go` + `subscription_*.go` + `controller/subscription_payment_*.go` |
| 数据看板 | `router/dashboard.go` + `model/usedata*.go` + `controller/usedata.go` + `model.UpdateQuotaData` |
| 用户体系（注册 / OAuth / Passkey / 2FA / Turnstile） | `controller/{user,oauth,passkey,twofa,secure_verification}.go` + `middleware/turnstile-check.go` |
| 异步任务（Midjourney / Suno / 视频） | `relay/relay_task.go` + `relay/channel/task/*` + `controller/{midjourney,task,task_video,swag_video}.go` |
| 实时（Realtime）WebSocket | `relay/websocket.go` + `relay-router.go` 的 `wsRouter` |
| 多语言 | 后端 `i18n/`（en/zh）+ 前端 `web/default/src/i18n/locales`（en/zh/fr/ru/ja/vi） |
| 性能监控 | `pkg/perf_metrics/` + Pyroscope + Prometheus + pprof + `system_monitor*.go` |
| 通知与告警 | `service/{user_notify,notify-limit,webhook}.go` + Uptime Kuma |
| 合规与敏感词 | `controller/payment_compliance.go` + `service/sensitive.go` + Aho-Corasick |

---

## 九、特别注意事项

1. **`CLAUDE.md` Rule 5** 把项目名 `nеw-аρi` 与组织名 `QuаntumΝоuѕ`（注意上游使用了同形异码字符）列为**受保护信息**，禁止任何形式的删改、改名、替换。基于此项目二次开发或派生时务必保留 README、LICENSE、模块路径、Docker 镜像名、HTML 标题等位置的署名。
2. **`CLAUDE.md` Rule 7** 要求修改计费表达式（`pkg/billingexpr/`）前必须先读 `pkg/billingexpr/expr.md`，理解表达式语言、`p` / `c` token 排除规则、表达式版本化等设计。
3. **`CLAUDE.md` Rule 4**：新增渠道时确认上游是否支持 `StreamOptions`，支持的话要加进 `streamSupportedChannels`。
4. **前端包管理器**：统一使用 **Bun**，不要混用 npm / yarn / pnpm（Dockerfile、CI 与本地脚本均依赖 `bun.lock` / `bunfig.toml`）。
5. **前端主题切换**：`FRONTEND_BASE_URL` 环境变量可把所有未匹配的请求 301 重定向到外部前端域名（适合前后端分离部署），主节点上自动忽略。
6. **桌面端壳**：`electron/` 目录提供 Electron 打包入口（README 暂未重点宣传，按需启用）。
7. **CGO 关闭 + GreenTea GC 实验**：默认镜像用 `CGO_ENABLED=0` 编 alpine 静态二进制，并启用 `GOEXPERIMENT=greenteagc` 试验 Go 新 GC。SQLite 走纯 Go 实现（`glebarez/sqlite`），无 cgo 依赖。

---

## 十、结论

New API 是一个**功能完整、工程化程度较高的企业级 AI 网关**：

- **后端**用 Go + Gin + GORM 实现清晰的分层架构，中继层通过 Adaptor 工厂统一了 40+ 厂商和 8+ 种 API 形态（chat / responses / claude / gemini / image / audio / embedding / rerank / realtime / midjourney / suno / video task），并配套了完整的用户、令牌、分组、配额、订阅、支付、动态计费、多源 OAuth、Passkey、2FA、限流、敏感词、SSRF 防护、观测体系。
- **前端**双主题并存：**`web/default` 是当前主用的现代化主题**（React 19 + TypeScript + Rsbuild + TanStack Router/Query/Table/Virtual + Base UI + Tailwind 4 + i18next 25 + Zustand 5，6 语言），**`web/classic` 是为兼容老用户保留的 Semi Design 主题**（React 18 + Vite + JS）。两者构建产物都通过 `//go:embed` 打入二进制，按服务端 `Theme` 配置切换。
- **部署**以 Docker Compose 为主，多数据库（SQLite / MySQL / PostgreSQL）通吃，可选 Redis、Pyroscope、Prometheus、pprof、Umami / GA。
- **强约束**集中体现在项目根 `CLAUDE.md`：JSON 必经包装、跨库兼容、`omitempty` 指针化、品牌信息保护、计费表达式必读设计文档等。

对二次开发者而言，**入口建议**：从 `main.go` → `router/` → `controller/relay.go` → `relay/relay_adaptor.go` → 目标 `relay/channel/<provider>/adaptor.go` 沿调用链阅读，配合 `service/` 与 `model/` 理解配额与渠道；前端则以 `web/default/src/routes/` 与 `features/` 为主，遵守 `web/default/AGENTS.md` 的规范。

---

*报告生成于 2026-05-25，依据当前 `third-party/new-api/` 中的源码快照与 `VERSION` 文件。上游持续迭代，部分能力（如新增渠道、计费策略）可能后续变化，以仓库最新代码为准。*

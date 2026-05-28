// Package relay 实现"业务系统 → 网关 → 上游 provider"的核心 relay 流程。
//
// 计划：docs/plans/2026-05-27-004-feat-workflow-f-min-openai-compat-relay-plan.md Unit 3 + 5
// 设计文档：docs/multimedia-gateway-design.md §9
//
// 包内职责（Unit 3 范围）：
//   - ModelEntry / Catalog：网关 model 字典（业务可见名 → 上游 provider / model / pricing 映射）
//   - ProviderAdapter interface + OpenAICompatAdapter 实现
//   - 错误 sentinel（timeout / unreachable / malformed / missing_usage）+ OpenAI 兼容错误响应格式
//
// 包内职责（Unit 5 范围，后续 commit）：
//   - RelayHandler：Reserve → Relay → Settle 流程
//   - token 估算（input_tokens / output reserve）
//
// 与 admin handler 的关系：
//   - 业务请求路径 `POST /v1/chat/completions` 平级于 `POST /admin/v1/*`
//   - 共用 LedgerService（Reserve/Commit/Release）+ audit.Logger
//   - 业务侧 KeyAuth 依赖 internal/businesskey（与 admintoken 同源 pepper，F-min D4）
package relay

import "time"

// ModelEntry 网关 model 字典单条记录（plan §决策 D11 锁定字段集）。
//
// MVP env 硬编码 1 条；P1+ 升级到 YAML 文件 / DB 表（plan §Scope Boundaries）。
// 字段语义：业务系统看到 GatewayModelName；网关查字典改写为 UpstreamModelName +
// 注入 UpstreamBaseURL + UpstreamAPIKey；usage 按 PriceInput/Output 计费。
type ModelEntry struct {
	// GatewayModelName 业务可见 model 名（如 "gw-default" / "gw-fast"）。
	// MVP 业务请求中传任何 model 都路由到字典唯一条，但响应仍透传上游真实 model 名。
	GatewayModelName string

	// UpstreamProviderType provider 协议簇标识；MVP 唯一合法值 "openai_compat"。
	// P1+ 扩展时新增枚举值（如 "volcano_async" / "anthropic_native"）。
	UpstreamProviderType string

	// UpstreamBaseURL 上游 base URL（不含 endpoint path）。
	// 例：火山 ARK = "https://ark.cn-beijing.volces.com/api/v3"
	UpstreamBaseURL string

	// UpstreamAPIKey 上游凭据（MVP env 明文；P1 envelope encryption 加 KEK_V1）。
	// **绝不**记 log；adapter 注入到 Authorization header 后即丢弃引用。
	UpstreamAPIKey string

	// UpstreamModelName 上游真实 model 名（如 "doubao-1-5-pro-32k-250115"）。
	UpstreamModelName string

	// PriceInputPer1MMinor input token 单价（每 1M token，单位 minor / CNY 分）。
	// 例：豆包 1.5 pro 32k input ¥8/M → 800。
	PriceInputPer1MMinor int64

	// PriceOutputPer1MMinor output token 单价（同上）。
	PriceOutputPer1MMinor int64

	// MaxContextTokens 字典默认 max（业务请求可传更小 max_tokens；不可超此值）。
	MaxContextTokens int32
}

// Usage 上游响应中 usage 字段的强类型视图（OpenAI 兼容 schema）。
//
// 字段命名 snake_case 与 OpenAI 协议一致（JSON tag）；网关侧业务逻辑读 PromptTokens
// / CompletionTokens 算 actual_cost。TotalTokens 透传不算（避免双计）。
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// UpstreamResponse adapter 返给 handler 的结构化响应（plan §决策 D2 + D3）。
//
// 设计：
//   - StatusCode：上游 HTTP 状态码（透传给业务时直接 c.Status(StatusCode)）
//   - Body：上游响应 body 字节流（透传给业务 c.Writer.Write(Body)）
//   - Usage：仅 StatusCode==200 + JSON 解析成功 + 含 usage 字段时填充；
//     其他情况（4xx/5xx / 缺 usage）为 nil，handler 按 plan §决策 D2 分流处理
type UpstreamResponse struct {
	StatusCode int
	Body       []byte
	Usage      *Usage

	// Duration 上游请求耗时（监控用；handler 写入 audit + metric）
	Duration time.Duration
}

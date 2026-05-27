package relay

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/sunxin-git/api-gateway/internal/httpapi/middleware"
)

// Sentinel errors —— 包对外契约。
//
// 调用方用 errors.Is 判断；error 链可用 fmt.Errorf("...: %w", Err...) 包装。
//
// 设计选择（plan §决策 D2）：
//   - 上游 4xx/5xx **不**作为 error 返回，而是放进 UpstreamResponse.StatusCode 让 handler 透传决策
//   - 仅"网络层 / 协议层"异常（timeout / unreachable / 上游 200 但 body 非 JSON）作 error
var (
	// ErrUpstreamTimeout 上游请求超过客户端 timeout（plan §决策 D10：60s 总超时）。
	// handler 映射：504 + OpenAI 兼容 `type: upstream_timeout`。
	ErrUpstreamTimeout = errors.New("upstream request timeout")

	// ErrUpstreamUnreachable 上游连接拒绝 / DNS 失败 / TLS 失败 / 网络分区。
	// handler 映射：502 + OpenAI 兼容 `type: upstream_error, code: upstream_unreachable`。
	ErrUpstreamUnreachable = errors.New("upstream connection failed")

	// ErrUpstreamMalformed 上游返 200 但 body 非合法 JSON / 缺关键字段。
	// 视作 provider bug；handler 兜底 Release reserve + 502。
	ErrUpstreamMalformed = errors.New("upstream response malformed")
)

// =============================================================================
// OpenAI 兼容错误响应（plan §决策 D9）
// =============================================================================

// ErrorBody 错误响应里层；JSON tag 严格遵循 OpenAI 协议字段命名。
//
// 业务方 SDK（openai-python / openai-node）依赖此 shape 解析报错；任何字段重命名
// 都会破坏业务方接入，需走 v2 路径。
type ErrorBody struct {
	// Message 中文人类可读错误描述
	Message string `json:"message"`
	// Type 错误大类（OpenAI 协议：invalid_request_error / invalid_api_key /
	// insufficient_quota / rate_limit_exceeded / upstream_error / upstream_timeout /
	// server_error / api_error）
	Type string `json:"type"`
	// Code 细粒度错误码（如 invalid_api_key / streaming_not_supported / account_frozen /
	// upstream_5xx / temporarily_unavailable）
	Code string `json:"code"`
	// RequestID 网关分配的请求 ID（与响应 header X-Request-Id 一致）；OpenAI 也有此字段
	RequestID string `json:"request_id,omitempty"`
}

// ErrorResponse 错误响应外层；JSON shape 与 OpenAI 协议一致 `{"error": {...}}`。
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

// WriteErrorJSON 写 OpenAI 兼容错误响应 + 中止后续 middleware / handler 执行。
//
// 入参：
//   - c：gin context（取 request_id 用 middleware.GetRequestID）
//   - status：HTTP status code（plan §决策 D9 完整映射表）
//   - errType / code / message：直接填 ErrorBody 字段
//
// 副作用：c.AbortWithStatusJSON → 后续 middleware / handler 被跳过；
// audit middleware（defer 模式）仍 emit record 含 status。
func WriteErrorJSON(c *gin.Context, status int, errType, code, message string) {
	c.AbortWithStatusJSON(status, ErrorResponse{
		Error: ErrorBody{
			Type:      errType,
			Code:      code,
			Message:   message,
			RequestID: middleware.GetRequestID(c),
		},
	})
}

// =============================================================================
// 错误类型常量（OpenAI 协议 type 字段值）
// =============================================================================

const (
	// ErrTypeInvalidRequest 入参问题（缺字段 / 越界 / stream=true 等）。
	ErrTypeInvalidRequest = "invalid_request_error"
	// ErrTypeInvalidAPIKey 鉴权失败（含未知 / revoked / 账户不存在）。
	ErrTypeInvalidAPIKey = "invalid_api_key"
	// ErrTypeInsufficientQuota 余额不足 / 账户冻结 / 阀门拒。
	ErrTypeInsufficientQuota = "insufficient_quota"
	// ErrTypeRateLimitExceeded RPM / 其他限速触发。
	ErrTypeRateLimitExceeded = "rate_limit_exceeded"
	// ErrTypeUpstreamError 上游 5xx / 连接失败。
	ErrTypeUpstreamError = "upstream_error"
	// ErrTypeUpstreamTimeout 上游超时（与 OpenAI 协议中扩展类型一致）。
	ErrTypeUpstreamTimeout = "upstream_timeout"
	// ErrTypeServerError CAS 冲突 / 暂时不可用。
	ErrTypeServerError = "server_error"
	// ErrTypeAPIError 网关内部异常（panic / DB 断）。
	ErrTypeAPIError = "api_error"
)

// 编译期断言常量未被误删（防 unused 误判）。
var _ = http.StatusOK

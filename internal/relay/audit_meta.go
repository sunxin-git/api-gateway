package relay

import (
	"time"

	"github.com/gin-gonic/gin"
)

// =============================================================================
// audit ctx helpers — handler 注入业务 audit 元数据
// =============================================================================
//
// RelayHandler 在请求流程中调用 Set* helper 注入 input/output tokens / cost /
// upstream_status 等业务字段；middleware.BusinessAudit defer 时通过 Get* helper
// 统一读 ctx 组装 AuditRecord 写入。
//
// 设计选择：放在 relay 包而非 middleware 包，避免循环依赖
// （middleware → relay 已是 D4 取错误响应工具的方向；relay 反向 import middleware
// 会形成循环）。
//
// 设计选择：分散 Set helper（而非单一 struct）让 handler 在 Reserve → Relay →
// Settle 各阶段渐进注入数据，与流程节奏对齐。

const (
	ctxKeyAuditOutcome          = "business_audit_outcome"
	ctxKeyAuditInputTokens      = "business_audit_input_tokens"
	ctxKeyAuditOutputTokens     = "business_audit_output_tokens"
	ctxKeyAuditCostMinor        = "business_audit_cost_minor"
	ctxKeyAuditGatewayModel     = "business_audit_gw_model"
	ctxKeyAuditUpstreamModel    = "business_audit_up_model"
	ctxKeyAuditUpstreamStatus   = "business_audit_up_status"
	ctxKeyAuditUpstreamDuration = "business_audit_up_duration_ms"
)

// =============================================================================
// Setters（由 RelayHandler 调用）
// =============================================================================

// SetBusinessAuditOutcomeCode 注入业务级 outcome code（如 "ok" /
// "insufficient_quota" / "upstream_5xx" / "streaming_not_supported"）。
// audit middleware defer 时读；缺失时按 HTTP status 推断。
func SetBusinessAuditOutcomeCode(c *gin.Context, code string) {
	c.Set(ctxKeyAuditOutcome, code)
}

// SetBusinessAuditTokens 注入上游返回的 input/output tokens。
func SetBusinessAuditTokens(c *gin.Context, input, output int) {
	c.Set(ctxKeyAuditInputTokens, input)
	c.Set(ctxKeyAuditOutputTokens, output)
}

// SetBusinessAuditCost 注入本请求实际 commit 的 cost（minor unit）。
func SetBusinessAuditCost(c *gin.Context, costMinor int64) {
	c.Set(ctxKeyAuditCostMinor, costMinor)
}

// SetBusinessAuditModelInfo 注入网关 model + 上游真实 model 名（路由日志用）。
func SetBusinessAuditModelInfo(c *gin.Context, gatewayModel, upstreamModel string) {
	c.Set(ctxKeyAuditGatewayModel, gatewayModel)
	c.Set(ctxKeyAuditUpstreamModel, upstreamModel)
}

// SetBusinessAuditUpstreamResult 注入上游 HTTP status + 耗时（监控用）。
func SetBusinessAuditUpstreamResult(c *gin.Context, status int, duration time.Duration) {
	c.Set(ctxKeyAuditUpstreamStatus, status)
	c.Set(ctxKeyAuditUpstreamDuration, duration.Milliseconds())
}

// =============================================================================
// Getters（由 middleware.BusinessAudit 调用）
// =============================================================================

// GetBusinessAuditOutcomeCode 读 handler 注入的 outcome；缺失返空串。
func GetBusinessAuditOutcomeCode(c *gin.Context) string {
	if v, ok := c.Get(ctxKeyAuditOutcome); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// GetBusinessAuditInputTokens 读 input tokens；缺失返 0。
func GetBusinessAuditInputTokens(c *gin.Context) int {
	if v, ok := c.Get(ctxKeyAuditInputTokens); ok {
		if n, ok := v.(int); ok {
			return n
		}
	}
	return 0
}

// GetBusinessAuditOutputTokens 读 output tokens；缺失返 0。
func GetBusinessAuditOutputTokens(c *gin.Context) int {
	if v, ok := c.Get(ctxKeyAuditOutputTokens); ok {
		if n, ok := v.(int); ok {
			return n
		}
	}
	return 0
}

// GetBusinessAuditCostMinor 读 cost；缺失返 0。
func GetBusinessAuditCostMinor(c *gin.Context) int64 {
	if v, ok := c.Get(ctxKeyAuditCostMinor); ok {
		if n, ok := v.(int64); ok {
			return n
		}
	}
	return 0
}

// GetBusinessAuditGatewayModel 读网关 model；缺失返空串。
func GetBusinessAuditGatewayModel(c *gin.Context) string {
	if v, ok := c.Get(ctxKeyAuditGatewayModel); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// GetBusinessAuditUpstreamModel 读上游 model；缺失返空串。
func GetBusinessAuditUpstreamModel(c *gin.Context) string {
	if v, ok := c.Get(ctxKeyAuditUpstreamModel); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// GetBusinessAuditUpstreamStatus 读上游 HTTP status；缺失返 0。
func GetBusinessAuditUpstreamStatus(c *gin.Context) int {
	if v, ok := c.Get(ctxKeyAuditUpstreamStatus); ok {
		if n, ok := v.(int); ok {
			return n
		}
	}
	return 0
}

// GetBusinessAuditUpstreamDurationMs 读上游耗时 ms；缺失返 0。
func GetBusinessAuditUpstreamDurationMs(c *gin.Context) int64 {
	if v, ok := c.Get(ctxKeyAuditUpstreamDuration); ok {
		if n, ok := v.(int64); ok {
			return n
		}
	}
	return 0
}

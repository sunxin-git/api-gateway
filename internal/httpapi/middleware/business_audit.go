package middleware

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/sunxin-git/api-gateway/internal/audit"
)

// =============================================================================
// ctx helpers — handler 注入业务 audit 元数据
// =============================================================================
//
// RelayHandler 在请求流程中调用这些 Set* helper 注入 input/output tokens / cost /
// upstream_status 等业务字段；BusinessAudit middleware defer 时统一读 ctx 组装
// AuditRecord 写入。
//
// 设计选择：分散的 Set helper（而非单一 struct）让 handler 在 Reserve → Relay →
// Settle 各阶段渐进注入数据，与流程节奏对齐。

const (
	ctxKeyBusinessAuditOutcome          = "business_audit_outcome"
	ctxKeyBusinessAuditInputTokens      = "business_audit_input_tokens"
	ctxKeyBusinessAuditOutputTokens     = "business_audit_output_tokens"
	ctxKeyBusinessAuditCostMinor        = "business_audit_cost_minor"
	ctxKeyBusinessAuditGatewayModel     = "business_audit_gw_model"
	ctxKeyBusinessAuditUpstreamModel    = "business_audit_up_model"
	ctxKeyBusinessAuditUpstreamStatus   = "business_audit_up_status"
	ctxKeyBusinessAuditUpstreamDuration = "business_audit_up_duration_ms"
)

// SetBusinessAuditOutcomeCode handler 调用注入业务级 outcome code（如 "ok" /
// "insufficient_quota" / "upstream_5xx" / "streaming_not_supported"）。
// audit middleware defer 时读；缺失时按 HTTP status 推断（"ok" / "client_error" / "internal_error"）。
func SetBusinessAuditOutcomeCode(c *gin.Context, code string) {
	c.Set(ctxKeyBusinessAuditOutcome, code)
}

// SetBusinessAuditTokens 注入上游返回的 input/output tokens（用于 audit + metric）。
func SetBusinessAuditTokens(c *gin.Context, input, output int) {
	c.Set(ctxKeyBusinessAuditInputTokens, input)
	c.Set(ctxKeyBusinessAuditOutputTokens, output)
}

// SetBusinessAuditCost 注入本请求实际 commit 的 cost（minor unit）。
func SetBusinessAuditCost(c *gin.Context, costMinor int64) {
	c.Set(ctxKeyBusinessAuditCostMinor, costMinor)
}

// SetBusinessAuditModelInfo 注入网关 model + 上游真实 model 名（路由日志用）。
func SetBusinessAuditModelInfo(c *gin.Context, gatewayModel, upstreamModel string) {
	c.Set(ctxKeyBusinessAuditGatewayModel, gatewayModel)
	c.Set(ctxKeyBusinessAuditUpstreamModel, upstreamModel)
}

// SetBusinessAuditUpstreamResult 注入上游 HTTP status + 耗时（监控用）。
func SetBusinessAuditUpstreamResult(c *gin.Context, status int, duration time.Duration) {
	c.Set(ctxKeyBusinessAuditUpstreamStatus, status)
	c.Set(ctxKeyBusinessAuditUpstreamDuration, duration.Milliseconds())
}

// =============================================================================
// BusinessAudit middleware
// =============================================================================

// BusinessAudit 业务 relay 审计中间件（plan §Unit 4 + R8 + 决策 D8）。
//
// 职责：
//
//  1. defer 模式：c.Next 前注册 defer 让 handler panic / abort / 正常返回均能 emit
//  2. recover panic：emit Tier1 record (status=500) + re-panic 让 Recover middleware 写 500
//  3. 按 status 决定 Tier（D8）：401 / 402 / 5xx → Tier1（攻击 / 资金 / 故障信号）；
//     其他 4xx → Tier2（恶意业务方刷 400 不应拖慢 fsync）；2xx → Tier2
//  4. **不**记 messages body（PII / prompt 敏感）；从 ctx helpers 读 handler 注入的
//     input/output_tokens / cost_minor / upstream_status 等
//  5. Tier1 sink 写失败 → bump auditWriteFailedCounter（让 /readyz 关闸）
//
// 必须放在业务链最后一个 middleware（handler 之前）；defer 顺序保证 audit 行最终一定 emit。
func BusinessAudit(
	logger audit.AuditLogger,
	auditWriteFailedCounter *prometheus.CounterVec,
) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()

		defer func() {
			panicVal := recover()
			duration := time.Since(start)
			status := c.Writer.Status()
			if panicVal != nil {
				status = http.StatusInternalServerError
			}

			path := c.FullPath()
			if path == "" {
				path = c.Request.URL.Path
			}
			rec := audit.AuditRecord{
				Event:        "business_relay",
				RequestID:    GetRequestID(c),
				TimestampUTC: time.Now().UTC(),
				SourceIP:     c.ClientIP(),
				Method:       c.Request.Method,
				Path:         path,
				Status:       status,
				DurationMs:   duration.Milliseconds(),
				OutcomeCode:  resolveBusinessOutcomeCode(c, status),
			}

			// 注入业务身份
			if vr := GetBusinessKeyValidation(c); vr != nil && vr.Key != nil {
				rec.BusinessAccountID = vr.Key.BusinessAccountID
				rec.APIKeyID = vr.Key.ID
				rec.Actor = "business_key:" + strconv.FormatInt(vr.Key.ID, 10)
			} else {
				rec.Actor = "anonymous"
			}

			// 注入 handler 元数据（Set*helpers）
			if v, ok := c.Get(ctxKeyBusinessAuditInputTokens); ok {
				if n, ok := v.(int); ok {
					rec.InputTokens = n
				}
			}
			if v, ok := c.Get(ctxKeyBusinessAuditOutputTokens); ok {
				if n, ok := v.(int); ok {
					rec.OutputTokens = n
				}
			}
			if v, ok := c.Get(ctxKeyBusinessAuditCostMinor); ok {
				if n, ok := v.(int64); ok {
					rec.CostMinor = n
				}
			}
			if v, ok := c.Get(ctxKeyBusinessAuditGatewayModel); ok {
				if s, ok := v.(string); ok {
					rec.GatewayModel = s
				}
			}
			if v, ok := c.Get(ctxKeyBusinessAuditUpstreamModel); ok {
				if s, ok := v.(string); ok {
					rec.UpstreamModel = s
				}
			}
			if v, ok := c.Get(ctxKeyBusinessAuditUpstreamStatus); ok {
				if n, ok := v.(int); ok {
					rec.UpstreamStatus = n
				}
			}
			if v, ok := c.Get(ctxKeyBusinessAuditUpstreamDuration); ok {
				if n, ok := v.(int64); ok {
					rec.UpstreamDurationMs = n
				}
			}

			rec.Tier = resolveBusinessTier(status)

			if err := logger.Emit(c.Request.Context(), rec); err != nil {
				if auditWriteFailedCounter != nil {
					auditWriteFailedCounter.WithLabelValues(auditTierLabel(rec.Tier), "emit_error").Inc()
				}
				_ = c.Error(err)
			}

			// re-panic：让外层 Recover middleware 写 500
			if panicVal != nil {
				panic(panicVal)
			}
		}()

		c.Next()
	}
}

// resolveBusinessTier 按 status 决定 business audit Tier（plan §决策 D8）。
//
// Tier1（同步 fsync）：
//   - 401 auth_failed —— 攻击信号
//   - 402 insufficient_quota —— 资金信号
//   - 5xx —— 故障信号
//
// Tier2（异步 stderr）：
//   - 2xx 成功
//   - 其他 4xx（400 / 413 / 429 / 上游 4xx 透传）—— 高频低安全意义，避免拖慢 fsync
func resolveBusinessTier(status int) audit.AuditTier {
	if status == http.StatusUnauthorized ||
		status == http.StatusPaymentRequired ||
		status >= 500 {
		return audit.Tier1
	}
	return audit.Tier2
}

// resolveBusinessOutcomeCode 决定 audit record 的 outcome_code 字段。
//
// 优先级：
//  1. handler 通过 SetBusinessAuditOutcomeCode 显式设置（如 "insufficient_quota" / "upstream_5xx"）
//  2. status 范围推断（fallback）
func resolveBusinessOutcomeCode(c *gin.Context, status int) string {
	if v, ok := c.Get(ctxKeyBusinessAuditOutcome); ok {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	switch {
	case status >= 200 && status < 300:
		return "ok"
	case status >= 400 && status < 500:
		return "client_error"
	case status >= 500:
		return "internal_error"
	default:
		return "unknown"
	}
}

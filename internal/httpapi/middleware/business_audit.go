package middleware

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/sunxin-git/api-gateway/internal/audit"
	"github.com/sunxin-git/api-gateway/internal/relay"
)

// ctx Setters / Getters 集中在 internal/relay/audit_meta.go（避免 middleware ↔ relay
// 循环依赖；relay handler 写、middleware audit 读，方向单向）。
// RelayHandler 通过 relay.SetBusinessAudit* 注入；本 middleware defer 时通过
// relay.GetBusinessAudit* 读取。

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
				// 注入 handler 元数据见下方 relay.GetBusinessAudit* 读取
			}

			// 注入业务身份
			if vr := GetBusinessKeyValidation(c); vr != nil && vr.Key != nil {
				rec.BusinessAccountID = vr.Key.BusinessAccountID
				rec.APIKeyID = vr.Key.ID
				rec.Actor = "business_key:" + strconv.FormatInt(vr.Key.ID, 10)
			} else {
				rec.Actor = "anonymous"
			}

			// 注入 handler 元数据（relay.SetBusinessAudit* 注入 → 此处统一读出）
			rec.InputTokens = relay.GetBusinessAuditInputTokens(c)
			rec.OutputTokens = relay.GetBusinessAuditOutputTokens(c)
			rec.CostMinor = relay.GetBusinessAuditCostMinor(c)
			rec.GatewayModel = relay.GetBusinessAuditGatewayModel(c)
			rec.UpstreamModel = relay.GetBusinessAuditUpstreamModel(c)
			rec.UpstreamStatus = relay.GetBusinessAuditUpstreamStatus(c)
			rec.UpstreamDurationMs = relay.GetBusinessAuditUpstreamDurationMs(c)

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
//  1. handler 通过 relay.SetBusinessAuditOutcomeCode 显式设置（如 "insufficient_quota" / "upstream_5xx"）
//  2. status 范围推断（fallback）
func resolveBusinessOutcomeCode(c *gin.Context, status int) string {
	if s := relay.GetBusinessAuditOutcomeCode(c); s != "" {
		return s
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

package middleware

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/sunxin-git/api-gateway/internal/admintoken"
)

// AdminThrottle RPM + 熔断器中间件（计划 Unit 4 + R5）。
//
// 链上位置：AdminTokenAuth 之后；handler 之前。
//
// 仅做"不需要 amount"的预检：
//   - CheckRPM 优先（最快失败 / 内存检查）
//   - CheckCircuitBreaker 第二（一次 DB SELECT）
//
// 不做 daily / single recharge / refund 检查：这些需要 handler 知道 amount，
// 推到 handler 内部预检（Unit 5 实施）。
//
// 失败时 429 + bump `admin_api_quota_exceeded_total{quota_type, token_id}`。
//
// 与决策 D2 + Unit 3 对齐：
//   - throttle.CheckRPM 是纯内存（InProcessRPM）；token.RequestsPerMinute = nil 时直接通过
//   - throttle.CheckCircuitBreaker 在 token.CircuitBreakerEnabled = false 时直接通过
func AdminThrottle(thr admintoken.Throttle, quotaExceededCounter *prometheus.CounterVec) gin.HandlerFunc {
	return func(c *gin.Context) {
		vr := GetAdminTokenValidation(c)
		if vr == nil || vr.Token == nil {
			// 防御性：AdminTokenAuth 应已注入；缺失视作 fail-closed
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
				"error": gin.H{
					"code":       "internal_error",
					"message":    "服务内部错误（throttle 缺少 token 上下文）",
					"request_id": GetRequestID(c),
				},
			})
			return
		}
		token := vr.Token

		// 1. RPM（内存最快）
		if err := thr.CheckRPM(token); err != nil {
			if errors.Is(err, admintoken.ErrRPMExceeded) {
				bumpQuotaExceeded(quotaExceededCounter, "rpm", token.ID)
				abortRateLimited(c, "rate_limited", "请求过于频繁")
				return
			}
			abortInternal(c, err)
			return
		}

		// 2. Circuit breaker（DB SELECT；token.CircuitBreakerEnabled=false 时 noop）
		if err := thr.CheckCircuitBreaker(c.Request.Context(), token); err != nil {
			if errors.Is(err, admintoken.ErrCircuitOpen) {
				bumpQuotaExceeded(quotaExceededCounter, "circuit_open", token.ID)
				abortRateLimited(c, "circuit_open", "Token 熔断中，请稍后重试")
				return
			}
			abortInternal(c, err)
			return
		}

		c.Next()
	}
}

func bumpQuotaExceeded(counter *prometheus.CounterVec, quotaType string, tokenID int64) {
	if counter == nil {
		return
	}
	counter.WithLabelValues(quotaType, formatTokenIDLabel(tokenID)).Inc()
}

func abortRateLimited(c *gin.Context, code, message string) {
	c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
		"error": gin.H{
			"code":       code,
			"message":    message,
			"request_id": GetRequestID(c),
		},
	})
}

func abortInternal(c *gin.Context, err error) {
	_ = c.Error(err)
	if !c.Writer.Written() {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
			"error": gin.H{
				"code":       "internal_error",
				"message":    "服务内部错误",
				"request_id": GetRequestID(c),
			},
		})
	} else {
		c.Abort()
	}
}

// formatTokenIDLabel 把 token_id 转为 metric label 安全字符串。
//
// 设计：直接 int64 → 十进制字符串；Prometheus 高基数风险由"token 总数 ≤ 几百个"承受。
// 若 P1 token 数膨胀，可改为 hash bucket。
func formatTokenIDLabel(id int64) string {
	if id == 0 {
		return "unknown"
	}
	return strconv.FormatInt(id, 10)
}

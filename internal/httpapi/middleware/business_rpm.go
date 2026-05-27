package middleware

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/sunxin-git/api-gateway/internal/businesskey"
	"github.com/sunxin-git/api-gateway/internal/relay"
)

// BusinessRPM 业务侧 RPM 限速中间件（plan §Unit 4 + R11）。
//
// 链上位置：BusinessKeyAuth 之后，BusinessAudit 之前。
//
// 单 RPM 检查（plan §D7：仅 RPM，无 circuit breaker；MVP 简化）：
//   - key.RequestsPerMinute = nil → 直接通过
//   - 60s 滚动窗口超限 → 429 rate_limit_exceeded
//
// 失败时 429 OpenAI 兼容错误 + bump rateLimitedCounter{key_id}。
func BusinessRPM(rpm *businesskey.InProcessRPM, rateLimitedCounter *prometheus.CounterVec) gin.HandlerFunc {
	return func(c *gin.Context) {
		vr := GetBusinessKeyValidation(c)
		if vr == nil || vr.Key == nil {
			// 防御性：BusinessKeyAuth 应已注入；缺失 fail-closed
			relay.WriteErrorJSON(c, http.StatusInternalServerError,
				relay.ErrTypeAPIError, "internal_error",
				"服务内部错误（rpm check 缺少 key 上下文）")
			return
		}
		key := vr.Key

		if err := rpm.Check(key); err != nil {
			if errors.Is(err, businesskey.ErrRPMExceeded) {
				if rateLimitedCounter != nil {
					rateLimitedCounter.WithLabelValues(formatBusinessKeyIDLabel(key.ID)).Inc()
				}
				relay.WriteErrorJSON(c, http.StatusTooManyRequests,
					relay.ErrTypeRateLimitExceeded, "rate_limit_exceeded",
					"请求过于频繁，请稍后重试")
				return
			}
			// 不应发生的其他错误
			relay.WriteErrorJSON(c, http.StatusInternalServerError,
				relay.ErrTypeAPIError, "internal_error", "服务内部错误")
			_ = c.Error(err)
			return
		}

		c.Next()
	}
}

// formatBusinessKeyIDLabel key.ID → metric label。
func formatBusinessKeyIDLabel(id int64) string {
	if id == 0 {
		return "unknown"
	}
	return strconv.FormatInt(id, 10)
}

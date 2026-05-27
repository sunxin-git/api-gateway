package middleware

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/sunxin-git/api-gateway/internal/businesskey"
	"github.com/sunxin-git/api-gateway/internal/relay"
)

const (
	// CtxKeyBusinessKey gin.Context 中业务 key 验证结果的 key（*businesskey.ValidationResult）。
	CtxKeyBusinessKey = "business_key_validation"
)

// BusinessKeyAuth 业务 API Key 鉴权中间件（plan §Unit 4 + R2）。
//
// 处理顺序：
//
//  1. 取 Authorization: Bearer <key> 头；缺失或非 Bearer scheme → 401 invalid_api_key
//  2. svc.ValidateByPlaintext(plaintext)：
//     - ErrKeyNotFound（含未知 / revoked）→ 401 invalid_api_key
//     - 其他 error → 500 api_error
//  3. 成功 → c.Set(CtxKeyBusinessKey, vr); c.Next()
//
// metric authFailedCounter 记录失败原因（label=reason: missing_header / bad_scheme /
// empty_token / invalid_api_key / internal_error）。
//
// 与 admin AdminTokenAuth 的差异（plan §D4 / D14）：
//   - 无 IP allowlist 校验（业务系统通常多 region 接入；MVP 简化）
//   - 错误响应用 OpenAI 兼容格式（业务 SDK 解析依赖）
//   - last_used_at 异步更新由 businesskey.Service 内部触发（markTouched）
func BusinessKeyAuth(svc businesskey.Service, authFailedCounter *prometheus.CounterVec) gin.HandlerFunc {
	return func(c *gin.Context) {
		plaintext, reason, ok := extractBearerToken(c.GetHeader("Authorization"))
		if !ok {
			respondBusinessAuthFailed(c, "missing_api_key",
				"Admin API Key 缺失或格式非法", authFailedCounter, reason)
			return
		}

		vr, err := svc.ValidateByPlaintext(c.Request.Context(), plaintext)
		if err != nil {
			switch {
			case errors.Is(err, businesskey.ErrKeyNotFound):
				respondBusinessAuthFailed(c, "invalid_api_key",
					"API Key 无效", authFailedCounter, "invalid_api_key")
			default:
				// 系统错误（DB 故障等）；不暴露给客户端
				relay.WriteErrorJSON(c, http.StatusInternalServerError,
					relay.ErrTypeAPIError, "internal_error", "服务内部错误")
				if authFailedCounter != nil {
					authFailedCounter.WithLabelValues("internal_error").Inc()
				}
				_ = c.Error(err)
			}
			return
		}

		c.Set(CtxKeyBusinessKey, vr)
		c.Next()
	}
}

// GetBusinessKeyValidation 从 gin.Context 取出业务鉴权结果；不存在返 nil。
//
// handler / 下游 middleware 通过此函数访问 key 视图。
func GetBusinessKeyValidation(c *gin.Context) *businesskey.ValidationResult {
	v, ok := c.Get(CtxKeyBusinessKey)
	if !ok {
		return nil
	}
	vr, ok := v.(*businesskey.ValidationResult)
	if !ok {
		return nil
	}
	return vr
}

// respondBusinessAuthFailed 401 响应 + bump auth_failed metric + abort。
//
// 不在此处 emit audit；audit 由 BusinessAudit middleware（defer 模式）统一记录。
func respondBusinessAuthFailed(c *gin.Context, code, message string, counter *prometheus.CounterVec, reason string) {
	if counter != nil && reason != "" {
		counter.WithLabelValues(reason).Inc()
	}
	relay.WriteErrorJSON(c, http.StatusUnauthorized, relay.ErrTypeInvalidAPIKey, code, message)
}

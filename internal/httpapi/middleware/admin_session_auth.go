package middleware

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"net/netip"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/sunxin-git/api-gateway/internal/admintoken"
	"github.com/sunxin-git/api-gateway/internal/session"
)

const (
	// AdminSessionCookieName 管理后台会话 Cookie 名（HttpOnly + Secure + SameSite）。
	AdminSessionCookieName = "admin_session"
	// CSRFHeaderName 会话通道状态变更请求携带的 CSRF token header。
	CSRFHeaderName = "X-CSRF-Token"
)

// AdminAuth 是 /admin/v1 链的**前置鉴权**（ADR-0008 决策 4：会话 OR Bearer 二选一）。
//
// 顺序：
//  1. 有会话 Cookie 且有效 → 注入 operator principal；状态变更方法校验 CSRF（缺/错 → 403）。
//  2. 否则走 Bearer（与既有 AdminTokenAuth 同逻辑）→ 注入 admin_token principal + CtxKeyAdminToken。
//  3. 两者都无 → 401。
//
// 会话无效（过期 / 被删）时**继续尝试 Bearer**（请求可能同时带 token）；DB 等系统错误 → 500。
func AdminAuth(sessionSvc session.Service, tokenSvc admintoken.Service, authFailedCounter *prometheus.CounterVec) gin.HandlerFunc {
	return func(c *gin.Context) {
		if cookie, err := c.Cookie(AdminSessionCookieName); err == nil && cookie != "" {
			sc, lerr := sessionSvc.Lookup(c.Request.Context(), cookie)
			switch {
			case lerr == nil:
				if isStateChangingMethod(c.Request.Method) && !validCSRF(c, sc.CSRFToken) {
					respondForbidden(c, "csrf_failed", "CSRF token 缺失或不匹配", authFailedCounter, "csrf_failed")
					return
				}
				SetAdminPrincipal(c, &AdminPrincipal{
					Kind:             PrincipalKindOperator,
					OperatorID:       sc.OperatorID,
					OperatorUsername: sc.Username,
				})
				c.Next()
				return
			case errors.Is(lerr, session.ErrSessionInvalid):
				// 会话过期 / 被删 → 落到 Bearer 兜底（不直接拒绝）
			default:
				// DB 等系统错误：不暴露细节
				respondAuthFailed(c, "internal_error", "服务内部错误", authFailedCounter, "internal_error")
				_ = c.Error(lerr)
				return
			}
		}

		// Bearer 兜底
		vr, ok := validateBearerToken(c, tokenSvc, authFailedCounter)
		if !ok {
			return
		}
		SetAdminPrincipal(c, &AdminPrincipal{Kind: PrincipalKindAdminToken, Token: vr.Token})
		c.Next()
	}
}

// validateBearerToken 执行 Bearer 鉴权（提取 token + 解析 IP + svc.ValidateByPlaintext）。
//
// 成功：c.Set(CtxKeyAdminToken, vr) + 返回 (vr, true)。
// 失败：已写 401/500 响应并 abort + 返回 (nil, false)。
//
// 由 AdminTokenAuth（纯 Bearer 链）与 AdminAuth（会话 OR Bearer）共用，逻辑单一真相源。
func validateBearerToken(c *gin.Context, svc admintoken.Service, counter *prometheus.CounterVec) (*admintoken.ValidationResult, bool) {
	plaintext, reason, ok := extractBearerToken(c.GetHeader("Authorization"))
	if !ok {
		respondAuthFailed(c, "unauthorized", "Admin Token 缺失或格式非法", counter, reason)
		return nil, false
	}

	clientIP, err := netip.ParseAddr(c.ClientIP())
	if err != nil || !clientIP.IsValid() {
		respondAuthFailed(c, "ip_not_allowed", "源 IP 非法或缺失", counter, "invalid_ip")
		return nil, false
	}

	vr, err := svc.ValidateByPlaintext(c.Request.Context(), plaintext, clientIP)
	if err != nil {
		switch {
		case errors.Is(err, admintoken.ErrTokenNotFound),
			errors.Is(err, admintoken.ErrTokenRevoked),
			errors.Is(err, admintoken.ErrTokenExpired):
			respondAuthFailed(c, "unauthorized", "Admin Token 无效", counter, "token_invalid")
		case errors.Is(err, admintoken.ErrIPNotAllowed):
			respondAuthFailed(c, "ip_not_allowed", "源 IP 不在白名单内", counter, "ip_not_allowed")
		default:
			// 内部错误（DB 故障等）：保持单一 401 响应（不暴露内部状态），仅记录到 c.Error。
			// 不再二次 AbortWithStatus(500)——否则 audit 读到的 writer status 被覆盖为 500，
			// 与客户端实收的 401 不一致、并误触熔断器计数（安全审查 P1-1）。
			_ = c.Error(err)
			respondAuthFailed(c, "internal_error", "服务内部错误", counter, "internal_error")
		}
		return nil, false
	}

	c.Set(CtxKeyAdminToken, vr)
	return vr, true
}

// isStateChangingMethod 状态变更方法（须校验 CSRF）。
func isStateChangingMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// validCSRF 常量时间比对请求 header 的 CSRF token 与会话绑定值。
func validCSRF(c *gin.Context, want string) bool {
	got := c.GetHeader(CSRFHeaderName)
	if got == "" || want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// respondForbidden 403 响应 + bump metric + abort（CSRF / scope 等拒绝）。
func respondForbidden(c *gin.Context, code, message string, counter *prometheus.CounterVec, reason string) {
	if counter != nil && reason != "" {
		counter.WithLabelValues(reason).Inc()
	}
	c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
		"error": gin.H{
			"code":       code,
			"message":    message,
			"request_id": GetRequestID(c),
		},
	})
}

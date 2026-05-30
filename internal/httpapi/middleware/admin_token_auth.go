package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/sunxin-git/api-gateway/internal/admintoken"
)

const (
	// CtxKeyAdminToken gin.Context 中 admin token 验证结果的 key（*admintoken.ValidationResult）。
	CtxKeyAdminToken = "admin_token_validation"
)

// AdminTokenAuth Admin Token 鉴权中间件（计划 Unit 4 + R2 / R4）。
//
// 处理顺序：
//
//  1. 取 Authorization: Bearer <token> 头；缺失或非 Bearer scheme → 401 unauthorized
//  2. 解析 c.ClientIP() → netip.Addr；解析失败 → 401 ip_not_allowed（fail-closed）
//  3. svc.ValidateByPlaintext(plaintext, clientIP)：
//     - ErrTokenNotFound → 401 unauthorized
//     - ErrIPNotAllowed → 401 ip_not_allowed
//     - 其他 error → 500 internal_error
//  4. 成功 → c.Set(CtxKeyAdminToken, vr); c.Next()
//
// metric `authFailedCounter` 记录失败原因（label=reason: missing_header / bad_scheme / token_invalid /
// ip_not_allowed / invalid_ip / internal_error）。
//
// 与决策 D2 + D10 对齐：
//   - 仅支持 Bearer header；不支持 query string / cookie（避免明文 token 入 URL / referer）
//   - IP 校验内嵌于 admintoken.Service.ValidateByPlaintext，本中间件不独立 IP middleware
//   - 失败路径**不**进入 throttle 阀门（next middleware 不被调用）
func AdminTokenAuth(svc admintoken.Service, authFailedCounter *prometheus.CounterVec) gin.HandlerFunc {
	return func(c *gin.Context) {
		vr, ok := validateBearerToken(c, svc, authFailedCounter)
		if !ok {
			return
		}
		// Bearer 成功也注入归一化身份（ADR-0008 决策 4：下游 Scope/Audit 统一读 AdminPrincipal）。
		SetAdminPrincipal(c, &AdminPrincipal{Kind: PrincipalKindAdminToken, Token: vr.Token})
		c.Next()
	}
}

// GetAdminTokenValidation 从 gin.Context 取出鉴权结果；不存在返 nil。
//
// handler / 下游 middleware 通过此函数访问 token 视图。
func GetAdminTokenValidation(c *gin.Context) *admintoken.ValidationResult {
	v, ok := c.Get(CtxKeyAdminToken)
	if !ok {
		return nil
	}
	vr, ok := v.(*admintoken.ValidationResult)
	if !ok {
		return nil
	}
	return vr
}

// extractBearerToken 从 Authorization header 提取 Bearer plaintext。
//
// 返回值：(plaintext, reason, ok)。失败时 reason 为 metric label。
//
// 验收规则：
//   - 空 header / 全空白 → reason="missing_header"
//   - scheme 部分非 "Bearer"（大小写不敏感）→ reason="bad_scheme"
//   - scheme 后无 token 或仅空白 → reason="empty_token"
//
// 实现：按第一个空白拆分 scheme / token；不依赖固定前缀长度，"Bearer\ttoken" 也支持。
func extractBearerToken(header string) (string, string, bool) {
	h := strings.TrimSpace(header)
	if h == "" {
		return "", "missing_header", false
	}
	// 按首个 ASCII 空白拆分 scheme / 剩余
	sp := strings.IndexAny(h, " \t")
	if sp == -1 {
		// 只有 scheme（如 "Bearer"）→ 缺 token；但若 scheme 本身不是 bearer，bad_scheme 更准确
		if strings.EqualFold(h, "bearer") {
			return "", "empty_token", false
		}
		return "", "bad_scheme", false
	}
	if !strings.EqualFold(h[:sp], "bearer") {
		return "", "bad_scheme", false
	}
	plaintext := strings.TrimSpace(h[sp+1:])
	if plaintext == "" {
		return "", "empty_token", false
	}
	return plaintext, "", true
}

// respondAuthFailed 401 响应 + bump auth_failed metric + abort。
//
// 不在此处 emit audit；audit 由 AdminAudit middleware（defer 模式）统一记录。
func respondAuthFailed(c *gin.Context, code, message string, counter *prometheus.CounterVec, reason string) {
	if counter != nil && reason != "" {
		counter.WithLabelValues(reason).Inc()
	}
	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
		"error": gin.H{
			"code":       code,
			"message":    message,
			"request_id": GetRequestID(c),
		},
	})
}

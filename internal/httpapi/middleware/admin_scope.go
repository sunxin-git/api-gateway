package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/sunxin-git/api-gateway/internal/admintoken"
)

// AdminScope handler 级 scope 校验中间件（计划 Unit 4 + R3）。
//
// 用法（在路由注册时按 handler 包裹）：
//
//	g := s.Engine().Group("/admin/v1")
//	g.POST("/business-accounts",
//	    middleware.AdminScope(svc, "business_account:create", authFailedCounter),
//	    handler.CreateAccount,
//	)
//
// 参数 `requiredScope` 必填；空串 panic（启动期 fail-fast，禁止"无要求"路由）。
//
// 失败：403 insufficient_scope（**不**消耗限流配额；throttle 已在上层校验过）。
//
// metric `authFailedCounter` 复用 admin_api_auth_failed_total{reason="insufficient_scope"}。
func AdminScope(svc admintoken.Service, requiredScope string, authFailedCounter *prometheus.CounterVec) gin.HandlerFunc {
	if requiredScope == "" {
		panic("middleware.AdminScope: requiredScope 不能为空字符串（fail-closed）")
	}
	return func(c *gin.Context) {
		vr := GetAdminTokenValidation(c)
		if vr == nil || vr.Token == nil {
			// 防御性：AdminTokenAuth 应已注入；缺失视作 fail-closed
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
				"error": gin.H{
					"code":       "internal_error",
					"message":    "服务内部错误（scope check 缺少 token 上下文）",
					"request_id": GetRequestID(c),
				},
			})
			return
		}
		if !svc.CheckScope(vr.Token, requiredScope) {
			if authFailedCounter != nil {
				authFailedCounter.WithLabelValues("insufficient_scope").Inc()
			}
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": gin.H{
					"code":       "insufficient_scope",
					"message":    "缺少所需 scope: " + requiredScope,
					"request_id": GetRequestID(c),
				},
			})
			return
		}
		c.Next()
	}
}

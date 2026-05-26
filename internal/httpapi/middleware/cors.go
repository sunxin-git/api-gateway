package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// CORS 实现一个最小、fail-closed 的 CORS 中间件。
//
// 行为：
//   - 白名单为空 = 拒绝所有跨域请求（带 Origin header 的请求直接 403）。
//     不带 Origin 的同源请求正常放行。
//   - Origin 在白名单中：回写 Access-Control-Allow-Origin 等头，OPTIONS 预检直接 204。
//   - Origin 不在白名单中：返回 403，不暴露白名单内容。
//
// 选择最小实现（而非 gin-contrib/cors）：依赖最小化，行为更可审计。
func CORS(allowedOrigins []string) gin.HandlerFunc {
	// 转 set 加速查询；空白名单 → 空 set。
	set := make(map[string]struct{}, len(allowedOrigins))
	for _, o := range allowedOrigins {
		if o != "" {
			set[o] = struct{}{}
		}
	}

	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		// 无 Origin = 同源请求或服务器对服务器调用，直接放行。
		if origin == "" {
			c.Next()
			return
		}

		if _, ok := set[origin]; !ok {
			// fail-closed：未在白名单的 Origin 拒绝（包括白名单为空的情况）。
			c.AbortWithStatus(http.StatusForbidden)
			return
		}

		// 在白名单内：回写 CORS 响应头。
		c.Writer.Header().Set("Access-Control-Allow-Origin", origin)
		c.Writer.Header().Set("Vary", "Origin")
		c.Writer.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, PATCH, OPTIONS")
		c.Writer.Header().Set("Access-Control-Allow-Headers",
			"Content-Type, Authorization, X-Request-Id")
		c.Writer.Header().Set("Access-Control-Allow-Credentials", "true")
		c.Writer.Header().Set("Access-Control-Max-Age", "600")

		// 预检请求直接 204 返回。
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	}
}

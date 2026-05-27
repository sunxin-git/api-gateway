package middleware

import (
	"github.com/gin-gonic/gin"
)

// HSTSHeader Strict-Transport-Security 响应头值（计划 Unit 7 决策）。
//
// max-age=63072000 = 2 年；includeSubDomains 强化所有子域；preload 是浏览器内置 HSTS 列表预登记
// （此处不加，避免误注册难以撤销；如运维需要可在反代层补）。
//
// 即便后端是 HTTP（前端反代终止 TLS），写本 header 也无害 —— 浏览器只在 HTTPS 请求下接受
// HSTS；HTTP 上写它会被忽略。配置在 admin 路由组下，作为部署侧 TLS 终止的"双保险"。
const HSTSHeader = "max-age=63072000; includeSubDomains"

// HSTS 把 Strict-Transport-Security header 写入响应。
//
// 用法：装在 admin 路由组上即可（全局装也无副作用，但语义不够清晰）：
//
//	g := engine.Group("/admin/v1")
//	g.Use(middleware.HSTS())
//	...
func HSTS() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Writer.Header().Set("Strict-Transport-Security", HSTSHeader)
		c.Next()
	}
}

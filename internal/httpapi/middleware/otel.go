package middleware

import (
	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"
)

// OTel 用 otelgin contrib 中间件为每个请求开 span。
// serviceName 作为 span scope；trace provider 应已通过 otel.SetTracerProvider 全局注册。
func OTel(serviceName string) gin.HandlerFunc {
	return otelgin.Middleware(serviceName)
}

package middleware

import (
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
)

// Recover 捕获 handler / 下游 middleware 抛出的 panic，将其转为 500 响应，
// 同时写 error 日志（含 stack）并自增 gateway_panic_total 计数。
//
// logger 为已初始化的 slog Logger（通常即 obs.NewLogger 的返回值）。
// panicCounter 为 obs.Metrics.PanicTotal。
func Recover(logger *slog.Logger, panicCounter prometheus.Counter) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				if panicCounter != nil {
					panicCounter.Inc()
				}
				if logger != nil {
					logger.Error("HTTP handler panic",
						slog.Any("panic", r),
						slog.String("request_id", GetRequestID(c)),
						slog.String("method", c.Request.Method),
						slog.String("path", c.Request.URL.Path),
						slog.String("stack", string(debug.Stack())),
					)
				}
				// 防止重复 WriteHeader：如已写过则跳过。
				if !c.Writer.Written() {
					c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
						"error":      "内部错误",
						"request_id": GetRequestID(c),
					})
				} else {
					c.Abort()
				}
			}
		}()
		c.Next()
	}
}

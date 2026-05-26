package middleware

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
)

// Prom 把每请求耗时写入 gateway_http_request_duration_seconds histogram。
// path 取 Gin route 模板（c.FullPath）以避免高基数 label 爆炸。
func Prom(httpReqDuration *prometheus.HistogramVec) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		path := c.FullPath()
		if path == "" {
			// 未匹配到路由的请求统一归为 "unknown"，避免 URL path 高基数。
			path = "unknown"
		}
		httpReqDuration.WithLabelValues(
			c.Request.Method,
			path,
			strconv.Itoa(c.Writer.Status()),
		).Observe(time.Since(start).Seconds())
	}
}

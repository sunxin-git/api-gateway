package middleware

import (
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/sunxin-git/api-gateway/internal/obs"
)

// RedactSensitiveAttrs 是 obs.RedactSensitiveAttrs 的 thin forwarder（保留 D-min Unit 4
// API 兼容）。
//
// 实现搬到 obs 包以避免 obs ↔ middleware 循环（F-min Unit 5 重构发现的循环路径：
// relay → ledger → obs → middleware → relay）。新代码请直接调 obs.RedactSensitiveAttrs。
func RedactSensitiveAttrs() func(groups []string, attr slog.Attr) slog.Attr {
	return obs.RedactSensitiveAttrs()
}

// Slog 输出每请求一条结构化 access log。
// 字段：method, path, status, latency_ms, request_id, client_ip, user_agent。
//
// 注意 path 取 c.FullPath()（Gin 路由模板，如 "/users/:id"），避免高基数 label。
// 若 FullPath 为空（未匹配到路由），退化到 c.Request.URL.Path。
func Slog(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		dur := time.Since(start)

		path := c.FullPath()
		if path == "" {
			path = c.Request.URL.Path
		}

		logger.Info("http_access",
			slog.String("method", c.Request.Method),
			slog.String("path", path),
			slog.Int("status", c.Writer.Status()),
			slog.Int64("latency_ms", dur.Milliseconds()),
			slog.String("request_id", GetRequestID(c)),
			slog.String("client_ip", c.ClientIP()),
			slog.String("user_agent", c.Request.UserAgent()),
		)
	}
}

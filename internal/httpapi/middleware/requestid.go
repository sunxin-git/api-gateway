// Package middleware 提供 Gin HTTP 中间件链。
//
// 装配顺序（由 httpapi.Server 维护）：
//
//	recover → requestid → slog → otel → prom → cors
//
// 每个中间件遵守：
//   - 只通过 gin.Context 传值，不污染全局状态。
//   - 失败优先（fail-closed）：未知/异常输入按拒绝处理。
package middleware

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	// HeaderRequestID 标准 request id 响应/请求 header 名。
	HeaderRequestID = "X-Request-Id"
	// CtxKeyRequestID gin.Context 中 request id 的 key。
	CtxKeyRequestID = "request_id"
)

// RequestID 从 X-Request-Id 取值；缺失则生成 UUIDv7（时间序，便于日志按时间排序）。
// 写回响应 header 与 gin context，供下游 middleware / handler 使用。
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		rid := c.GetHeader(HeaderRequestID)
		if rid == "" {
			// UUIDv7 失败概率极低（系统时钟读取或 rand 读取错误）；
			// 兜底退化到 UUIDv4，保证请求一定有 id。
			if id, err := uuid.NewV7(); err == nil {
				rid = id.String()
			} else {
				rid = uuid.NewString()
			}
		}
		c.Set(CtxKeyRequestID, rid)
		c.Writer.Header().Set(HeaderRequestID, rid)
		c.Next()
	}
}

// GetRequestID 从 gin.Context 取 request id；不存在返回空串。
func GetRequestID(c *gin.Context) string {
	if v, ok := c.Get(CtxKeyRequestID); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

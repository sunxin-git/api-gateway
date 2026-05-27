package middleware

import (
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// sensitiveAttrKeyPattern 匹配应当 redact 的 attribute key（大小写不敏感）。
//
// 匹配规则：key 含 authorization / token / key / secret / cookie 任一子串。
// 用于全局 slog handler 的 ReplaceAttr 钩子；同时防止 audit / handler 误将 Bearer 明文写入日志。
//
// 决策（计划 Unit 4 全局 slog redactor）：
//   - 即便当前 Slog 中间件不打 header，未来扩展时 redactor 仍是兜底防线
//   - 用 regexp 而非白名单：日志 attr key 命名风格未来可能变（如 authToken / apiSecret），单一 pattern 维护成本低
var sensitiveAttrKeyPattern = regexp.MustCompile(`(?i)(authorization|token|key|secret|cookie)`)

// redactedPlaceholder slog attr 命中敏感 key 时的替换值。
const redactedPlaceholder = "[REDACTED]"

// RedactSensitiveAttrs 返回 slog.HandlerOptions.ReplaceAttr 兼容函数。
//
// 用法：obs.NewLogger 在构造 *slog.Logger 时把本函数挂到 HandlerOptions.ReplaceAttr。
// 命中规则的 attr：
//   - key 匹配 sensitiveAttrKeyPattern → value 替换为 [REDACTED]
//   - 内层 group 同样递归命中（slog 框架自动按 group 调用）
//
// 不影响非敏感字段；不修改 time / level / msg 等顶层字段。
func RedactSensitiveAttrs() func(groups []string, attr slog.Attr) slog.Attr {
	return func(_ []string, attr slog.Attr) slog.Attr {
		// 跳过 slog 内置字段（time/level/msg/source）；它们 key 都是英文小写预定义
		switch attr.Key {
		case slog.TimeKey, slog.LevelKey, slog.MessageKey, slog.SourceKey:
			return attr
		}
		if sensitiveAttrKeyPattern.MatchString(strings.ToLower(attr.Key)) {
			return slog.Attr{Key: attr.Key, Value: slog.StringValue(redactedPlaceholder)}
		}
		return attr
	}
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

package obs

import (
	"log/slog"
	"regexp"
	"strings"
)

// sensitiveAttrKeyPattern 匹配应当 redact 的 attribute key（大小写不敏感）。
//
// 匹配规则：key 含 authorization / token / key / secret / cookie 任一子串。
// 用于全局 slog handler 的 ReplaceAttr 钩子；同时防止 audit / handler 误将 Bearer 明文写入日志。
//
// 决策（D-min Unit 4 全局 slog redactor）：
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
//
// 设计位置：放在 obs 包（不依赖 gin / middleware），让 obs/log.go 直接装载；
// 同时避免 middleware ↔ obs 循环依赖（F-min Unit 5 重构时发现）。
func RedactSensitiveAttrs() func(groups []string, attr slog.Attr) slog.Attr {
	return func(_ []string, attr slog.Attr) slog.Attr {
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

// Package obs 提供可观测性基础设施：日志（slog JSON）、指标（Prometheus）、链路（OTel）。
package obs

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// NewLogger 按指定等级创建 slog JSON handler，输出到 stderr。
//
// level 支持 debug | info | warn | error；其他值返回错误。
// stderr 用于结构化日志，stdout 留给 trace exporter 等其他工具占用。
func NewLogger(level string) (*slog.Logger, error) {
	lvl, err := parseLevel(level)
	if err != nil {
		return nil, err
	}
	return newLoggerWithWriter(os.Stderr, lvl), nil
}

// newLoggerWithWriter 内部入口，便于测试时注入 buffer。
func newLoggerWithWriter(w io.Writer, lvl slog.Level) *slog.Logger {
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level:     lvl,
		AddSource: false, // 性能优先，必要时按需打开
	})
	return slog.New(h)
}

func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("非法日志等级 %q（仅支持 debug|info|warn|error）", s)
	}
}

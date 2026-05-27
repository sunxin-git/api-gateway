package audit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

// Logger 是 audit 包对外的主入口（计划 Unit 4 + 决策 D3）。
//
// 持有两个 sink（Tier1 同步文件 / Tier2 异步 stderr），按 record.Tier 路由。
// middleware 与 handler 全部通过 Logger 写 audit；不直接接触 Sink。
//
// 错误传播：
//   - Tier1 sink 失败 → Emit 返 error；caller 升级 503 + readiness 关闸
//   - Tier2 sink 失败 → 内部 slog warn + 返 nil（best-effort，不影响请求）
//   - record.Tier = TierUnknown → 返 error（fail-closed 防止误降级到 Tier2）
type Logger struct {
	tier1   Sink
	tier2   Sink
	failLog *slog.Logger // 仅用于"Tier2 写失败"的兜底 warn 输出（避免日志循环）
}

// AuditLogger 是 Logger 的接口形式，便于上层 middleware mock 替换。
type AuditLogger interface {
	Emit(ctx context.Context, record AuditRecord) error
	Close() error
}

// 编译期断言。
var _ AuditLogger = (*Logger)(nil)

// NewLogger 构造 Logger。
//
// 入参：
//   - tier1：高价值同步 sink（通常 SyncFileSink）；nil → Emit Tier1 record 时返 error（fail-closed）
//   - tier2：低价值异步 sink（通常 AsyncStderrSink）；nil → Emit Tier2 record 时返 error
//   - failLog：兜底 logger（Tier2 写失败时 warn）；nil → silent
func NewLogger(tier1, tier2 Sink, failLog *slog.Logger) *Logger {
	return &Logger{tier1: tier1, tier2: tier2, failLog: failLog}
}

// Emit 把 record 路由到对应 sink。
//
// 路由规则：
//   - Tier1 → tier1.Emit；失败必须返 error（caller 升级告警）
//   - Tier2 → tier2.Emit；失败时 warn 日志 + 返 nil（best-effort）
//   - 其他 (TierUnknown) → 返 error（fail-closed）
//
// **不**在此处做 Tier 决策（不查 path / status）；调用方（admin_audit middleware）按业务规则决定 Tier 后传入。
func (l *Logger) Emit(ctx context.Context, record AuditRecord) error {
	switch record.Tier {
	case Tier1:
		if l.tier1 == nil {
			return errors.New("audit.Logger: Tier1 sink 未注入；fail-closed 拒绝写入")
		}
		return l.tier1.Emit(ctx, record)
	case Tier2:
		if l.tier2 == nil {
			return errors.New("audit.Logger: Tier2 sink 未注入；fail-closed 拒绝写入")
		}
		if err := l.tier2.Emit(ctx, record); err != nil {
			// Tier2 best-effort：warn 但不向上传播（避免低价值事件影响业务路径）
			if l.failLog != nil {
				l.failLog.Warn("Tier2 audit emit failed",
					slog.Any("error", err),
					slog.String("request_id", record.RequestID),
					slog.String("path", record.Path),
				)
			}
			return nil
		}
		return nil
	default:
		return fmt.Errorf("audit.Logger: 未知 tier=%d；fail-closed 拒绝写入", record.Tier)
	}
}

// Close 关闭两个 sink；返回首个非 nil 错误（Tier1 优先）。
func (l *Logger) Close() error {
	var firstErr error
	if l.tier1 != nil {
		if err := l.tier1.Close(); err != nil {
			firstErr = err
		}
	}
	if l.tier2 != nil {
		if err := l.tier2.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

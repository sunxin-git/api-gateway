package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
)

// Sink 单个审计行的落地目标接口（计划 Unit 4 + 决策 D3）。
//
// 实现：
//   - SyncFileSink：高价值同步落盘（O_APPEND + O_SYNC，每写 fsync）
//   - AsyncStderrSink：低价值异步 stderr（基于 slog.JSONHandler，无 fsync）
//
// Emit 应在调用线程内同步返回；async 行为由 sink 内部实现（如 SyncFileSink 是真同步，
// AsyncStderrSink 是 stderr 写后立即返回，依赖 OS 缓冲）。
//
// 错误传播：
//   - Tier1 (SyncFileSink) 写失败必须返 error，Logger.Emit 透传到 caller，
//     caller（middleware）应 bump `admin_audit_write_failed_total` + readiness 关闸
//   - Tier2 (AsyncStderrSink) 失败也返 error，但 caller 通常 best-effort 忽略
type Sink interface {
	Emit(ctx context.Context, record AuditRecord) error
	// Close 释放底层资源（文件 / goroutine）；多次 Close 应幂等。
	Close() error
}

// =============================================================================
// SyncFileSink — Tier1 同步 O_APPEND + O_SYNC 文件
// =============================================================================

// SyncFileSink 把 audit 行同步写到独立 append-only 文件；每写 + fsync（O_SYNC）。
//
// 设计动机：refund / token lifecycle / 攻击信号事件不能容忍 best-effort 丢失；
// fsync 保证 HTTP 响应返回前 audit 已落盘（决策 D3）。
//
// 并发：sync.Mutex 串行化写；O_APPEND 已是 POSIX 原子写小于 PIPE_BUF 的数据，
// mutex 进一步保证多 goroutine 不交错半行。
type SyncFileSink struct {
	path string
	mu   sync.Mutex
	f    *os.File
}

// NewSyncFileSink 打开文件（已存在追加，不存在创建）；权限 0600（仅 owner 读写）。
//
// 错误：
//   - 路径不可写 → 返 error；caller 必须把这视为启动期 fail-fast
//   - dir 不存在 → 返 error；不自动创建（避免 typo 静默写到错误目录）
func NewSyncFileSink(path string) (*SyncFileSink, error) {
	if path == "" {
		return nil, errors.New("audit.SyncFileSink: path 不能为空")
	}
	// O_APPEND + O_SYNC + O_CREATE；权限 0600
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY|os.O_SYNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("打开 audit 文件 %q 失败: %w", path, err)
	}
	return &SyncFileSink{path: path, f: f}, nil
}

// Emit 写一行 JSON + fsync。写失败时返 error；caller 必须升级告警。
func (s *SyncFileSink) Emit(_ context.Context, record AuditRecord) error {
	line, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("json.Marshal AuditRecord 失败: %w", err)
	}
	// 追加换行（JSON Lines 格式，便于 log shipper 行级切分）
	line = append(line, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.f.Write(line); err != nil {
		return fmt.Errorf("写 audit 文件失败: %w", err)
	}
	// O_SYNC 已让每次 write 同步落盘，但显式 Sync() 在跨平台行为差异时更稳
	// （Windows 下 O_SYNC 语义弱；macOS 也建议显式 F_FULLFSYNC）
	if err := s.f.Sync(); err != nil {
		return fmt.Errorf("fsync audit 文件失败: %w", err)
	}
	return nil
}

// Close 关闭底层文件；多次 Close 幂等。
func (s *SyncFileSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	return err
}

// =============================================================================
// AsyncStderrSink — Tier2 异步 stderr（slog.JSONHandler）
// =============================================================================

// AsyncStderrSink 把 audit 行作为 slog Info 事件写到 stderr。
//
// 设计动机：低价值事件（recharge / balance read）量大；走 stderr 由部署侧 log shipper 转走。
// 同步 stderr 写本身非 OS 阻塞（依赖管道缓冲）；"异步"理解为"无 fsync"而非真后台 goroutine。
//
// 强制 JSONHandler（构造时拒绝 TextHandler 注入）：审计字段含 user-controlled 字符串
// （external_ref / account_id / reference_id），text handler 攻击者可注入伪造日志行。
type AsyncStderrSink struct {
	logger *slog.Logger
}

// NewAsyncStderrSink 构造 stderr sink；内部强制 slog.JSONHandler（防 text handler 注入）。
//
// 出参：可直接复用一个共享 *AsyncStderrSink 实例。
func NewAsyncStderrSink() *AsyncStderrSink {
	return newAsyncSinkTo(os.Stderr)
}

// newAsyncSinkTo 测试入口；允许注入任意 io.Writer 验证输出。
func newAsyncSinkTo(w io.Writer) *AsyncStderrSink {
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo})
	return &AsyncStderrSink{logger: slog.New(h)}
}

// Emit 把 AuditRecord 转换为 slog attrs 并 Info 输出。
//
// slog.JSONHandler 自动加 time / level / msg 三个外层字段；audit 字段作为 attrs 嵌入。
// 这与 SyncFileSink 的纯净 JSON 行格式不同：log shipper 在 ingest 时需识别两种结构。
func (s *AsyncStderrSink) Emit(ctx context.Context, record AuditRecord) error {
	s.logger.LogAttrs(ctx, slog.LevelInfo, "admin_audit",
		slog.Int("tier", int(record.Tier)),
		slog.String("request_id", record.RequestID),
		slog.Time("timestamp_utc", record.TimestampUTC),
		slog.Int64("token_id", record.TokenID),
		slog.String("token_description", record.TokenDescription),
		slog.String("actor", record.Actor),
		slog.String("source_ip", record.SourceIP),
		slog.String("method", record.Method),
		slog.String("path", record.Path),
		slog.String("request_hash", record.RequestHash),
		slog.Int64("body_size_bytes", record.BodySizeBytes),
		slog.Int("status", record.Status),
		slog.Int64("duration_ms", record.DurationMs),
		slog.String("outcome_code", record.OutcomeCode),
		slog.String("reason", record.Reason),
	)
	return nil
}

// Close stderr sink 无需关闭资源；幂等返 nil。
func (s *AsyncStderrSink) Close() error { return nil }

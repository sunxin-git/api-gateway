// Package asyncq 封装 Asynq client/server 的构造、队列命名约定与优雅停机。
//
// 设计依据 ADR-0006「异步执行基座」：
//   - Asynq 仅作**异步执行器**（享其重试/退避/调度/可重入 handler）；
//   - **不**承载 R15 并发硬上限——并发上限由 DB 原子 claim 承载（见 ADR-0006 决策 2）；
//   - 这里的 Concurrency / Queues 只是**执行层吞吐与调度优先级**，与业务并发上限正交。
//
// 队列按优先级 tier 划分（asynq 加权轮询，防低优先级饿死），而非按 (账户×模型)
// 动态分桶——后者是 DB claim 的职责。Unit 6/8 的各 worker 按语义映射到下列 tier。
package asyncq

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"time"

	"github.com/hibiken/asynq"
)

// 队列优先级 tier（值见 DefaultQueuePriorities）。
const (
	// QueueCritical 动钱 / 强时效任务（如 settle 结算）。
	QueueCritical = "critical"
	// QueueDefault 常规任务（如 submit 提交、fetch 轮询）。
	QueueDefault = "default"
	// QueueLow 兜底 sweep（如 reconcile 对账、expire 过期收敛）。
	QueueLow = "low"
)

// DefaultQueuePriorities 返回队列→权重映射（asynq 加权轮询）。
// 权重是**调度优先级**而非并发上限：critical 被取走的频率更高，但不限制 in-flight 数。
func DefaultQueuePriorities() map[string]int {
	return map[string]int{
		QueueCritical: 6,
		QueueDefault:  3,
		QueueLow:      1,
	}
}

// Config 是 asyncq client/server 的构造参数。
type Config struct {
	// RedisAddr Redis 连接地址（host:port），复用 config.RedisAddr。
	RedisAddr string
	// RedisPassword Redis ACL 密码（评审 #9）；空 = 无密码。
	RedisPassword string
	// RedisTLSEnabled 对 Redis 启用 TLS（评审 #9：生产 mTLS / 网络侧 Redis）。
	RedisTLSEnabled bool
	// Concurrency server worker 池大小（执行层并发吞吐，**非** R15 业务并发上限）。
	// 仅 server 用；≤0 时 asynq 默认取可用 CPU 数。
	Concurrency int
	// Queues 队列→权重；nil 时用 DefaultQueuePriorities()。仅 server 用。
	Queues map[string]int
	// ShutdownTimeout asynq 在停机时等待在途任务的上限；超时后强制取消（评审 #3）。
	// ≤0 时 asynq 默认 8s。仅 server 用。
	ShutdownTimeout time.Duration
	// Logger 注入 slog；nil 时 asynq 走其默认 stdlib logger。
	Logger *slog.Logger
}

func (c Config) redisOpt() asynq.RedisClientOpt {
	opt := asynq.RedisClientOpt{
		Addr:     c.RedisAddr,
		Password: c.RedisPassword,
	}
	if c.RedisTLSEnabled {
		// ServerName 由 asynq 从 Addr 推导；用系统根证书校验。
		opt.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	return opt
}

// Client 是 Asynq client 的薄封装，用于 enqueue 任务（Unit 6 起使用）。
//
// 构造是惰性的（不连接 Redis）；调用 Ping() 才建连，用作启动期 fail-fast。
// 内嵌 *asynq.Client 暴露 Enqueue / EnqueueContext / Close / Ping。
type Client struct {
	*asynq.Client
}

// NewClient 构造 Asynq client。
//
// TODO(Unit 6): main.go enqueue worker 装配本 Client 并在停机序列加 defer client.Close()。
func NewClient(cfg Config) *Client {
	return &Client{Client: asynq.NewClient(cfg.redisOpt())}
}

// Server 是 Asynq server 的薄封装，负责取任务并执行（重试/退避由 asynq 兜底）。
type Server struct {
	inner *asynq.Server
}

// NewServer 构造 Asynq server。queues 为空时用默认优先级 tier。
func NewServer(cfg Config) *Server {
	queues := cfg.Queues
	if len(queues) == 0 {
		queues = DefaultQueuePriorities()
	}
	acfg := asynq.Config{
		Concurrency:     cfg.Concurrency,
		Queues:          queues,
		ShutdownTimeout: cfg.ShutdownTimeout,
	}
	if cfg.Logger != nil {
		acfg.Logger = slogAdapter{l: cfg.Logger}
	}
	return &Server{inner: asynq.NewServer(cfg.redisOpt(), acfg)}
}

// Ping 同步探活（asynq 内部用 context.Background()，无 deadline）。
// 启动期 fail-fast 应优先用 PingContext 加超时（评审 #3）。
func (s *Server) Ping() error { return s.inner.Ping() }

// PingContext 在 ctx 截止前等待 Redis ping；超时返回 ctx.Err()。
//
// asynq 的 Ping 自身无 deadline，Redis「可连但不回」会无限挂起阻塞启动（评审 #3）。
// 超时返回后底层 goroutine 仍会随 TCP 超时自然结束——启动期 fail-fast 路径会随即退出进程，
// 故该 goroutine 残留无害。
func (s *Server) PingContext(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() { errCh <- s.inner.Ping() }()
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Start 非阻塞启动 server（asynq 内部起 worker goroutine 处理任务）。
// handler 通常为 asynq.NewServeMux()，Unit 6/8 在其上注册各 task 类型的 handler。
func (s *Server) Start(handler asynq.Handler) error { return s.inner.Start(handler) }

// Shutdown 优雅停机：停止取新任务 + 等在途任务完成（asynq 内部按 ShutdownTimeout 兜底）。
func (s *Server) Shutdown() { s.inner.Shutdown() }

// ShutdownWithTimeout 在 timeout 内优雅停机；超时返回 false（调用方记录并继续）。
//
// asynq.Shutdown() 本身无外层 deadline——worker 阻塞会让进程永挂、pool.Close defer 不执行
// （评审 #3）。本方法给停机加外层上界，保证总能继续到后续 cleanup。
func (s *Server) ShutdownWithTimeout(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		s.inner.Shutdown()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// slogAdapter 把 asynq 内部日志路由进 slog（保持全局 JSON 结构化日志一致）。
//
// asynq.Logger 的 Fatal 映射到 slog.Error（**不** os.Exit）：不让第三方库
// 在我方未预期的时机杀进程，失败由我方 handler/启动 ping 显式处理。
type slogAdapter struct {
	l *slog.Logger
}

func (s slogAdapter) Debug(args ...any) { s.l.Debug(joinArgs(args)) }
func (s slogAdapter) Info(args ...any)  { s.l.Info(joinArgs(args)) }
func (s slogAdapter) Warn(args ...any)  { s.l.Warn(joinArgs(args)) }
func (s slogAdapter) Error(args ...any) { s.l.Error(joinArgs(args)) }
func (s slogAdapter) Fatal(args ...any) {
	s.l.Error(joinArgs(args), slog.String("asynq_level", "fatal"))
}

func joinArgs(args []any) string {
	return "asynq: " + fmt.Sprint(args...)
}

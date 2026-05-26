// admin-cli 共享的 service 构造逻辑。
//
// 所有子命令（account create / account recharge / drift-check）通过 OpenServices
// 拿到统一构造好的 PG 连接池 + LedgerService + Reconciler；调用方负责 defer cleanup()。
//
// 设计原则：
//   - fail-fast：任一构造步骤失败立即返错，CLI 直接退出非零
//   - 显式优于隐式（CLAUDE.md §四 #6）：所有依赖在 CLIServices 中显式持有，便于测试 / 排错
//   - admin-cli 不接 --created-by flag（计划 R17 + ADR）：Actor 由各子命令写死为
//     `Actor{Type: ActorTypeCLI, ID: "bootstrap"}`，本文件不暴露该构造点（避免漂移）
//
// Reconciler 在 CLI 路径上**仅**用于 drift-check 子命令一次性调用 RunOnce —— 不启动后台
// goroutine、不抢 advisory lock（lock 抢占逻辑在 Reconciler.Run 内部；RunOnce 直接调
// 跳过该步骤）。
package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sunxin-git/api-gateway/internal/config"
	"github.com/sunxin-git/api-gateway/internal/ledger"
	"github.com/sunxin-git/api-gateway/internal/obs"
	"github.com/sunxin-git/api-gateway/internal/outbox"
)

// CLIServices 含所有 admin-cli 共享的服务依赖。
//
// 调用方拿到后应立即 defer cleanup()；cleanup 负责关闭 pgxpool（其他依赖无状态）。
type CLIServices struct {
	// Pool pgxpool 连接池；CLI 单进程短生命周期，MaxConns=25 足够。
	Pool *pgxpool.Pool
	// Service 已 wire 好 outbox + log 的 LedgerService 实例。
	Service *ledger.PostgresService
	// Reconciler 已构造的 reconciler；CLI 仅调 RunOnce，不调用 Run。
	Reconciler *ledger.Reconciler
	// Log slog logger（admin-cli 输出到 stderr，避免污染 stdout JSON）。
	Log *slog.Logger
	// Metrics 完整 Prometheus 指标容器；CLI 路径不暴露 /metrics，但 reconciler 需要它 bump 计数。
	Metrics *obs.Metrics
}

// OpenServices 读取配置 → 建 pgxpool → 构造 LedgerService + Reconciler。
//
// 任一步骤失败立即返错（不构造 partial state），调用方一般 `return err` 让 cobra 用非零退出码退出。
// 返回的 cleanup 函数关闭 pool；调用方应在每个 RunE 入口 `defer cleanup()`，即使中途出错也安全。
//
// ctx 用于 pgxpool.NewWithConfig 与 Ping；调用方一般传 cmd.Context()。
func OpenServices(ctx context.Context) (*CLIServices, func(), error) {
	cfg, err := config.Load(".env.local")
	if err != nil {
		return nil, func() {}, fmt.Errorf("加载配置失败: %w", err)
	}

	// admin-cli 日志输出到 stderr（obs.NewLogger 默认即 stderr），不污染 stdout 上的 JSON 输出。
	logger, err := obs.NewLogger(cfg.LogLevel)
	if err != nil {
		return nil, func() {}, fmt.Errorf("初始化 logger 失败: %w", err)
	}

	poolCfg, err := pgxpool.ParseConfig(cfg.PGDSN)
	if err != nil {
		return nil, func() {}, fmt.Errorf("解析 PGDSN 失败: %w", err)
	}
	// CLI 单进程短生命周期：MaxConns 25 已足够。
	// Reconciler 启动时会用 advisory lock 占一个连接（CLI 走 RunOnce 不走 Run，无此问题）。
	poolCfg.MaxConns = 25
	poolCfg.MinConns = 1
	poolCfg.MaxConnLifetime = 30 * time.Minute
	poolCfg.MaxConnIdleTime = 5 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, func() {}, fmt.Errorf("pgxpool.NewWithConfig 失败: %w", err)
	}

	// fail-fast Ping：DB 不可达立即返错，避免 RunE 内部出 cryptic 错误。
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, func() {}, fmt.Errorf("PG Ping 失败（DSN=%s）: %w", cfg.PGDSN, err)
	}

	metrics := obs.NewMetrics("admin-cli", "dev")
	publisher := outbox.NewPostgresPublisher()
	service := ledger.NewPostgresService(pool, publisher, logger)

	reconciler := ledger.NewReconciler(service, pool, ledger.ReconcilerConfig{
		// CLI drift-check 走 RunOnce 一次，不依赖 ticker/initialDelay；保留默认即可。
		DriftAction: cfg.LedgerDriftAction,
		Log:         logger,
		Metrics:     metrics,
	})

	svc := &CLIServices{
		Pool:       pool,
		Service:    service,
		Reconciler: reconciler,
		Log:        logger,
		Metrics:    metrics,
	}
	cleanup := func() {
		pool.Close()
	}
	return svc, cleanup, nil
}

// cliActor 返回 admin-cli 子命令统一使用的 Actor。
//
// **不接受**任何 --created-by flag 或参数（计划 R17 + ADR：P0 admin-cli 写死、不可信）；
// 本 helper 集中暴露唯一构造点，防止散落各处出现 ActorID 漂移。
func cliActor() ledger.Actor {
	return ledger.Actor{Type: ledger.ActorTypeCLI, ID: "bootstrap"}
}

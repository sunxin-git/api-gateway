// api-gateway 进程入口（Phase 2 工作流 E Unit 10：pgxpool + reconciler + /readyz DB ping）。
//
// 启动流程：
//  1. 加载配置（默认值 → .env.local → 进程 env，env 胜出，缺失必填项 fail-fast）。
//  2. 初始化可观测性（slog logger / Prometheus metrics / OTel tracer provider）。
//  3. **新增**：连接 PostgreSQL（pgxpool），Ping fail-fast；defer pool.Close()。
//  4. **新增**：构造 LedgerService（PostgresService）+ OutboxPublisher + Reconciler。
//  5. **新增**：注册 /readyz "postgres" 探针（pool.Ping）。
//  6. 装配 httpapi.Server（Gin engine + 中间件链 + 骨架端点）。
//  7. **新增**：启动 ReconcilerController（goroutine + SIGUSR1 暂停 handler，仅 Unix）。
//  8. 监听 SIGINT/SIGTERM，收到信号后 30s graceful shutdown，超时强制 Close。
//
// 优雅停机顺序（计划 Unit 10 §graceful shutdown）：
//
//	SIGTERM/INT → HTTP server.Shutdown (30s) → ReconcilerController.Stop → pgxpool.Close
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"

	"github.com/sunxin-git/api-gateway/internal/admin"
	"github.com/sunxin-git/api-gateway/internal/admintoken"
	"github.com/sunxin-git/api-gateway/internal/audit"
	"github.com/sunxin-git/api-gateway/internal/config"
	"github.com/sunxin-git/api-gateway/internal/httpapi"
	"github.com/sunxin-git/api-gateway/internal/httpapi/middleware"
	"github.com/sunxin-git/api-gateway/internal/ledger"
	"github.com/sunxin-git/api-gateway/internal/obs"
	"github.com/sunxin-git/api-gateway/internal/outbox"
)

// 通过 ldflags 注入；Phase 1 默认值。
//
//	go build -ldflags "-X main.version=v1.0.0 -X main.commit=abcdef"
var (
	version = "dev"
	commit  = "dev"
)

// pgxpool 默认参数（与 internal/ledger/testutil.go 测试池一致量级）。
//
//   - MaxConns=25：Phase 2 单实例 QPS 估算 + reconciler 1 conn + 业务 ~10-20 conn 留余量；
//     若上 D-min HTTP 后实际不够再调高（CLAUDE.md §六：依赖变更走 ADR）。
//   - MinConns=5：避免冷启动首批请求建连抖动。
//   - MaxConnLifetime=30min / MaxConnIdleTime=5min：与 PG 默认 idle_in_transaction_session_timeout 留余量。
const (
	dbMaxConns        = 25
	dbMinConns        = 5
	dbMaxConnLifetime = 30 * time.Minute
	dbMaxConnIdleTime = 5 * time.Minute
	// dbPingTimeout 启动期 pool.Ping 的超时（不可达即 fail-fast）。
	dbPingTimeout = 5 * time.Second
	// shutdownTimeout HTTP graceful shutdown 总预算。
	shutdownTimeout = 30 * time.Second
)

func main() {
	if err := run(); err != nil {
		// 用 stderr 直接打印（此时 logger 可能还未就绪）。
		fmt.Fprintf(os.Stderr, "[FATAL] api-gateway 启动失败: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// 第 1 步：加载配置。
	cfg, err := config.Load(".env.local")
	if err != nil {
		return fmt.Errorf("加载配置失败: %w", err)
	}

	// 第 2 步：初始化可观测性。
	logger, err := obs.NewLogger(cfg.LogLevel)
	if err != nil {
		return fmt.Errorf("初始化 logger 失败: %w", err)
	}
	slog.SetDefault(logger)

	metrics := obs.NewMetrics(version, commit)

	tp, err := obs.NewTracerProvider(cfg.OTelExporter)
	if err != nil {
		return fmt.Errorf("初始化 OTel tracer provider 失败: %w", err)
	}
	otel.SetTracerProvider(tp)
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tp.Shutdown(shutdownCtx); err != nil {
			logger.Error("关闭 OTel tracer provider 失败", slog.String("err", err.Error()))
		}
	}()

	logger.Info("api-gateway 启动",
		slog.String("version", version),
		slog.String("commit", commit),
		slog.String("http_addr", cfg.HTTPAddr),
		slog.String("log_level", cfg.LogLevel),
		slog.String("otel_exporter", cfg.OTelExporter),
		slog.Int("cors_allowed_origins", len(cfg.CORSAllowedOrigins)),
		slog.String("ledger_drift_action", cfg.LedgerDriftAction),
	)

	// 第 3 步：连接 PostgreSQL（pgxpool）。Ping fail-fast。
	pool, err := newPGXPool(cfg.PGDSN)
	if err != nil {
		return fmt.Errorf("初始化 pgxpool 失败: %w", err)
	}
	defer pool.Close()
	logger.Info("pgxpool 初始化成功",
		slog.Int("max_conns", dbMaxConns),
		slog.Int("min_conns", dbMinConns),
		slog.Duration("max_lifetime", dbMaxConnLifetime),
		slog.Duration("max_idle", dbMaxConnIdleTime),
	)

	// 第 4 步：构造 LedgerService + OutboxPublisher + Reconciler。
	publisher := outbox.NewPostgresPublisher()
	ledgerSvc := ledger.NewPostgresService(pool, publisher, logger)
	reconciler := ledger.NewReconciler(ledgerSvc, pool, ledger.ReconcilerConfig{
		Interval:     5 * time.Minute,
		InitialDelay: 30 * time.Second,
		ConfirmDelay: 1 * time.Second,
		DriftAction:  cfg.LedgerDriftAction, // P0 默认 "log"；生产 1-2 周零误报后切 "freeze"
		Log:          logger,
		Metrics:      metrics,
	})

	// 第 5 步：装配 Admin Token 鉴权 + 阀门 + 审计（D-min Unit 7 装配 5-9）。
	adminTokenSvc := admintoken.NewPostgresService(pool, cfg.TokenPepperBytes, logger)
	// CIDR 启动期 sweep：发现 ip_allowlist 异常的 token 立刻 bump metric 告警
	sweepAdminTokenCIDRs(context.Background(), adminTokenSvc, metrics, logger)

	instanceID := hostnameOrUnknown()
	rpm := admintoken.NewInProcessRPM(logger, func() {
		metrics.AdminThrottleRPMColdStartTotal.WithLabelValues(instanceID).Inc()
	})
	defer func() { _ = rpm.Close() }()
	throttle := admintoken.NewPostgresThrottle(pool, rpm, logger)

	auditLogger, auditCleanup, err := buildAuditLogger(cfg, logger)
	if err != nil {
		return fmt.Errorf("初始化 audit logger 失败: %w", err)
	}
	defer auditCleanup()

	adminHandler := admin.NewHandler(ledgerSvc, throttle, &admin.Metrics{
		IdempotencyConflictTotal: metrics.AdminAPIIdempotencyConflictTotal,
	}, logger)

	// 第 6 步：装配 HTTP server + 注册 /readyz 的 postgres 探针 + admin 路由组 + audit-health 探针。
	srv := httpapi.NewServer(cfg, httpapi.Deps{
		Logger:  logger,
		Metrics: metrics,
		Build:   httpapi.BuildInfo{Version: version, Commit: commit},
	})
	srv.AddReadinessCheck("postgres", func(ctx context.Context) error {
		// /readyz handler 内部已加 2s 超时；这里直接 Ping 即可。
		return pool.Ping(ctx)
	})
	// audit-health：Tier1 sink 写失败累加后 readyz 关闸（计划 Unit 7 §1.5）。
	srv.AddReadinessCheck("admin_audit_health", func(_ context.Context) error {
		if hasAuditWriteFailure(metrics) {
			return errors.New("admin audit Tier1 write failures present (see admin_audit_write_failed_total); 需重启进程恢复")
		}
		return nil
	})

	// 第 7 步：注册 admin 路由组。
	registerAdminRoutes(srv, adminHandler, adminTokenSvc, throttle, auditLogger, metrics)

	// 第 8 步：启动 ReconcilerController（goroutine + SIGUSR1 handler；Windows 是 no-op）。
	// parentCtx 用 Background：由 controller.Stop 终止，不依赖外部 signal ctx。
	reconCtrl := ledger.NewReconcilerController(context.Background(), reconciler, logger, metrics)
	reconCtrl.Start()

	// 第 9 步：信号驱动的优雅停机。
	serverErr := make(chan error, 1)
	go func() {
		var startErr error
		if cfg.ListenTLS {
			logger.Info("启动 TLS 监听", slog.String("cert", cfg.TLSCertPath))
			startErr = srv.StartTLS(cfg.TLSCertPath, cfg.TLSKeyPath)
		} else {
			startErr = srv.Start()
		}
		if startErr != nil && !errors.Is(startErr, http.ErrServerClosed) {
			serverErr <- startErr
		}
		close(serverErr)
	}()

	sigCh := make(chan os.Signal, 1)
	// SIGUSR1 不在此处监听：由 ReconcilerController（Unix 平台）单独注册。
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		logger.Info("收到信号，开始优雅停机", slog.String("signal", sig.String()))
	case err := <-serverErr:
		if err != nil {
			// HTTP server 异常退出：仍按顺序 cleanup reconciler / pool 再返回。
			logger.Error("HTTP server 异常退出，触发 cleanup",
				slog.String("err", err.Error()))
			reconCtrl.Stop()
			return fmt.Errorf("HTTP server 异常退出: %w", err)
		}
		// serverErr 关闭但无 error → 正常 Shutdown 路径走过；继续 cleanup。
	}

	// graceful shutdown 顺序（计划 Unit 10）：
	//  1. HTTP server.Shutdown（拒新连接 + 等存活请求完成，最长 30s）
	//  2. ReconcilerController.Stop（cancel goroutine + cleanup signal handler）
	//  3. pool.Close（defer，函数末尾执行）
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("HTTP server 停机异常", slog.String("err", err.Error()))
		// Shutdown 内部已调用 Close 兜底，这里只记录不再返回错误。
	}

	reconCtrl.Stop()

	logger.Info("api-gateway 已退出")
	return nil
}

// newPGXPool 构造 pgxpool 并 Ping fail-fast。
//
// 失败语义：
//   - DSN 解析失败 → wrap error 返回（不可达，启动失败）
//   - pgxpool.NewWithConfig 失败 → wrap error 返回
//   - Ping 失败 → 先 Close pool 再返回 error（让 main fail-fast 退出）
func newPGXPool(dsn string) (*pgxpool.Pool, error) {
	if dsn == "" {
		return nil, errors.New("PGDSN 为空（config 校验本该拦住，请检查）")
	}
	pcfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("pgxpool.ParseConfig: %w", err)
	}
	pcfg.MaxConns = dbMaxConns
	pcfg.MinConns = dbMinConns
	pcfg.MaxConnLifetime = dbMaxConnLifetime
	pcfg.MaxConnIdleTime = dbMaxConnIdleTime

	ctx, cancel := context.WithTimeout(context.Background(), dbPingTimeout)
	defer cancel()

	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, fmt.Errorf("pgxpool.NewWithConfig: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pool.Ping（DB 不可达）: %w", err)
	}
	return pool, nil
}

// =============================================================================
// D-min Unit 7 helpers
// =============================================================================

// hostnameOrUnknown 取 os.Hostname；失败返 "unknown"。
//
// 用作 RPM cold-start metric 的 instance_id 标签；运维通过 dashboard 看到
// 短期内多个 instance_id 的 cold-start spike 即 OOM / liveness 重启信号。
func hostnameOrUnknown() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unknown"
	}
	return h
}

// sweepAdminTokenCIDRs 启动期扫所有活跃 Admin Token 的 ip_allowlist，
// 发现损坏（解析失败 / 空数组）→ bump admin_token_corrupt_total + log critical。
//
// 不阻塞启动（这是诊断信号，不是 hard gate）；schema 已用 cidr[] 类型保证不应发生。
func sweepAdminTokenCIDRs(ctx context.Context, svc *admintoken.PostgresService, m *obs.Metrics, log *slog.Logger) {
	tokens, err := svc.List(ctx)
	if err != nil {
		log.Warn("启动期 admin token CIDR sweep 跳过（List 失败）",
			slog.String("err", err.Error()))
		return
	}
	for _, tok := range tokens {
		if len(tok.AllowedCIDRs) == 0 {
			log.Error("admin token ip_allowlist 为空（fail-closed：该 token 将拒绝所有请求）",
				slog.Int64("token_id", tok.ID),
				slog.String("description", tok.Description),
			)
			m.AdminTokenCorruptTotal.WithLabelValues(
				int64ToLabel(tok.ID), "empty_cidr_list",
			).Inc()
			continue
		}
		// 防御性二次解析（pgx 已解析过；这里若失败说明类型库变了）
		for _, p := range tok.AllowedCIDRs {
			if !p.IsValid() {
				log.Error("admin token ip_allowlist 含非法 CIDR（fail-closed）",
					slog.Int64("token_id", tok.ID),
					slog.String("cidr_repr", p.String()),
				)
				m.AdminTokenCorruptTotal.WithLabelValues(
					int64ToLabel(tok.ID), "malformed_cidr",
				).Inc()
				break
			}
		}
	}
	log.Info("admin token CIDR sweep 完成", slog.Int("scanned", len(tokens)))
}

// buildAuditLogger 按 config 决定 Tier1/Tier2 sink 组合。
//
//   - GATEWAY_ENV=production：Tier1 SyncFileSink（必须）+ Tier2 AsyncStderrSink
//   - 其他环境：path 设了走 SyncFileSink；未设走 AsyncStderrSink fallback for both
//
// 返回 cleanup func 必须 defer 调用。
func buildAuditLogger(cfg *config.Config, log *slog.Logger) (audit.AuditLogger, func(), error) {
	tier2 := audit.NewAsyncStderrSink()
	var tier1 audit.Sink
	if path := cfg.AdminAuditTier1Path; path != "" {
		fs, err := audit.NewSyncFileSink(path)
		if err != nil {
			return nil, func() {}, fmt.Errorf("打开 audit Tier1 文件 %q: %w", path, err)
		}
		tier1 = fs
		log.Info("audit Tier1 同步落盘", slog.String("path", path))
	} else {
		// dev/test fallback：Tier1 也走 stderr（无 fsync 保证；log warn 让运维知晓）
		log.Warn("ADMIN_AUDIT_HIGH_VALUE_LOG_PATH 未设置，Tier1 fallback 到 stderr（无 fsync 保证）")
		tier1 = tier2
	}
	logger := audit.NewLogger(tier1, tier2, log)
	cleanup := func() { _ = logger.Close() }
	return logger, cleanup, nil
}

// hasAuditWriteFailure 检查 admin_audit_write_failed_total 是否累加过。
//
// 实现：用 prometheus.Gather 读 CounterVec 所有 child 值；任一 > 0 即视为失败。
// 不缓存：每次 /readyz 调用都现读，保证最新状态。
func hasAuditWriteFailure(m *obs.Metrics) bool {
	mfs, err := m.Registry.Gather()
	if err != nil {
		return false // 读 metric 失败时不关闸（避免次级故障扩散）
	}
	for _, mf := range mfs {
		if mf.GetName() != "gateway_admin_audit_write_failed_total" {
			continue
		}
		for _, child := range mf.GetMetric() {
			if child.Counter != nil && child.Counter.GetValue() > 0 {
				return true
			}
		}
	}
	return false
}

// registerAdminRoutes 把 admin handler 5 个 endpoint 挂在 /admin/v1 路由组下，
// 装好完整 5 件套中间件链 + HSTS。
//
// 链顺序（plan Unit 4）：
//
//	HSTS → AdminBodyLimit → AdminTokenAuth → AdminThrottle →
//	  AdminScope(handler-specific) → AdminAudit → handler
func registerAdminRoutes(
	srv *httpapi.Server,
	h *admin.Handler,
	tokenSvc admintoken.Service,
	thr admintoken.Throttle,
	auditLogger audit.AuditLogger,
	m *obs.Metrics,
) {
	g := srv.Engine().Group("/admin/v1")
	g.Use(
		middleware.HSTS(),
		middleware.AdminBodyLimit(m.AdminAPIBodyTooLargeTotal),
		middleware.AdminTokenAuth(tokenSvc, m.AdminAPIAuthFailedTotal),
		middleware.AdminThrottle(thr, m.AdminAPIQuotaExceededTotal),
	)

	scope := func(name string) gin.HandlerFunc {
		return middleware.AdminScope(tokenSvc, name, m.AdminAPIAuthFailedTotal)
	}

	g.POST("/business-accounts",
		scope("business_account:create"),
		middleware.AdminAudit(auditLogger, thr, m.AdminAuditWriteFailedTotal),
		h.CreateAccount,
	)
	g.POST("/business-accounts/:id/recharge",
		scope("business_account:recharge"),
		middleware.AdminAudit(auditLogger, thr, m.AdminAuditWriteFailedTotal),
		h.Recharge,
	)
	g.POST("/business-accounts/:id/refund",
		scope("business_account:refund"),
		middleware.AdminAudit(auditLogger, thr, m.AdminAuditWriteFailedTotal),
		h.Refund,
	)
	g.GET("/business-accounts/:id/balance",
		scope("business_account:read"),
		middleware.AdminAudit(auditLogger, thr, m.AdminAuditWriteFailedTotal),
		h.GetBalance,
	)
	// whoami 无需 scope；任一已鉴权 token 可调
	g.GET("/whoami",
		middleware.AdminAudit(auditLogger, thr, m.AdminAuditWriteFailedTotal),
		h.Whoami,
	)
}

// int64ToLabel int64 → metric label 字符串。
func int64ToLabel(n int64) string {
	if n == 0 {
		return "unknown"
	}
	return fmt.Sprintf("%d", n)
}

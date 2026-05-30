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
// 优雅停机顺序（计划 Unit 10 §graceful shutdown；Unit 1 加入 Asynq）：
//
//	SIGTERM/INT → HTTP server.Shutdown (30s) → Asynq server.Shutdown（仅 AsyncEnabled）
//	  → ReconcilerController.Stop → pgxpool.Close
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
	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"

	"github.com/sunxin-git/api-gateway/internal/admin"
	"github.com/sunxin-git/api-gateway/internal/admintoken"
	"github.com/sunxin-git/api-gateway/internal/asyncq"
	"github.com/sunxin-git/api-gateway/internal/audit"
	"github.com/sunxin-git/api-gateway/internal/businesskey"
	"github.com/sunxin-git/api-gateway/internal/channel"
	"github.com/sunxin-git/api-gateway/internal/config"
	"github.com/sunxin-git/api-gateway/internal/crypto"
	"github.com/sunxin-git/api-gateway/internal/httpapi"
	"github.com/sunxin-git/api-gateway/internal/httpapi/middleware"
	"github.com/sunxin-git/api-gateway/internal/ledger"
	"github.com/sunxin-git/api-gateway/internal/obs"
	"github.com/sunxin-git/api-gateway/internal/outbox"
	"github.com/sunxin-git/api-gateway/internal/relay"
	"github.com/sunxin-git/api-gateway/internal/relay/video"
	"github.com/sunxin-git/api-gateway/internal/task"
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
	// redisPingTimeout 启动期 Asynq/Redis ping 的超时（评审 #3；与 dbPingTimeout 同风格）。
	redisPingTimeout = 5 * time.Second
	// asynqShutdownTimeout Asynq 优雅停机外层上界（< shutdownTimeout，留 HTTP 停机余量；评审 #3）。
	asynqShutdownTimeout = 20 * time.Second
	// upstreamHTTPTimeout relay 调上游 chat completions 的客户端总超时（plan §决策 D2）。
	// 同步非流式场景 60s 足够；超时 → ErrUpstreamTimeout → Release reserve + 504。
	upstreamHTTPTimeout = 60 * time.Second
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

	// 第 7bis 步：装配 + 注册业务 relay 路由组 /v1（仅 RelayEnabled=true 时）。
	// admin-only 部署（RelayEnabled=false）跳过：不构造 businesskey / catalog / adapter，
	// /v1 不存在（业务请求 404）。fail-fast：catalog 校验失败拒启动。
	if cfg.RelayEnabled {
		bizKeySvc := businesskey.NewPostgresService(pool, cfg.TokenPepperBytes, logger)
		defer func() { _ = bizKeySvc.Close() }()

		bizRPM := businesskey.NewInProcessRPM(logger, func() {
			metrics.BusinessThrottleRPMColdStartTotal.WithLabelValues(instanceID).Inc()
		})
		defer func() { _ = bizRPM.Close() }()

		relayHandler, err := buildRelayHandler(cfg, ledgerSvc, metrics, logger)
		if err != nil {
			return fmt.Errorf("装配 relay handler 失败: %w", err)
		}
		registerBusinessRoutes(srv, relayHandler, bizKeySvc, bizRPM, auditLogger, metrics)
		logger.Info("业务 relay 路由已注册",
			slog.String("path", "/v1/chat/completions"),
			slog.String("gateway_model", cfg.RelayModelName),
			slog.String("upstream_provider", cfg.RelayUpstreamProviderType),
		)
	} else {
		logger.Info("RelayEnabled=false：admin-only 部署，/v1 业务路由未注册")
	}

	// 第 7ter 步：异步执行基座（Asynq + Redis；ADR-0006 / Unit 1）。仅 AsyncEnabled 时装配。
	//   - Redis ping fail-fast（与 pgxpool 同风格；不可达即拒启动）
	//   - server 起 worker goroutine 处理任务；当前 mux 为空，handler 在 Unit 6/8 注册
	//   - AsyncEnabled=false 时完全不碰 Redis（现有 admin-only / 同步 relay 部署零 Redis 依赖）
	var asyncSrv *asyncq.Server
	var videoScheduler *asyncq.Scheduler
	var videoEnqueuer *task.AsynqEnqueuer
	if cfg.AsyncEnabled {
		asyncSrv = asyncq.NewServer(asyncq.Config{
			RedisAddr:       cfg.RedisAddr,
			RedisPassword:   cfg.RedisPassword,
			RedisTLSEnabled: cfg.RedisTLSEnabled,
			Concurrency:     cfg.AsyncConcurrency,
			ShutdownTimeout: asynqShutdownTimeout,
			Logger:          logger,
		})
		// Redis ping fail-fast，带超时（评审 #3：asynq Ping 自身无 deadline）。
		// Scheduler 复用同一 Redis，无需再 ping。
		pingCtx, pingCancel := context.WithTimeout(context.Background(), redisPingTimeout)
		pingErr := asyncSrv.PingContext(pingCtx)
		pingCancel()
		if pingErr != nil {
			return fmt.Errorf("Asynq/Redis 不可达（AsyncEnabled=true）: %w", pingErr)
		}

		// 视频异步任务闭环（Unit 6）：仅 VideoRelayEnabled 时装配 submit/settle/sweep handler +
		// 周期 sweep scheduler。VideoRelayEnabled ⟹ AsyncEnabled（config.validate 已保证），故在此嵌套。
		mux := asynq.NewServeMux()
		if cfg.VideoRelayEnabled {
			taskSvc, enq, err := buildVideoTaskService(cfg, pool, ledgerSvc, logger)
			if err != nil {
				asyncSrv.Shutdown()
				return fmt.Errorf("装配视频异步任务 service 失败: %w", err)
			}
			videoEnqueuer = enq
			taskSvc.RegisterHandlers(mux)

			// 回调入口（Unit 8）：仅配置了回调 base URL 时注册公网回调路由（鉴权 = URL 路径 per-task
			// token，不走业务 key 链）。未配 → 纯轮询兜底（submit 不带回调 URL，sweep 主动 Poll）。
			if cfg.VideoCallbackBaseURL != "" {
				registerVideoCallbackRoutes(srv, taskSvc, logger)
				logger.Info("视频回调入口已注册",
					slog.String("route", task.CallbackPathPrefix+"/:task_id/:token"))
			}

			videoScheduler = asyncq.NewScheduler(asyncq.Config{
				RedisAddr:       cfg.RedisAddr,
				RedisPassword:   cfg.RedisPassword,
				RedisTLSEnabled: cfg.RedisTLSEnabled,
				Logger:          logger,
			})
			if err := registerVideoSweepSchedule(videoScheduler); err != nil {
				_ = videoEnqueuer.Close()
				asyncSrv.Shutdown()
				return fmt.Errorf("注册视频 sweep 周期任务失败: %w", err)
			}
		}

		if err := asyncSrv.Start(mux); err != nil {
			asyncSrv.Shutdown() // 评审 #7：Start 失败也要停已起的内部 goroutine
			if videoEnqueuer != nil {
				_ = videoEnqueuer.Close()
			}
			return fmt.Errorf("启动 Asynq server 失败: %w", err)
		}
		logger.Info("Asynq 异步基座已启动",
			slog.String("redis_addr", cfg.RedisAddr),
			slog.Int("concurrency", cfg.AsyncConcurrency),
			slog.Bool("redis_tls", cfg.RedisTLSEnabled),
			slog.Bool("video_relay", cfg.VideoRelayEnabled),
		)

		// server 起来后再启动 scheduler（worker 已就绪，避免 sweep job 入队后无人取）。
		if videoScheduler != nil {
			if err := videoScheduler.Start(); err != nil {
				videoScheduler.Shutdown()
				asyncSrv.Shutdown()
				_ = videoEnqueuer.Close()
				return fmt.Errorf("启动视频 sweep scheduler 失败: %w", err)
			}
			logger.Info("视频异步任务闭环已启动（submit/settle handler + 周期兜底 sweep）",
				slog.String("gateway_model", cfg.VideoRelayModelName))
		}
	} else {
		logger.Info("AsyncEnabled=false：异步基座未启用，不连接 Redis")
	}

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
			shutdownAsyncStack(asyncSrv, videoScheduler, videoEnqueuer, asynqShutdownTimeout, logger)
			reconCtrl.Stop()
			return fmt.Errorf("HTTP server 异常退出: %w", err)
		}
		// serverErr 关闭但无 error → 正常 Shutdown 路径走过；继续 cleanup。
	}

	// graceful shutdown 顺序（计划 Unit 10；Unit 1 加入 Asynq；Unit 6b 加入 scheduler + enqueuer）：
	//  1. HTTP server.Shutdown（拒新连接 + 等存活请求完成，最长 30s）
	//  2. video sweep scheduler.Shutdown（停止入队新周期任务；仅 VideoRelayEnabled）
	//  3. Asynq server.Shutdown（停止取新任务 + 等在途 worker 完成；仅 AsyncEnabled）
	//  4. video enqueuer.Close（worker 已排空，关底层 client；仅 VideoRelayEnabled）
	//  5. ReconcilerController.Stop（cancel goroutine + cleanup signal handler）
	//  6. pool.Close（defer，函数末尾执行）
	// Asynq 在 pool.Close 之前停：worker（Unit 6 起）会用 DB，须先让其排空再关连接池。
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("HTTP server 停机异常", slog.String("err", err.Error()))
		// Shutdown 内部已调用 Close 兜底，这里只记录不再返回错误。
	}

	shutdownAsyncStack(asyncSrv, videoScheduler, videoEnqueuer, asynqShutdownTimeout, logger)

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

// =============================================================================
// F-min Unit 7 helpers（业务 relay 装配）
// =============================================================================

// buildRelayHandler 构造 relay catalog + adapter + handler（plan §Unit 7 装配）。
//
//   - catalog：EnvCatalog 单条；RequireHTTPS = (production)；校验失败 fail-fast
//   - upstream client：标准库 net/http，60s 总超时（无第三方依赖，CLAUDE.md §三）
//   - adapter：OpenAI 兼容（MVP 唯一）
//   - HandlerMetrics：从 obs.Metrics 抽 relay 子集注入（解耦 relay 不 import obs）
func buildRelayHandler(cfg *config.Config, l ledger.Service, m *obs.Metrics, log *slog.Logger) (*relay.RelayHandler, error) {
	catalog, err := relay.NewEnvCatalog(relay.CatalogConfig{
		ModelName:             cfg.RelayModelName,
		UpstreamProviderType:  cfg.RelayUpstreamProviderType,
		UpstreamBaseURL:       cfg.RelayUpstreamBaseURL,
		UpstreamAPIKey:        cfg.RelayUpstreamAPIKey,
		UpstreamModelName:     cfg.RelayUpstreamModelName,
		PriceInputPer1MMinor:  cfg.RelayPriceInputPer1MMinor,
		PriceOutputPer1MMinor: cfg.RelayPriceOutputPer1MMinor,
		MaxContextTokens:      cfg.RelayMaxContextTokens,
		RequireHTTPS:          cfg.GatewayEnv == config.EnvProduction,
	})
	if err != nil {
		return nil, err
	}

	upstreamClient := &http.Client{Timeout: upstreamHTTPTimeout}
	adapter := relay.NewOpenAICompatAdapter(upstreamClient)

	handlerMetrics := &relay.HandlerMetrics{
		RequestTotal:         m.RelayRequestTotal,
		ReserveFailedTotal:   m.RelayReserveFailedTotal,
		SettleFailedTotal:    m.RelaySettleFailedTotal,
		TokenCostMinor:       m.RelayTokenCostMinor,
		UpstreamDuration:     m.RelayUpstreamDuration,
		UpstreamMissingUsage: m.RelayUpstreamMissingUsage,
	}

	return relay.NewRelayHandler(catalog, adapter, l, handlerMetrics, log), nil
}

// registerBusinessRoutes 把业务 relay endpoint 挂在 /v1 路由组下，装好业务中间件链。
//
// 链顺序（plan §Unit 4 业务链）：
//
//	HSTS → BusinessBodyLimit(1MiB) → BusinessKeyAuth → BusinessRPM → BusinessAudit → handler
//
// 与 admin 链对称：BodyLimit 在鉴权前（pre-auth 拒绝超大 body）；Audit 在 handler 前最后一环
// （defer 模式保证一定 emit）。
func registerBusinessRoutes(
	srv *httpapi.Server,
	h *relay.RelayHandler,
	keySvc businesskey.Service,
	rpm *businesskey.InProcessRPM,
	auditLogger audit.AuditLogger,
	m *obs.Metrics,
) {
	g := srv.Engine().Group("/v1")
	g.Use(
		middleware.HSTS(),
		middleware.BusinessBodyLimit(m.BusinessAPIBodyTooLargeTotal),
		middleware.BusinessKeyAuth(keySvc, m.BusinessAPIAuthFailedTotal),
		middleware.BusinessRPM(rpm, m.BusinessAPIRateLimitedTotal),
		middleware.BusinessAudit(auditLogger, m.AdminAuditWriteFailedTotal),
	)
	g.POST("/chat/completions", h.ChatCompletion)
}

// =============================================================================
// Phase 2 Unit 6 helpers（视频异步任务闭环装配）
// =============================================================================

// 视频 sweep 周期（cron 描述符；执行隔离 / 兜底频率，**非**业务硬上限）。Unique TTL ≈ 周期，
// 防多副本各自 Scheduler 在同一窗口重复入队同一 sweep（sweep 是幂等全表扫描，一窗一 job 足矣）。
const (
	sweepFetchInterval   = "@every 30s" // 回调缺失 / 入队丢失 / 卡 SETTLING 的轮询兜底（时效较敏感）
	sweepRecoverInterval = "@every 1m"  // 崩溃恢复 fail-closed（lease 过期才动）
	sweepExpireInterval  = "@every 5m"  // 终态收敛兜底（最长执行期 48h，低频足矣）
	sweepOrphanInterval  = "@every 5m"  // 孤儿 reserve 回收（低频）
)

// shutdownAsyncStack 按序停止视频异步栈（停机 / 异常退出共用，避免两处重复）：
//
//  1. scheduler.Shutdown：先停，不再入队新 sweep job
//  2. server.ShutdownWithTimeout：等在途 worker 排空（worker 可能仍经 enqueuer 入队 settle）
//  3. enqueuer.Close：worker 已排空，关底层 asynq client
//
// 各参数允许为 nil（AsyncEnabled=false / VideoRelayEnabled=false 时部分未装配）。
func shutdownAsyncStack(
	asyncSrv *asyncq.Server,
	sched *asyncq.Scheduler,
	enq *task.AsynqEnqueuer,
	timeout time.Duration,
	logger *slog.Logger,
) {
	if sched != nil {
		logger.Info("停止视频 sweep scheduler（停止入队新周期任务）")
		sched.Shutdown()
	}
	if asyncSrv != nil {
		logger.Info("停止 Asynq server（等在途任务完成）")
		if ok := asyncSrv.ShutdownWithTimeout(timeout); !ok {
			logger.Error("Asynq server 停机超时，强制继续 cleanup", slog.Duration("timeout", timeout))
		}
	}
	if enq != nil {
		if err := enq.Close(); err != nil {
			logger.Error("关闭 video enqueuer client 失败", slog.String("err", err.Error()))
		}
	}
}

// buildVideoTaskService 装配视频异步任务 service（Unit 6 装配链）：
//
//	keyring(GATEWAY_KEK_V1) → channel.Service(凭据解密) → video catalog + seedance adapter →
//	  task.Service(提交流程 + 状态机 CAS + settle + sweep) + AsynqEnqueuer(submit/settle 入队)
//
// 返回 enqueuer 供停机序列在 server 排空后 Close。任一 fail-fast 校验失败拒启动（catalog 自洽校验
// 在 NewEnvVideoCatalog 内，与 RelayEnabled 同分工）。
func buildVideoTaskService(
	cfg *config.Config,
	pool *pgxpool.Pool,
	ledgerSvc ledger.Service,
	log *slog.Logger,
) (*task.Service, *task.AsynqEnqueuer, error) {
	keyring, err := buildKeyring(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("装配凭据 keyring: %w", err)
	}
	channelSvc := channel.NewPostgresService(pool, keyring, log)

	catalog, err := video.NewEnvVideoCatalog(video.CatalogConfig{
		GatewayModelName:       cfg.VideoRelayModelName,
		UpstreamProviderType:   cfg.VideoRelayProviderType,
		UpstreamBaseURL:        cfg.VideoRelayUpstreamBaseURL,
		UpstreamModelName:      cfg.VideoRelayUpstreamModelName,
		ChannelName:            cfg.VideoRelayChannelName,
		RequireHTTPS:           cfg.GatewayEnv == config.EnvProduction,
		Price480pPer1MMinor:    cfg.VideoRelayPrice480pPer1MMinor,
		Price720pPer1MMinor:    cfg.VideoRelayPrice720pPer1MMinor,
		Price1080pPer1MMinor:   cfg.VideoRelayPrice1080pPer1MMinor,
		BillingMultiplierBP:    cfg.VideoRelayBillingMultiplierBP,
		DurationMinSeconds:     cfg.VideoRelayDurationMinSeconds,
		DurationMaxSeconds:     cfg.VideoRelayDurationMaxSeconds,
		DurationDefaultSeconds: cfg.VideoRelayDurationDefaultSeconds,
		FpsDefault:             cfg.VideoRelayFpsDefault,
		FpsMax:                 cfg.VideoRelayFpsMax,
		Ratios:                 cfg.VideoRelayRatios,
		RatioDefault:           cfg.VideoRelayRatioDefault,
		ResolutionDefault:      cfg.VideoRelayResolutionDefault,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("装配 video catalog: %w", err)
	}

	// 上游 HTTP 客户端：总超时兜底（单次 Submit/Poll 由 ctx deadline 控制，见 task.Service 超时配置）。
	// 禁止跟随重定向（defense-in-depth：上游被劫持 / DNS 污染时防二次 SSRF 到内网；可信 Ark endpoint
	// 正常不重定向，3xx 交由 adapter 当非 2xx 分类，见 seedance_adapter doc）。
	videoUpstreamClient := &http.Client{
		Timeout: upstreamHTTPTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	adapter := video.NewSeedanceAdapter(videoUpstreamClient)

	enqueuer := task.NewAsynqEnqueuer(asyncq.NewClient(asyncq.Config{
		RedisAddr:       cfg.RedisAddr,
		RedisPassword:   cfg.RedisPassword,
		RedisTLSEnabled: cfg.RedisTLSEnabled,
	}))

	// R15 并发上限值解析器（Unit 8）：默认值来自 config；per-(account,model) 覆写预留 Unit 11 admin。
	limits, err := video.NewConcurrencyLimits(int32(cfg.VideoRelayConcurrencyDefault), nil)
	if err != nil {
		_ = enqueuer.Close()
		return nil, nil, fmt.Errorf("装配并发上限解析器: %w", err)
	}

	svc, err := task.NewService(task.Config{
		Pool:            pool,
		Ledger:          ledgerSvc,
		Adapter:         adapter,
		Catalog:         catalog,
		Creds:           channelSvc,
		Enqueuer:        enqueuer,
		Logger:          log,
		WorkerID:        hostnameOrUnknown(),
		Limits:          limits,                   // Unit 8：账户×模型并发上限（DB 原子 claim 的 cap）
		CallbackBaseURL: cfg.VideoCallbackBaseURL, // Unit 8：空 → 纯轮询兜底
		// 各超时 / sweep 阈值用 task 包默认。
	})
	if err != nil {
		_ = enqueuer.Close()
		return nil, nil, fmt.Errorf("构造 task.Service: %w", err)
	}
	return svc, enqueuer, nil
}

// buildKeyring 从 cfg.GatewayKEKV1 装配单版本 KEK keyring（ADR-0006 决策 4；P1 扩多版本 V2…）。
func buildKeyring(cfg *config.Config) (*crypto.Keyring, error) {
	kekV1, err := crypto.DecodeKEK(cfg.GatewayKEKV1)
	if err != nil {
		return nil, fmt.Errorf("解码 GATEWAY_KEK_V1: %w", err)
	}
	return crypto.NewKeyring(map[int32][]byte{1: kekV1})
}

// registerVideoSweepSchedule 把 4 个周期 sweep 注册到 scheduler（QueueLow；Unique 防多副本重复入队）。
func registerVideoSweepSchedule(sched *asyncq.Scheduler) error {
	entries := []struct {
		cron, typ string
		uniqueTTL time.Duration
	}{
		{sweepFetchInterval, task.TypeReconcileFetch, 30 * time.Second},
		{sweepRecoverInterval, task.TypeRecover, 1 * time.Minute},
		{sweepExpireInterval, task.TypeExpire, 5 * time.Minute},
		{sweepOrphanInterval, task.TypeOrphanReserve, 5 * time.Minute},
	}
	for _, e := range entries {
		if _, err := sched.Register(e.cron, e.typ, asyncq.QueueLow, e.uniqueTTL); err != nil {
			return fmt.Errorf("注册 sweep %q: %w", e.typ, err)
		}
	}
	return nil
}

// 回调端点全局限速参数（Unit 8；defense-in-depth，主防线是 per-task token 校验 + 去抖）。
const (
	callbackThrottleRPS   = 50  // 稳态每秒令牌；远超正常上游回调速率
	callbackThrottleBurst = 100 // 突发桶容量
)

// registerVideoCallbackRoutes 注册上游回调入口（Unit 8）：
//
//	POST {task.CallbackPathPrefix}/:task_id/:token
//
// 中间件链：CallbackThrottle（全局限速）→ CallbackBodyLimit（64 KiB）→ handler。
// **不**走业务 key 鉴权链（上游调用；鉴权 = URL 路径 per-task token，在 svc.HandleCallback 内常量时间校验）。
func registerVideoCallbackRoutes(srv *httpapi.Server, svc *task.Service, logger *slog.Logger) {
	h := httpapi.NewVideoCallbackHandler(svc, logger)
	throttle := middleware.NewCallbackThrottle(callbackThrottleRPS, callbackThrottleBurst)
	g := srv.Engine().Group(task.CallbackPathPrefix)
	g.Use(
		throttle.Middleware(),
		middleware.CallbackBodyLimit(),
	)
	g.POST("/:task_id/:token", h.Handle)
}

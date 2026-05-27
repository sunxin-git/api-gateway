package obs

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics 是项目的 Prometheus 指标集合容器。
// 使用自有 *prometheus.Registry 而非 prometheus.DefaultRegisterer，
// 原因：避免引入第三方包顺带注册的默认指标，确保 /metrics 端点暴露内容可控。
type Metrics struct {
	// Registry 自有注册中心，/metrics handler 用 promhttp.HandlerFor(Registry, ...) 暴露。
	Registry *prometheus.Registry

	// HTTPRequestDuration 记录每个 HTTP 请求的端到端耗时（秒）。
	// 标签：method（GET/POST/...）、path（如 "/healthz"，需注意 Gin route 模板而非真实 URL）、
	//       status（HTTP 状态码字符串，如 "200"）。
	HTTPRequestDuration *prometheus.HistogramVec

	// BuildInfo 版本信息 gauge，永远为 1，标签携带 version 与 commit。
	BuildInfo *prometheus.GaugeVec

	// PanicTotal 进程内 panic 计数（由 recover middleware 自增）。
	PanicTotal prometheus.Counter

	// ===== Phase 2 工作流 E：drift reconciler 指标（Unit 7） =====
	//
	// 说明：account_id 作为标签的指标在 P0 阶段账户数 < 5000 可接受；
	// P1 视生产规模决定是否改为仅 count + log（pass-2 高基数风险）。

	// LedgerDriftTotal 真 drift 计数（二次确认后仍不一致）。
	// 标签：account_id、reason（固定 "drift"）、action（"log" | "freeze"）。
	LedgerDriftTotal *prometheus.CounterVec

	// LedgerDriftFalsePositiveTotal 误报计数（首次不一致，二次确认一致）。
	// 标签：account_id。
	LedgerDriftFalsePositiveTotal *prometheus.CounterVec

	// ReconcilerRunDuration 每轮 RunOnce 端到端耗时（秒）。
	ReconcilerRunDuration prometheus.Histogram

	// ReconcilerOverloadTotal 单轮跑过阈值的次数（trip-wire）。
	// 标签：reason（"accounts" | "duration"）。
	ReconcilerOverloadTotal *prometheus.CounterVec

	// ReconcilerPanicTotal reconciler goroutine 内 recover 捕获的 panic 次数。
	ReconcilerPanicTotal prometheus.Counter

	// ReconcilerPaused 暂停状态 gauge（0=运行中，1=暂停）。SIGUSR1 由 U10 main.go 切换。
	ReconcilerPaused prometheus.Gauge

	// ReconcilerSkippedTotal advisory lock 抢占导致 Run 跳过启动的次数。
	ReconcilerSkippedTotal prometheus.Counter

	// LedgerRebuildStuckTotal RebuildBalance 卡在 rebuild_in_progress 超阈值的账户次数。
	// 标签：account_id。
	LedgerRebuildStuckTotal *prometheus.CounterVec

	// ===== Phase 2 工作流 D-min（Unit 7）：Admin API 指标 =====
	//
	// 命名约定：gateway_admin_*；与现有 gateway_* 系列一致。

	// AdminAPIQuotaExceededTotal 阀门触发次数（throttle 中间件 + handler 预检）。
	// 标签：quota_type（rpm | circuit_open | single_recharge | daily_recharge | single_refund |
	//                  daily_refund | daily_create）、token_id。
	AdminAPIQuotaExceededTotal *prometheus.CounterVec

	// AdminAPIAuthFailedTotal 鉴权失败计数。
	// 标签：reason（missing_header | bad_scheme | empty_token | token_invalid |
	//             ip_not_allowed | invalid_ip | insufficient_scope | internal_error）。
	AdminAPIAuthFailedTotal *prometheus.CounterVec

	// AdminAPIBodyTooLargeTotal 请求体超 64KiB 拒绝次数；pre-auth 阶段，不带 token_id 标签。
	AdminAPIBodyTooLargeTotal prometheus.Counter

	// AdminAPIIdempotencyConflictTotal 同 external_ref 不同 body 触发 ErrIdempotencyConflict 次数。
	// 标签：token_id。值 > 0 通常是业务系统 bug 或攻击重放。
	AdminAPIIdempotencyConflictTotal *prometheus.CounterVec

	// AdminAPIXFFRejectedTotal 请求源 IP ≠ trusted proxy 但携带 X-Forwarded-For 头时累加。
	// 标签：reason（untrusted_source / malformed_xff）。
	AdminAPIXFFRejectedTotal *prometheus.CounterVec

	// AdminAuditWriteFailedTotal Tier1 / Tier2 sink emit 失败次数。
	// 标签：tier（tier1 | tier2 | unknown）、reason。
	// 值 > 0 让 /readyz 关闸（plan Unit 7 §1.5）。
	AdminAuditWriteFailedTotal *prometheus.CounterVec

	// AdminTokenCorruptTotal 启动期 CIDR sweep 或运行期发现 token 数据损坏次数。
	// 标签：token_id、reason（malformed_cidr / unexpected_field / ...）。
	AdminTokenCorruptTotal *prometheus.CounterVec

	// AdminThrottleRPMColdStartTotal 进程启动时 InProcessRPM 冷启次数（每次进程启动 +1）。
	// 标签：instance_id（main.go 装配时填 hostname / pod name；为空填 "unknown"）。
	// 让运维通过短期内 spike 识别"OOM 重启 / liveness 循环重启绕 RPM"信号。
	AdminThrottleRPMColdStartTotal *prometheus.CounterVec
}

// NewMetrics 构造并注册所有指标。version/commit 通常通过 ldflags 注入；Phase 1 写 "dev"。
func NewMetrics(version, commit string) *Metrics {
	reg := prometheus.NewRegistry()

	httpReqDur := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "gateway_http_request_duration_seconds",
			Help:    "HTTP 请求端到端耗时（秒）",
			Buckets: prometheus.DefBuckets, // 0.005 → 10s
		},
		[]string{"method", "path", "status"},
	)

	buildInfo := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gateway_build_info",
			Help: "进程构建信息（version/commit），值恒为 1。",
		},
		[]string{"version", "commit"},
	)
	buildInfo.WithLabelValues(version, commit).Set(1)

	panicTotal := prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "gateway_panic_total",
			Help: "进程内 recover middleware 捕获的 panic 总次数",
		},
	)

	// ===== Phase 2 工作流 E reconciler 指标 =====

	driftTotal := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gateway_ledger_drift_total",
			Help: "reconciler 二次确认后仍不一致的真 drift 计数",
		},
		[]string{"account_id", "reason", "action"},
	)

	driftFalsePositive := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gateway_ledger_drift_false_positive_total",
			Help: "reconciler 首次发现不一致但 1s 后二次确认一致的误报计数",
		},
		[]string{"account_id"},
	)

	reconRunDur := prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "gateway_reconciler_run_duration_seconds",
			Help:    "reconciler 每轮 RunOnce 端到端耗时（秒）",
			Buckets: []float64{0.1, 0.5, 1, 5, 10, 30, 60, 120, 300},
		},
	)

	reconOverload := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gateway_reconciler_overload_total",
			Help: "reconciler 单轮跑过阈值的次数（trip-wire）",
		},
		[]string{"reason"},
	)

	reconPanic := prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "gateway_reconciler_panic_total",
			Help: "reconciler goroutine 内 recover 捕获的 panic 次数",
		},
	)

	reconPaused := prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "gateway_reconciler_paused",
			Help: "reconciler 暂停状态（0=运行中，1=已暂停）",
		},
	)

	reconSkipped := prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "gateway_reconciler_skipped_total",
			Help: "reconciler 因 PG advisory lock 抢占跳过启动的次数",
		},
	)

	rebuildStuck := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gateway_ledger_rebuild_stuck_total",
			Help: "RebuildBalance 卡在 rebuild_in_progress 超过阈值的账户次数",
		},
		[]string{"account_id"},
	)

	// ===== D-min Unit 7 admin_api_* 指标 =====

	adminQuotaExceeded := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gateway_admin_api_quota_exceeded_total",
			Help: "Admin API 阀门触发次数（rpm / circuit_open / single_recharge / daily_* / single_refund / daily_create）",
		},
		[]string{"quota_type", "token_id"},
	)
	adminAuthFailed := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gateway_admin_api_auth_failed_total",
			Help: "Admin API 鉴权失败计数（missing_header / bad_scheme / token_invalid / ip_not_allowed / insufficient_scope / ...）",
		},
		[]string{"reason"},
	)
	adminBodyTooLarge := prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "gateway_admin_api_body_too_large_total",
			Help: "Admin API 请求体超 64 KiB 拒绝次数（pre-auth；不带 token_id 标签）",
		},
	)
	adminIdempConflict := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gateway_admin_api_idempotency_conflict_total",
			Help: "Admin API 同 external_ref 不同 body 触发 idempotency_conflict 次数",
		},
		[]string{"token_id"},
	)
	adminXFFRejected := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gateway_admin_api_xff_rejected_total",
			Help: "请求源 IP ≠ trusted proxy 但携带 X-Forwarded-For 头时累加（配置错误信号）",
		},
		[]string{"reason"},
	)
	adminAuditWriteFailed := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gateway_admin_audit_write_failed_total",
			Help: "Admin audit sink emit 失败次数；值 > 0 让 /readyz 关闸",
		},
		[]string{"tier", "reason"},
	)
	adminTokenCorrupt := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gateway_admin_token_corrupt_total",
			Help: "Admin token CIDR sweep 发现数据损坏次数",
		},
		[]string{"token_id", "reason"},
	)
	adminRPMColdStart := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gateway_admin_throttle_rpm_cold_start_total",
			Help: "InProcessRPM 冷启次数（每次进程启动 +1）；短期 spike 提示 OOM/liveness 循环重启",
		},
		[]string{"instance_id"},
	)

	reg.MustRegister(
		httpReqDur, buildInfo, panicTotal,
		driftTotal, driftFalsePositive,
		reconRunDur, reconOverload, reconPanic,
		reconPaused, reconSkipped, rebuildStuck,
		adminQuotaExceeded, adminAuthFailed, adminBodyTooLarge,
		adminIdempConflict, adminXFFRejected, adminAuditWriteFailed,
		adminTokenCorrupt, adminRPMColdStart,
	)

	return &Metrics{
		Registry:                         reg,
		HTTPRequestDuration:              httpReqDur,
		BuildInfo:                        buildInfo,
		PanicTotal:                       panicTotal,
		LedgerDriftTotal:                 driftTotal,
		LedgerDriftFalsePositiveTotal:    driftFalsePositive,
		ReconcilerRunDuration:            reconRunDur,
		ReconcilerOverloadTotal:          reconOverload,
		ReconcilerPanicTotal:             reconPanic,
		ReconcilerPaused:                 reconPaused,
		ReconcilerSkippedTotal:           reconSkipped,
		LedgerRebuildStuckTotal:          rebuildStuck,
		AdminAPIQuotaExceededTotal:       adminQuotaExceeded,
		AdminAPIAuthFailedTotal:          adminAuthFailed,
		AdminAPIBodyTooLargeTotal:        adminBodyTooLarge,
		AdminAPIIdempotencyConflictTotal: adminIdempConflict,
		AdminAPIXFFRejectedTotal:         adminXFFRejected,
		AdminAuditWriteFailedTotal:       adminAuditWriteFailed,
		AdminTokenCorruptTotal:           adminTokenCorrupt,
		AdminThrottleRPMColdStartTotal:   adminRPMColdStart,
	}
}

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

	reg.MustRegister(
		httpReqDur, buildInfo, panicTotal,
		driftTotal, driftFalsePositive,
		reconRunDur, reconOverload, reconPanic,
		reconPaused, reconSkipped, rebuildStuck,
	)

	return &Metrics{
		Registry:                      reg,
		HTTPRequestDuration:           httpReqDur,
		BuildInfo:                     buildInfo,
		PanicTotal:                    panicTotal,
		LedgerDriftTotal:              driftTotal,
		LedgerDriftFalsePositiveTotal: driftFalsePositive,
		ReconcilerRunDuration:         reconRunDur,
		ReconcilerOverloadTotal:       reconOverload,
		ReconcilerPanicTotal:          reconPanic,
		ReconcilerPaused:              reconPaused,
		ReconcilerSkippedTotal:        reconSkipped,
		LedgerRebuildStuckTotal:       rebuildStuck,
	}
}

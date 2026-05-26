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

	reg.MustRegister(httpReqDur, buildInfo, panicTotal)

	return &Metrics{
		Registry:            reg,
		HTTPRequestDuration: httpReqDur,
		BuildInfo:           buildInfo,
		PanicTotal:          panicTotal,
	}
}

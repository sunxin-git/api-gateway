package relay

import "github.com/prometheus/client_golang/prometheus"

// HandlerMetrics RelayHandler 用到的 Prometheus 指标集合（plan §Unit 5 + R14）。
//
// 设计选择：解耦让 relay 包不直接 import obs（避免循环）；main.go Unit 7 装配时
// 从 obs.Metrics 抽出子集注入到本 struct。
//
// 所有字段 nil-safe：handler 在 emit 前会 nil 检查（测试无需注入完整 metric set）。
type HandlerMetrics struct {
	// RequestTotal 每次 relay 完成记一次（含成功 / 失败 / 上游 4xx 等所有路径）。
	// 标签：model（gateway model name）、upstream_status（"200" / "4xx" / "5xx" / "timeout" / "unreachable" / "malformed"）。
	RequestTotal *prometheus.CounterVec

	// ReserveFailedTotal Reserve 阶段失败计数。
	// 标签：reason（"insufficient_balance" / "account_frozen" / "account_not_found" / "version_conflict" / "internal"）。
	ReserveFailedTotal *prometheus.CounterVec

	// SettleFailedTotal Commit / Release 永久失败计数（3 次重试仍失败 → orphan reserve 信号）。
	// 标签：phase（"commit" / "release"）、reason（"version_conflict" / "internal"）。
	SettleFailedTotal *prometheus.CounterVec

	// TokenCostMinor 累计花费（minor unit）；业务对账用。
	// 标签：model（gateway model name）。
	TokenCostMinor *prometheus.CounterVec

	// UpstreamDuration 上游 HTTP 请求端到端耗时（秒）。
	// 标签：model（gateway model name）、status（http status 字符串）。
	UpstreamDuration *prometheus.HistogramVec

	// UpstreamMissingUsage 上游返 200 但缺 usage 字段次数（plan §决策 D2 兜底信号）。
	// 标签：model（gateway model name）。
	// 值 > 0 提示运维：provider 协议异常或 endpoint 配错。
	UpstreamMissingUsage *prometheus.CounterVec
}

// safeRequestTotal nil-safe bump。
func (m *HandlerMetrics) safeRequestTotal(model, upstreamStatus string) {
	if m == nil || m.RequestTotal == nil {
		return
	}
	m.RequestTotal.WithLabelValues(model, upstreamStatus).Inc()
}

func (m *HandlerMetrics) safeReserveFailed(reason string) {
	if m == nil || m.ReserveFailedTotal == nil {
		return
	}
	m.ReserveFailedTotal.WithLabelValues(reason).Inc()
}

func (m *HandlerMetrics) safeSettleFailed(phase, reason string) {
	if m == nil || m.SettleFailedTotal == nil {
		return
	}
	m.SettleFailedTotal.WithLabelValues(phase, reason).Inc()
}

func (m *HandlerMetrics) safeTokenCost(model string, costMinor int64) {
	if m == nil || m.TokenCostMinor == nil || costMinor <= 0 {
		return
	}
	m.TokenCostMinor.WithLabelValues(model).Add(float64(costMinor))
}

func (m *HandlerMetrics) safeUpstreamDuration(model, status string, seconds float64) {
	if m == nil || m.UpstreamDuration == nil {
		return
	}
	m.UpstreamDuration.WithLabelValues(model, status).Observe(seconds)
}

func (m *HandlerMetrics) safeUpstreamMissingUsage(model string) {
	if m == nil || m.UpstreamMissingUsage == nil {
		return
	}
	m.UpstreamMissingUsage.WithLabelValues(model).Inc()
}

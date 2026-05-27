package admin

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/sunxin-git/api-gateway/internal/admintoken"
	"github.com/sunxin-git/api-gateway/internal/httpapi/middleware"
	"github.com/sunxin-git/api-gateway/internal/ledger"
)

// Metrics handler 级别的 Prometheus 指标集合（不与 obs.Metrics 重复；Unit 7 装配时合并注册）。
//
// 设计：handler 不直接 import obs；通过本地 Metrics struct + 注入构造，便于单测。
type Metrics struct {
	// IdempotencyConflictTotal 同 external_ref 不同 body 触发 LedgerService.ErrIdempotencyConflict 的次数。
	// 标签：token_id。值 > 0 通常是业务系统 bug 或攻击重放信号。
	IdempotencyConflictTotal *prometheus.CounterVec
}

// Handler Admin API HTTP 处理器（计划 Unit 5）。
//
// 持有：
//   - ledger.Service：账户 / 充值 / 退款 / 余额 4 个核心方法
//   - admintoken.Throttle：daily / single recharge & refund 预检；whoami 快照查询
//   - Metrics（可空）：handler 级 metric
//   - logger：诊断日志（业务级 audit 走 audit.Logger middleware，不在此 handler 内）
//
// Constructor 校验所有非可空依赖；nil 直接 panic（启动期 fail-fast）。
type Handler struct {
	ledger   ledger.Service
	throttle admintoken.Throttle
	metrics  *Metrics
	logger   *slog.Logger
}

// NewHandler 构造 admin handler。
//
// 入参：
//   - l：ledger.Service（与 admin-cli 共用 PostgresService）
//   - t：admintoken.Throttle（middleware AdminThrottle 也用同一个实例）
//   - m：admin-handler metric set；nil 表示禁用 metric（测试 / 简化场景）
//   - log：slog logger；不能为 nil
func NewHandler(l ledger.Service, t admintoken.Throttle, m *Metrics, log *slog.Logger) *Handler {
	if l == nil {
		panic("admin.NewHandler: ledger.Service 不能为 nil")
	}
	if t == nil {
		panic("admin.NewHandler: admintoken.Throttle 不能为 nil")
	}
	if log == nil {
		panic("admin.NewHandler: log 不能为 nil")
	}
	return &Handler{ledger: l, throttle: t, metrics: m, logger: log}
}

// =============================================================================
// Whoami（任何已鉴权 token 可调；不需 scope；计划 R1 第 5 endpoint）
// =============================================================================

// Whoami GET /admin/v1/whoami。
//
// 返回：token meta + 阀门快照 + 今日用量 + 熔断状态。
// 不返回：token_hash / ip_allowlist 具体 CIDR 列表（仅返回 CIDR 数量）。
//
// 性能：所有 token 元数据已在 ctx；额外 2 次 PG SELECT（usage + circuit）。
//
// outcome_code = "ok"（audit Tier2，read-only 无副作用）。
func (h *Handler) Whoami(c *gin.Context) {
	vr := middleware.GetAdminTokenValidation(c)
	if vr == nil || vr.Token == nil {
		// 防御性：中间件应已注入；缺失 → 500
		respondError(c, errorMapping{
			status:      http.StatusInternalServerError,
			code:        "internal_error",
			message:     "服务内部错误（whoami 缺少 token 上下文）",
			outcomeCode: "internal_error",
		})
		return
	}
	tok := vr.Token

	usage, err := h.throttle.GetUsageToday(c.Request.Context(), tok.ID)
	if err != nil {
		MapError(c, err)
		return
	}
	circuit, err := h.throttle.GetCircuitSnapshot(c.Request.Context(), tok.ID)
	if err != nil {
		MapError(c, err)
		return
	}

	resp := WhoamiResponse{
		TokenID:              tok.ID,
		Description:          tok.Description,
		Scopes:               tok.Scopes,
		IPAllowlistCIDRCount: len(tok.AllowedCIDRs),
		ExpiresAt:            formatTimePtr(tok.ExpiresAt),
		ThrottleLimits: ThrottleLimits{
			SingleRechargeMax:       tok.SingleRechargeMax,
			DailyRechargeQuotaLimit: tok.DailyRechargeQuotaLimit,
			SingleRefundMax:         tok.SingleRefundMax,
			DailyRefundQuotaLimit:   tok.DailyRefundQuotaLimit,
			DailyAccountCreateLimit: tok.DailyAccountCreateLimit,
			RequestsPerMinute:       tok.RequestsPerMinute,
			CircuitBreakerEnabled:   tok.CircuitBreakerEnabled,
		},
		TodayUsageUTC: UsageBlock{
			RechargeTotalMinor: usage.RechargeTotalMinor,
			RefundTotalMinor:   usage.RefundTotalMinor,
			AccountCreateCount: usage.AccountCreateCount,
		},
		CircuitState: CircuitStateDTO{
			Open:               circuit.Open,
			TrippedUntil:       formatTimePtr(circuit.TrippedUntil),
			ErrorCountInWindow: circuit.ErrorCount,
		},
		ServerTimeUTC: time.Now().UTC().Format(time.RFC3339),
	}
	middleware.SetAuditOutcomeCode(c, "ok")
	c.JSON(http.StatusOK, resp)
}

// =============================================================================
// 共享 helpers
// =============================================================================

// formatTimePtr time.Time 指针 → RFC3339 字符串指针；nil → nil。
func formatTimePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}

// formatTime time.Time → RFC3339 字符串。
func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// actorFromCtx 从 gin.Context 取 admin token → 构造 ledger.Actor。
//
// 调用方应保证 middleware AdminTokenAuth 已注入 token；缺失返 zero Actor + ok=false。
func actorFromCtx(c *gin.Context) (ledger.Actor, bool) {
	vr := middleware.GetAdminTokenValidation(c)
	if vr == nil || vr.Token == nil {
		return ledger.Actor{}, false
	}
	return ledger.Actor{
		Type: ledger.ActorTypeAdminToken,
		ID:   strconv.FormatInt(vr.Token.ID, 10),
	}, true
}

// tokenFromCtx 从 gin.Context 取 admin token（合并 nil 检查）。
func tokenFromCtx(c *gin.Context) *admintoken.Token {
	vr := middleware.GetAdminTokenValidation(c)
	if vr == nil {
		return nil
	}
	return vr.Token
}

// bumpIdempotencyConflict 安全 bump（metrics 可能 nil）。
func (h *Handler) bumpIdempotencyConflict(tokenID int64) {
	if h.metrics == nil || h.metrics.IdempotencyConflictTotal == nil {
		return
	}
	h.metrics.IdempotencyConflictTotal.WithLabelValues(strconv.FormatInt(tokenID, 10)).Inc()
}

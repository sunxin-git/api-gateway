package admin

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/sunxin-git/api-gateway/internal/admintoken"
	"github.com/sunxin-git/api-gateway/internal/httpapi/middleware"
	"github.com/sunxin-git/api-gateway/internal/ledger"
)

// ErrorResponse Admin API 统一错误响应（计划 R12）。
//
// 形状：{"error": {"code": "...", "message": "...", "request_id": "..."}}
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody 错误响应里层。
type ErrorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

// errorMapping 单条错误映射。
type errorMapping struct {
	status      int
	code        string
	message     string
	outcomeCode string // 用于 audit middleware 记录细粒度 outcome
}

// errorTable 把 ledger / admintoken sentinel 一一映射到 HTTP 状态与 error.code。
//
// 与计划 D4 表对齐；命中外加 `outcomeCode` 让 audit middleware 记录具体业务语义
// （区别于宽泛的 "client_error" / "internal_error"）。
var errorTable = []struct {
	target  error
	mapping errorMapping
}{
	// ===== ledger sentinel =====
	{ledger.ErrInvalidAmount, errorMapping{http.StatusBadRequest, "invalid_amount", "金额必须大于 0", "invalid_amount"}},
	{ledger.ErrAccountNotFound, errorMapping{http.StatusNotFound, "account_not_found", "业务账户不存在", "account_not_found"}},
	{ledger.ErrAccountAlreadyExists, errorMapping{http.StatusConflict, "account_already_exists", "业务账户已存在", "account_already_exists"}},
	{ledger.ErrAccountFrozen, errorMapping{http.StatusConflict, "account_frozen", "账户已冻结，请联系运营", "account_frozen"}},
	{ledger.ErrInsufficientUsed, errorMapping{http.StatusConflict, "insufficient_used", "退款金额超过已结算金额", "insufficient_used"}},
	{ledger.ErrIdempotencyConflict, errorMapping{http.StatusConflict, "idempotency_conflict", "请求体与已记录操作不一致，无法重复处理", "idempotency_conflict"}},
	{ledger.ErrVersionConflict, errorMapping{http.StatusServiceUnavailable, "version_conflict", "服务繁忙，请重试", "version_conflict"}},

	// ===== throttle sentinel =====
	{admintoken.ErrSingleRechargeExceeded, errorMapping{http.StatusTooManyRequests, "single_recharge_exceeded", "单笔充值超过阀门", "single_recharge_exceeded"}},
	{admintoken.ErrDailyRechargeExceeded, errorMapping{http.StatusTooManyRequests, "daily_recharge_quota_exceeded", "今日充值额度已用尽", "daily_recharge_quota_exceeded"}},
	{admintoken.ErrSingleRefundExceeded, errorMapping{http.StatusTooManyRequests, "single_refund_exceeded", "单笔退款超过阀门", "single_refund_exceeded"}},
	{admintoken.ErrDailyRefundExceeded, errorMapping{http.StatusTooManyRequests, "daily_refund_quota_exceeded", "今日退款额度已用尽", "daily_refund_quota_exceeded"}},
	{admintoken.ErrDailyCreateExceeded, errorMapping{http.StatusTooManyRequests, "daily_create_exceeded", "今日创建账户数超阀", "daily_create_exceeded"}},
}

// MapError 把 service / throttle error 映射为 (status, ErrorResponse)。
//
// 调用方：handler 在调 LedgerService / Throttle 失败后调本函数返响应。
// 未命中 errorTable → 500 internal_error（不暴露原 error 给客户端）。
//
// 副作用：调 SetAuditOutcomeCode 把细粒度业务 code 注入 ctx，audit middleware
// 在 defer emit 时读取并写入 audit record 的 OutcomeCode 字段。
func MapError(c *gin.Context, err error) {
	if err == nil {
		return
	}
	for _, m := range errorTable {
		if errors.Is(err, m.target) {
			respondError(c, m.mapping)
			return
		}
	}
	// 未命中：500
	respondError(c, errorMapping{
		status:      http.StatusInternalServerError,
		code:        "internal_error",
		message:     "服务内部错误",
		outcomeCode: "internal_error",
	})
	// 仍把原 err 推给 c.Errors 让 access log + recover 路径可见
	_ = c.Error(err)
}

// RespondInvalidBody 400 invalid_request_body 响应（入参 bind 失败 / validate 失败时调）。
func RespondInvalidBody(c *gin.Context, detail string) {
	msg := "请求体不合法"
	if detail != "" {
		msg = msg + ": " + detail
	}
	respondError(c, errorMapping{
		status:      http.StatusBadRequest,
		code:        "invalid_request_body",
		message:     msg,
		outcomeCode: "invalid_request_body",
	})
}

// respondError 统一响应 helper：写 status + JSON + 记录 audit outcome code。
func respondError(c *gin.Context, m errorMapping) {
	if m.outcomeCode != "" {
		middleware.SetAuditOutcomeCode(c, m.outcomeCode)
	}
	c.AbortWithStatusJSON(m.status, ErrorResponse{
		Error: ErrorBody{
			Code:      m.code,
			Message:   m.message,
			RequestID: middleware.GetRequestID(c),
		},
	})
}

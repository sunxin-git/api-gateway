package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/sunxin-git/api-gateway/internal/httpapi/middleware"
	"github.com/sunxin-git/api-gateway/internal/ledger"
)

// =============================================================================
// CreateAccount —— POST /admin/v1/business-accounts
// =============================================================================

// CreateAccount 处理创建业务账户请求。
//
// 流程（计划 Unit 5 两步式 + R1 / R10 / R11）：
//
//  1. bind + validate 入参
//  2. Step 1 — throttle.CheckDailyCreate（token.DailyAccountCreateLimit）
//  3. Step 2 — ledger.CreateAccount（actor=admin_token:<id>）
//  4. Step 3 — 成功 → throttle.RecordSuccessfulCreate（首次成功才累加；ErrAccountAlreadyExists 不算）
//  5. 返回 201 + AccountResponse
//
// 错误：
//   - 入参非法 → 400 invalid_request_body
//   - DailyCreate 超阀门 → 429 daily_create_exceeded
//   - ErrAccountAlreadyExists → 409 account_already_exists
//   - 其他 ledger 错误 → 按 MapError
func (h *Handler) CreateAccount(c *gin.Context) {
	var req CreateAccountRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		RespondInvalidBody(c, err.Error())
		return
	}
	if msg := req.validate(); msg != "" {
		RespondInvalidBody(c, msg)
		return
	}

	token := tokenFromCtx(c)
	actor, ok := actorFromCtx(c)
	if !ok {
		respondError(c, errorMapping{
			status:      http.StatusInternalServerError,
			code:        "internal_error",
			message:     "服务内部错误（缺少 actor 上下文）",
			outcomeCode: "internal_error",
		})
		return
	}

	// Step 1 — 两步式预检
	if err := h.throttle.CheckDailyCreate(c.Request.Context(), token); err != nil {
		MapError(c, err)
		return
	}

	// Step 2 — Ledger call
	id := strings.TrimSpace(req.ID)
	metadata := normalizeMetadata(req.Metadata)
	acc, err := h.ledger.CreateAccount(c.Request.Context(), actor, ledger.CreateAccountParams{
		ID:                id,
		IsolationRequired: req.IsolationRequired,
		Metadata:          metadata,
	})
	if err != nil {
		MapError(c, err)
		return
	}

	// Step 3 — 累加 daily counter（仅首次成功；UNIQUE 冲突走 ErrAccountAlreadyExists 不到此）
	if recErr := h.throttle.RecordSuccessfulCreate(c.Request.Context(), token.ID); recErr != nil {
		// 失败不影响业务返回；只 log + audit Reason
		_ = c.Error(recErr)
	}

	middleware.SetAuditOutcomeCode(c, "ok")
	c.JSON(http.StatusCreated, AccountResponse{
		ID:                acc.ID,
		Status:            acc.Status,
		IsolationRequired: acc.IsolationRequired,
		CreatedAt:         formatTime(acc.CreatedAt),
	})
}

// =============================================================================
// Recharge —— POST /admin/v1/business-accounts/:id/recharge
// =============================================================================

// Recharge 处理充值请求。
//
// 流程：
//
//  1. bind + validate；path :id 与 body 入参分开
//  2. Step 1 — throttle.CheckSingleRecharge + CheckDailyRecharge（D11 两步式）
//  3. Step 2 — ledger.Recharge（actor=admin_token:<id>，IdempotencyKey=external_ref）
//  4. Step 3 — outcome=FreshlyWritten 时 throttle.RecordSuccessfulRecharge；
//     IdempotentReplay 时**不**累加（避免业务系统重试膨胀配额）
//
// 错误：ErrIdempotencyConflict → 409 + bump idempotency_conflict metric + Tier1 audit
func (h *Handler) Recharge(c *gin.Context) {
	accountID := strings.TrimSpace(c.Param("id"))
	if accountID == "" || len(accountID) > MaxAccountIDLen {
		RespondInvalidBody(c, "account id 非法")
		return
	}

	var req RechargeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		RespondInvalidBody(c, err.Error())
		return
	}
	if msg := req.validate(); msg != "" {
		RespondInvalidBody(c, msg)
		return
	}

	token := tokenFromCtx(c)
	actor, ok := actorFromCtx(c)
	if !ok {
		respondError(c, errorMapping{
			status:      http.StatusInternalServerError,
			code:        "internal_error",
			message:     "服务内部错误（缺少 actor 上下文）",
			outcomeCode: "internal_error",
		})
		return
	}

	// Step 1 — 阀门预检
	if err := h.throttle.CheckSingleRecharge(token, req.Amount); err != nil {
		MapError(c, err)
		return
	}
	if err := h.throttle.CheckDailyRecharge(c.Request.Context(), token, req.Amount); err != nil {
		MapError(c, err)
		return
	}

	// Step 2 — Ledger call
	externalRef := strings.TrimSpace(req.ExternalRef)
	metadata := normalizeMetadata(req.Metadata)
	entry, outcome, err := h.ledger.Recharge(c.Request.Context(), actor, ledger.RechargeParams{
		AccountID:      accountID,
		Amount:         req.Amount,
		CorrelationID:  externalRef, // recharge 不靠 correlation_id 幂等，复用 external_ref
		IdempotencyKey: externalRef,
		CanonicalBody: &ledger.RechargeBody{
			AccountID:   accountID,
			Amount:      req.Amount,
			ExternalRef: externalRef,
		},
		ReferenceType: req.ReferenceType,
		ReferenceID:   req.ReferenceID,
		Metadata:      metadata,
	})
	if err != nil {
		// 幂等冲突单独 bump metric（攻击信号 / 业务 bug）
		if errors.Is(err, ledger.ErrIdempotencyConflict) {
			h.bumpIdempotencyConflict(token.ID)
		}
		MapError(c, err)
		return
	}

	// Step 3 — 仅 FreshlyWritten 累加 daily 配额（D11 关键）
	idempotent := outcome == ledger.WriteOutcomeIdempotentReplay
	if !idempotent {
		if recErr := h.throttle.RecordSuccessfulRecharge(c.Request.Context(), token.ID, req.Amount); recErr != nil {
			_ = c.Error(recErr)
		}
	}

	middleware.SetAuditOutcomeCode(c, "ok")
	c.JSON(http.StatusOK, ledgerEntryResponse(entry, idempotent))
}

// =============================================================================
// Refund —— POST /admin/v1/business-accounts/:id/refund
// =============================================================================

// Refund 处理退款请求。
//
// 流程对称 Recharge：
//
//  1. bind + validate
//  2. throttle.CheckSingleRefund + CheckDailyRefund
//  3. ledger.Refund（correlation_id 复合 UNIQUE 幂等）
//  4. FreshlyWritten → RecordSuccessfulRefund；IdempotentReplay → 跳过
//
// audit：refund 路径无论 outcome 都被 audit middleware 自动判为 Tier1（同步落盘）。
func (h *Handler) Refund(c *gin.Context) {
	accountID := strings.TrimSpace(c.Param("id"))
	if accountID == "" || len(accountID) > MaxAccountIDLen {
		RespondInvalidBody(c, "account id 非法")
		return
	}

	var req RefundRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		RespondInvalidBody(c, err.Error())
		return
	}
	if msg := req.validate(); msg != "" {
		RespondInvalidBody(c, msg)
		return
	}

	token := tokenFromCtx(c)
	actor, ok := actorFromCtx(c)
	if !ok {
		respondError(c, errorMapping{
			status:      http.StatusInternalServerError,
			code:        "internal_error",
			message:     "服务内部错误（缺少 actor 上下文）",
			outcomeCode: "internal_error",
		})
		return
	}

	if err := h.throttle.CheckSingleRefund(token, req.Amount); err != nil {
		MapError(c, err)
		return
	}
	if err := h.throttle.CheckDailyRefund(c.Request.Context(), token, req.Amount); err != nil {
		MapError(c, err)
		return
	}

	correlationID := strings.TrimSpace(req.CorrelationID)
	metadata := normalizeMetadata(req.Metadata)
	entry, outcome, err := h.ledger.Refund(c.Request.Context(), actor, ledger.RefundParams{
		AccountID:     accountID,
		Amount:        req.Amount,
		CorrelationID: correlationID,
		ReferenceType: req.ReferenceType,
		ReferenceID:   req.ReferenceID,
		Metadata:      metadata,
	})
	if err != nil {
		MapError(c, err)
		return
	}

	idempotent := outcome == ledger.WriteOutcomeIdempotentReplay
	if !idempotent {
		if recErr := h.throttle.RecordSuccessfulRefund(c.Request.Context(), token.ID, req.Amount); recErr != nil {
			_ = c.Error(recErr)
		}
	}

	middleware.SetAuditOutcomeCode(c, "ok")
	c.JSON(http.StatusOK, ledgerEntryResponse(entry, idempotent))
}

// =============================================================================
// GetBalance —— GET /admin/v1/business-accounts/:id/balance
// =============================================================================

// GetBalance 查询账户当前余额（含 frozen 状态）。
//
// 流程：无阀门预检 + 直接 ledger.GetBalance；audit Tier2（read-only）。
func (h *Handler) GetBalance(c *gin.Context) {
	accountID := strings.TrimSpace(c.Param("id"))
	if accountID == "" || len(accountID) > MaxAccountIDLen {
		RespondInvalidBody(c, "account id 非法")
		return
	}

	bal, err := h.ledger.GetBalance(c.Request.Context(), accountID)
	if err != nil {
		MapError(c, err)
		return
	}

	resp := BalanceResponse{
		AccountID:     bal.BusinessAccountID,
		Available:     bal.Available,
		Reserved:      bal.Reserved,
		UsedTotal:     bal.UsedTotal,
		RechargeTotal: bal.RechargeTotal,
		RefundTotal:   bal.RefundTotal,
		Frozen:        bal.Frozen,
		FrozenReason:  bal.FrozenReason,
		Version:       bal.Version,
		UpdatedAt:     formatTime(bal.UpdatedAt),
	}
	middleware.SetAuditOutcomeCode(c, "ok")
	c.JSON(http.StatusOK, resp)
}

// =============================================================================
// helpers
// =============================================================================

// ledgerEntryResponse ledger.LedgerEntry → 对外响应 DTO。
func ledgerEntryResponse(entry *ledger.LedgerEntry, idempotent bool) LedgerEntryResponse {
	return LedgerEntryResponse{
		ID:             entry.ID,
		EntryType:      entry.EntryType,
		Amount:         entry.Amount,
		AvailableDelta: entry.AvailableDelta,
		UsedDelta:      entry.UsedDelta,
		CorrelationID:  entry.CorrelationID,
		IdempotencyKey: entry.IdempotencyKey,
		CreatedAt:      formatTime(entry.CreatedAt),
		Idempotent:     idempotent,
	}
}

// normalizeMetadata 把 json.RawMessage 转换为 []byte；空时返 nil（service 转 '{}'::jsonb）。
//
// 防御：传入非法 JSON 时（极少见，因 gin bind 已校验 JSON 完整性）返 nil 而非 panic。
func normalizeMetadata(raw json.RawMessage) []byte {
	if len(raw) == 0 {
		return nil
	}
	// 简单校验：非法 JSON 视作 nil（让 service 用 '{}'）
	if !json.Valid(raw) {
		return nil
	}
	return []byte(raw)
}

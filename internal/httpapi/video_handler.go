package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/sunxin-git/api-gateway/internal/businesskey"
	"github.com/sunxin-git/api-gateway/internal/channel"
	"github.com/sunxin-git/api-gateway/internal/db"
	"github.com/sunxin-git/api-gateway/internal/httpapi/middleware"
	"github.com/sunxin-git/api-gateway/internal/ledger"
	"github.com/sunxin-git/api-gateway/internal/relay"
	"github.com/sunxin-git/api-gateway/internal/relay/video"
	"github.com/sunxin-git/api-gateway/internal/task"
)

// VideoHandler 业务对外异步视频 API 的 handler（Phase 2 / Unit 10）。
//
// 端点（挂在业务 /v1 组下，复用 HSTS→BodyLimit→KeyAuth→RPM→Audit 中间件链）：
//   - POST /v1/video/generations         提交 text_to_video
//   - GET  /v1/video/generations/:id      轮询本地任务状态（强制归属，跨租户 404）
//   - GET  /v1/account/balance            查余额/用量
//
// 提交链（任一前置失败在 reserve **前**短路，无 orphan reserve）：
//
//	鉴权(中间件) → bind → entitlement(403) → 能力校验(400) → EstimateReserveMinor → Submit
//
// 错误形状 OpenAI 兼容（relay.WriteErrorJSON）。错误映射集中在 writeSubmitError / 各 handler。
type VideoHandler struct {
	svc            VideoTaskService
	catalog        video.VideoCatalog
	safetyFactorBP int64
	minTokenFloor  int64
	resultURLTTL   time.Duration
	logger         *slog.Logger
}

// VideoTaskService 是 handler 依赖的任务服务最小接口（DIP；*task.Service 满足，测试可 fake）。
type VideoTaskService interface {
	Submit(ctx context.Context, p task.SubmitParams) (string, error)
	GetForAccount(ctx context.Context, accountID, taskID string) (*task.TaskView, error)
	GetBalance(ctx context.Context, accountID string) (*ledger.Balance, error)
	CheckEntitlement(ctx context.Context, accountID, gatewayModel string) (bool, error)
	// PresignResult 现签产物 URL；accountID 强制归属（结构性防越权，不依赖调用约定）。
	PresignResult(ctx context.Context, accountID, taskID string, ttl time.Duration) (string, error)
}

// NewVideoHandler 构造 handler + fail-fast 必填校验。
func NewVideoHandler(svc VideoTaskService, catalog video.VideoCatalog, safetyFactorBP, minTokenFloor int64, resultURLTTL time.Duration, logger *slog.Logger) *VideoHandler {
	if svc == nil {
		panic("httpapi.NewVideoHandler: svc 不能为 nil")
	}
	if catalog == nil {
		panic("httpapi.NewVideoHandler: catalog 不能为 nil")
	}
	if logger == nil {
		panic("httpapi.NewVideoHandler: logger 不能为 nil")
	}
	if resultURLTTL <= 0 {
		resultURLTTL = 15 * time.Minute
	}
	return &VideoHandler{
		svc:            svc,
		catalog:        catalog,
		safetyFactorBP: safetyFactorBP,
		minTokenFloor:  minTokenFloor,
		resultURLTTL:   resultURLTTL,
		logger:         logger,
	}
}

// errTypePermission 是 OpenAI 协议中权限类错误的 type（relay 未定义此常量，本地用）。
const errTypePermission = "permission_error"

// =============================================================================
// POST /v1/video/generations
// =============================================================================

// Submit 提交 text_to_video 任务。
func (h *VideoHandler) Submit(c *gin.Context) {
	key, ok := businessKeyFromCtx(c)
	if !ok {
		h.respondInternal(c, "缺少业务鉴权上下文")
		return
	}

	var rawBody map[string]any
	if err := c.ShouldBindJSON(&rawBody); err != nil {
		relay.WriteErrorJSON(c, http.StatusBadRequest, relay.ErrTypeInvalidRequest, "invalid_request_body",
			"请求体不合法："+err.Error())
		return
	}

	model, _ := rawBody["model"].(string)
	if strings.TrimSpace(model) == "" {
		relay.WriteErrorJSON(c, http.StatusBadRequest, relay.ErrTypeInvalidRequest, "missing_model",
			"必填字段 model 缺失")
		return
	}
	taskType, _ := rawBody["task_type"].(string)
	if strings.TrimSpace(taskType) == "" {
		taskType = string(video.TaskTypeTextToVideo)
	}

	entry, ok := h.catalog.Lookup(model)
	if !ok || entry == nil {
		// 与同步 /v1/chat/completions 一致：未知 model 属客户端入参错误 → 400（非 404）。
		relay.WriteErrorJSON(c, http.StatusBadRequest, relay.ErrTypeInvalidRequest, "model_not_found",
			"未知 model")
		return
	}

	// === entitlement（按 catalog 规范 model 名校验；R13）===
	entitled, err := h.svc.CheckEntitlement(c.Request.Context(), key.BusinessAccountID, entry.GatewayModelName)
	if err != nil {
		h.logger.Error("video submit: entitlement 查询失败", slog.String("err", err.Error()))
		h.respondInternal(c, "entitlement 查询失败")
		return
	}
	if !entitled {
		relay.WriteErrorJSON(c, http.StatusForbidden, errTypePermission, "model_not_entitled",
			"账户未开通该模型，请联系运营开通")
		return
	}

	// === 能力校验（task_type 支持集 + 参数取值档；R6/R7）===
	validated, verr := entry.Capability.Validate(taskType, rawBody)
	if verr != nil {
		relay.WriteErrorJSON(c, http.StatusBadRequest, video.ErrorType, verr.Code, verr.Message)
		return
	}

	// === reserve 估算（token 上界；Unit 7 权威公式 + 单一配置源 safety/floor）===
	reserveTokens, reserveMinor, err := video.EstimateReserveMinor(validated, entry.Pricing, video.ReserveOptions{
		SafetyFactorBP: h.safetyFactorBP,
		MinTokenFloor:  h.minTokenFloor,
	})
	if err != nil {
		h.logger.Error("video submit: reserve 估算失败（catalog/请求不一致）",
			slog.String("err", err.Error()))
		h.respondInternal(c, "计费估算失败")
		return
	}

	// === 提交（reserve → DB claim → 落 task → 入队；channel 由 task.Service 按名解析）===
	tokenID := key.ID
	taskID, err := h.svc.Submit(c.Request.Context(), task.SubmitParams{
		BusinessAccountID: key.BusinessAccountID,
		ActorTokenID:      &tokenID,
		Entry:             entry,
		Request:           validated,
		ReserveMinor:      reserveMinor,
		ReserveTokens:     reserveTokens,
		MinTokenFloor:     h.minTokenFloor,
	})
	if err != nil {
		h.writeSubmitError(c, err)
		return
	}

	c.JSON(http.StatusOK, videoSubmitResponse{
		ID:       taskID,
		Object:   "video.generation",
		Status:   "queued",
		Model:    entry.GatewayModelName,
		TaskType: taskType,
	})
}

// writeSubmitError 把 task.Submit / ledger / channel 错误映射为 OpenAI 兼容响应。
func (h *VideoHandler) writeSubmitError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, task.ErrConcurrencyLimit):
		relay.WriteErrorJSON(c, http.StatusTooManyRequests, relay.ErrTypeRateLimitExceeded, "concurrency_limit",
			"账户该模型并发任务数已达上限，请稍后重试")
	case errors.Is(err, ledger.ErrInsufficientBalance):
		relay.WriteErrorJSON(c, http.StatusPaymentRequired, relay.ErrTypeInsufficientQuota, "insufficient_quota",
			"账户余额不足，请联系运营充值")
	case errors.Is(err, ledger.ErrAccountFrozen):
		relay.WriteErrorJSON(c, http.StatusPaymentRequired, relay.ErrTypeInsufficientQuota, "account_frozen",
			"账户已冻结，请联系运营")
	case errors.Is(err, ledger.ErrAccountNotFound):
		relay.WriteErrorJSON(c, http.StatusUnauthorized, relay.ErrTypeInvalidAPIKey, "account_not_found",
			"业务账户不存在")
	case errors.Is(err, ledger.ErrVersionConflict):
		relay.WriteErrorJSON(c, http.StatusServiceUnavailable, relay.ErrTypeServerError, "temporarily_unavailable",
			"服务繁忙，请重试")
	case errors.Is(err, channel.ErrChannelNotFound), errors.Is(err, channel.ErrDecryptFailed):
		// 渠道未就绪 / 凭据解密失败：模型当前不可用（fail-closed）。reserve 已在 Submit 内回退（无 orphan）。
		h.logger.Error("video submit: 渠道不可用", slog.String("err", err.Error()))
		relay.WriteErrorJSON(c, http.StatusServiceUnavailable, relay.ErrTypeServerError, "model_unavailable",
			"模型暂不可用，请联系运营")
	default:
		h.logger.Error("video submit: 内部错误", slog.String("err", err.Error()))
		h.respondInternal(c, "提交失败")
	}
}

// =============================================================================
// GET /v1/video/generations/:id
// =============================================================================

// Get 轮询本地任务状态（强制归属；跨租户 → 404 不泄露存在性）。
func (h *VideoHandler) Get(c *gin.Context) {
	key, ok := businessKeyFromCtx(c)
	if !ok {
		h.respondInternal(c, "缺少业务鉴权上下文")
		return
	}
	taskID := c.Param("id")

	view, err := h.svc.GetForAccount(c.Request.Context(), key.BusinessAccountID, taskID)
	if err != nil {
		if errors.Is(err, task.ErrTaskNotFound) {
			relay.WriteErrorJSON(c, http.StatusNotFound, relay.ErrTypeInvalidRequest, "task_not_found",
				"任务不存在")
			return
		}
		h.logger.Error("video get: 查询失败", slog.String("err", err.Error()))
		h.respondInternal(c, "查询失败")
		return
	}

	status := apiStatus(view.Status, view.ErrorCode)
	resp := videoTaskResponse{
		ID:        view.ID,
		Object:    "video.generation",
		Status:    status,
		Model:     view.Model,
		TaskType:  view.TaskType,
		CreatedAt: view.SubmittedAt.Unix(),
		UpdatedAt: view.UpdatedAt.Unix(),
	}
	switch status {
	case "failed", "cancelled", "expired":
		resp.ErrorCode = view.ErrorCode
		resp.ErrorMessage = view.ErrorMessage
		// SETTLE_FAILED（COMPLETED 来源、error_code 空）映射为 failed：上游可能已成功，但网关侧结算/
		// 转存未完成、产物无法经网关交付 → 给业务一个稳定终态码停止轮询，转人工对账（不谎报 succeeded）。
		if resp.ErrorCode == "" {
			resp.ErrorCode = "settlement_failed"
			resp.ErrorMessage = "任务结算未完成，请联系运营对账"
		}
	case "succeeded":
		// 产物现签 URL（Unit 9 转存后）；尚未转存 → url=="" → 业务稍后重试 GET。
		// 签名 URL 绝不入日志（仅 err 记录，不记 url）。accountID 强制归属（结构性防越权）。
		url, perr := h.svc.PresignResult(c.Request.Context(), key.BusinessAccountID, view.ID, h.resultURLTTL)
		if perr != nil {
			h.logger.Error("video get: 结果签名 URL 生成失败（返回状态不含 URL）",
				slog.String("task_id", view.ID), slog.String("err", perr.Error()))
		} else if url != "" {
			resp.Result = &videoResult{
				VideoURL:         url,
				ExpiresInSeconds: int64(h.resultURLTTL / time.Second),
			}
		}
	}
	c.JSON(http.StatusOK, resp)
}

// =============================================================================
// GET /v1/account/balance
// =============================================================================

// GetBalance 查账户余额/用量（available=可用余额、reserved=在途占用、used_total=累计用量）。
func (h *VideoHandler) GetBalance(c *gin.Context) {
	key, ok := businessKeyFromCtx(c)
	if !ok {
		h.respondInternal(c, "缺少业务鉴权上下文")
		return
	}
	b, err := h.svc.GetBalance(c.Request.Context(), key.BusinessAccountID)
	if err != nil {
		if errors.Is(err, ledger.ErrAccountNotFound) {
			// 与 Submit 流程 / relay handler 一致：账户不存在 → 401（鉴权语义），不用 404。
			relay.WriteErrorJSON(c, http.StatusUnauthorized, relay.ErrTypeInvalidAPIKey, "account_not_found",
				"业务账户不存在")
			return
		}
		h.logger.Error("video balance: 查询失败", slog.String("err", err.Error()))
		h.respondInternal(c, "查询失败")
		return
	}
	c.JSON(http.StatusOK, balanceResponse{
		AvailableMinor:     b.Available,
		ReservedMinor:      b.Reserved,
		UsedTotalMinor:     b.UsedTotal,
		RechargeTotalMinor: b.RechargeTotal,
		RefundTotalMinor:   b.RefundTotal,
		Frozen:             b.Frozen,
		Currency:           "CNY_minor",
	})
}

// =============================================================================
// helpers
// =============================================================================

// respondInternal 500 响应（不暴露细节）。
func (h *VideoHandler) respondInternal(c *gin.Context, reason string) {
	h.logger.Error("video handler 内部错误", slog.String("reason", reason))
	relay.WriteErrorJSON(c, http.StatusInternalServerError, relay.ErrTypeAPIError, "internal_error",
		"服务内部错误")
}

// apiStatus 把内部 10 态 task_status 收敛为业务可见状态（queued/running/succeeded/failed/cancelled/expired）。
//
//   - 提交/上游在途/结算中 → running（结果未就绪，业务继续轮询）
//   - SETTLED：error_code 空 = COMPLETED 来源成功 → succeeded；否则失败来源 → failed
//   - SETTLE_FAILED：error_code 空（COMPLETED 来源，仅计费对账中，业务侧视为成功，结果可取）→ succeeded
//   - 上游终态未结算（FAILED/CANCELLED/EXPIRED 短暂态）→ 对应失败状态
func apiStatus(s db.TaskStatus, errorCode string) string {
	switch s {
	case db.TaskStatusSUBMITTED, db.TaskStatusUPSTREAMSUBMITTING, db.TaskStatusUPSTREAMSUBMITTED,
		db.TaskStatusCOMPLETED, db.TaskStatusSETTLING:
		return "running"
	case db.TaskStatusSETTLED:
		// SETTLED：error_code 空 = COMPLETED 来源成功结算 → succeeded；非空 = 失败来源已 release → failed。
		if errorCode == "" {
			return "succeeded"
		}
		return "failed"
	case db.TaskStatusSETTLEFAILED:
		// 结算失败终态（缺 usage / Poll 持续失败 / commit 永久失败 / 快照损坏）：即便上游成功，
		// 网关侧结算未完成且结果转存 store job 未触发（只在 SETTLED 触发）→ 产物经网关不可取。
		// 对业务暴露为 failed（停止轮询 + 转人工对账），**绝不**谎报 succeeded（否则业务无限轮询取不到产物）。
		return "failed"
	case db.TaskStatusFAILED:
		return "failed"
	case db.TaskStatusCANCELLED:
		return "cancelled"
	case db.TaskStatusEXPIRED:
		return "expired"
	default:
		return "running"
	}
}

// businessKeyFromCtx 从 gin.Context 读已鉴权的业务 key（BusinessKeyAuth 中间件注入）。
//
// 直接引用 middleware.CtxKeyBusinessKey（httpapi → middleware 单向依赖，无循环；与 relay 包不同，
// relay 因 middleware→relay 反向依赖才硬编码字符串）。不存在/类型不符返 (nil, false)（handler 兜底 500）。
func businessKeyFromCtx(c *gin.Context) (*businesskey.Key, bool) {
	v, ok := c.Get(middleware.CtxKeyBusinessKey)
	if !ok {
		return nil, false
	}
	vr, ok := v.(*businesskey.ValidationResult)
	if !ok || vr == nil || vr.Key == nil {
		return nil, false
	}
	return vr.Key, true
}

// =============================================================================
// 响应 DTO
// =============================================================================

type videoSubmitResponse struct {
	ID       string `json:"id"`
	Object   string `json:"object"`
	Status   string `json:"status"`
	Model    string `json:"model"`
	TaskType string `json:"task_type"`
}

type videoResult struct {
	VideoURL         string `json:"video_url"`
	ExpiresInSeconds int64  `json:"expires_in_seconds"`
}

type videoTaskResponse struct {
	ID           string       `json:"id"`
	Object       string       `json:"object"`
	Status       string       `json:"status"`
	Model        string       `json:"model"`
	TaskType     string       `json:"task_type"`
	CreatedAt    int64        `json:"created_at"`
	UpdatedAt    int64        `json:"updated_at"`
	ErrorCode    string       `json:"error_code,omitempty"`
	ErrorMessage string       `json:"error_message,omitempty"`
	Result       *videoResult `json:"result,omitempty"`
}

type balanceResponse struct {
	AvailableMinor     int64  `json:"available_minor"`
	ReservedMinor      int64  `json:"reserved_minor"`
	UsedTotalMinor     int64  `json:"used_total_minor"`
	RechargeTotalMinor int64  `json:"recharge_total_minor"`
	RefundTotalMinor   int64  `json:"refund_total_minor"`
	Frozen             bool   `json:"frozen"`
	Currency           string `json:"currency"`
}

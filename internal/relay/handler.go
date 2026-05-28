package relay

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/sunxin-git/api-gateway/internal/businesskey"
	"github.com/sunxin-git/api-gateway/internal/ledger"
)

// RelayHandler 业务 chat completions endpoint 的 handler（plan §Unit 5）。
//
// 整合 catalog + adapter + ledger 三层完成 Reserve → Relay → Settle 端到端闭环。
//
// 不在 handler 处理（已在上游链 / 后续 unit 处理）：
//   - body size 限制（BusinessBodyLimit 已限 1 MiB）
//   - 鉴权（BusinessKeyAuth 已注入 ValidationResult 到 ctx）
//   - RPM 限速（BusinessRPM 已检查）
//   - audit emit（BusinessAudit defer 模式）
//   - metric 顶层（Prom middleware）/ HSTS / TLS（部署侧）
type RelayHandler struct {
	catalog  Catalog
	adapter  ProviderAdapter
	ledger   ledger.Service
	metrics  *HandlerMetrics
	logger   *slog.Logger
	settleTO time.Duration // Settle 独立 ctx 超时（默认 5s）
}

// NewRelayHandler 构造 handler；Constructor fail-fast 校验。
//
// 入参：
//   - catalog：MVP 用 EnvCatalog；P1+ 用 YAML / DB
//   - adapter：OpenAI 兼容（MVP 唯一）
//   - ledger：复用 D-min admin handler 同 LedgerService
//   - metrics：可空（测试不必注入完整 metric set；handler 内 nil-safe）
//   - log：不能为 nil
func NewRelayHandler(catalog Catalog, adapter ProviderAdapter, l ledger.Service, m *HandlerMetrics, log *slog.Logger) *RelayHandler {
	if catalog == nil {
		panic("relay.NewRelayHandler: catalog 不能为 nil")
	}
	if adapter == nil {
		panic("relay.NewRelayHandler: adapter 不能为 nil")
	}
	if l == nil {
		panic("relay.NewRelayHandler: ledger.Service 不能为 nil")
	}
	if log == nil {
		panic("relay.NewRelayHandler: log 不能为 nil")
	}
	return &RelayHandler{
		catalog:  catalog,
		adapter:  adapter,
		ledger:   l,
		metrics:  m,
		logger:   log,
		settleTO: 5 * time.Second,
	}
}

// settleRetryBackoff Commit / Release 重试退避序列（plan §决策 D2）。
//
// 设计选择：3 次指数退避；CAS 冲突在 Settle 阶段实际罕见（reserve 已锁该账户）；
// 3 次足够覆盖瞬时冲突。永久失败不向业务返错（业务已收到上游响应），由运维 SOP 兜底。
var settleRetryBackoff = []time.Duration{
	100 * time.Millisecond,
	300 * time.Millisecond,
	1 * time.Second,
}

// referenceTypeChat ledger ReserveParams.ReferenceType 值；运维查 SUM ledger entries by reference_type 用。
const referenceTypeChat = "chat_completion"

// ChatCompletion 业务 POST /v1/chat/completions 入口（plan §Unit 5 完整流程）。
//
// 流程：
//
//  1. bind body 为 map[string]any（保留所有 OpenAI 协议字段透传）
//  2. 入参校验：model 必填 / stream=false / messages 非空 / max_tokens ≤ entry.MaxContextTokens
//  3. 查字典（MVP EnvCatalog 唯一条；业务传任何 model 都路由）
//  4. 估 input tokens（保守上界 = len(json) / 4）
//  5. 计算 reserve = ceil((input × in_price + max_tokens_or_default × out_price) / 1_000_000)
//  6. Reserve（actor=business_key:{id}, correlation=request_id）
//  7. Relay 调上游（业务 ctx 透传 → 业务断开时上游连接断 → Release）
//  8. Settle：用**独立 background ctx**（防业务 ctx 取消导致 orphan reserve）
//     - 上游 200 + usage → Commit(actual_cost)
//     - 200 缺 usage → Commit(reserve) 兜底 + critical metric
//     - 4xx/5xx/timeout/unreachable → Release(reserve) + OpenAI 兼容错误
//  9. 透传上游响应 body / status；audit middleware defer 时读 ctx 元数据 emit
func (h *RelayHandler) ChatCompletion(c *gin.Context) {
	// === 1-2. 解析 + 校验入参 ===
	var rawBody map[string]any
	if err := c.ShouldBindJSON(&rawBody); err != nil {
		SetBusinessAuditOutcomeCode(c, "invalid_request_body")
		WriteErrorJSON(c, http.StatusBadRequest, ErrTypeInvalidRequest, "invalid_request_body",
			"请求体不合法："+err.Error())
		return
	}

	modelName, _ := rawBody["model"].(string)
	if modelName == "" {
		SetBusinessAuditOutcomeCode(c, "invalid_request_body")
		WriteErrorJSON(c, http.StatusBadRequest, ErrTypeInvalidRequest, "missing_model",
			"必填字段 model 缺失")
		return
	}
	if stream, _ := rawBody["stream"].(bool); stream {
		SetBusinessAuditOutcomeCode(c, "streaming_not_supported")
		WriteErrorJSON(c, http.StatusBadRequest, ErrTypeInvalidRequest, "streaming_not_supported",
			"流式响应未实装；请将 stream 设为 false（MVP 限制）")
		return
	}
	messages, _ := rawBody["messages"].([]any)
	if len(messages) == 0 {
		SetBusinessAuditOutcomeCode(c, "invalid_request_body")
		WriteErrorJSON(c, http.StatusBadRequest, ErrTypeInvalidRequest, "empty_messages",
			"messages 不能为空")
		return
	}

	// === 3. 查字典 ===
	entry, ok := h.catalog.Lookup(modelName)
	if !ok || entry == nil {
		// MVP EnvCatalog 永远命中；实际不会走到此分支。P1+ 多条字典时 fallback。
		SetBusinessAuditOutcomeCode(c, "model_not_found")
		WriteErrorJSON(c, http.StatusBadRequest, ErrTypeInvalidRequest, "model_not_found",
			"未知 model")
		return
	}
	SetBusinessAuditModelInfo(c, entry.GatewayModelName, entry.UpstreamModelName)

	// === 4-5. 估算 + 计算 reserve ===
	maxTokens := readMaxTokens(rawBody, entry.MaxContextTokens)
	if maxTokens > int64(entry.MaxContextTokens) {
		SetBusinessAuditOutcomeCode(c, "max_tokens_exceeds_context")
		WriteErrorJSON(c, http.StatusBadRequest, ErrTypeInvalidRequest, "max_tokens_exceeds_context",
			"max_tokens 超过模型 context 上限")
		return
	}
	inputEst := estimateInputTokens(messages)
	reserveAmount := computeReserveMinor(int64(inputEst), maxTokens, entry.PriceInputPer1MMinor, entry.PriceOutputPer1MMinor)

	// === 6. Reserve ===
	key := getBusinessKeyFromCtx(c)
	if key == nil {
		// 防御性：BusinessKeyAuth 应已注入
		h.respondInternal(c, "missing key context")
		return
	}
	actor := ledger.Actor{
		Type: ledger.ActorTypeBusinessKey,
		ID:   strconv.FormatInt(key.ID, 10),
	}
	correlationID := getRequestIDFromCtx(c)

	if _, err := h.ledger.Reserve(c.Request.Context(), actor, ledger.ReserveParams{
		AccountID:     key.BusinessAccountID,
		Amount:        reserveAmount,
		CorrelationID: correlationID,
		ReferenceType: referenceTypeChat,
		ReferenceID:   correlationID,
	}); err != nil {
		h.handleReserveError(c, err)
		return
	}

	// === 7. Relay 上游 ===
	upstreamResp, relayErr := h.adapter.ChatCompletion(c.Request.Context(), entry, rawBody)
	if upstreamResp != nil {
		h.metrics.safeUpstreamDuration(entry.GatewayModelName, statusLabel(upstreamResp.StatusCode), upstreamResp.Duration.Seconds())
		SetBusinessAuditUpstreamResult(c, upstreamResp.StatusCode, upstreamResp.Duration)
	}

	if relayErr != nil {
		h.handleRelayError(c, relayErr, actor, key.BusinessAccountID, correlationID, reserveAmount, entry)
		return
	}

	// === 8. 分流：200 / 4xx / 5xx ===
	switch {
	case upstreamResp.StatusCode == http.StatusOK:
		h.handleUpstream200(c, upstreamResp, actor, key.BusinessAccountID, correlationID, reserveAmount, entry)
	case upstreamResp.StatusCode >= 500:
		// 上游 5xx → release + 502（plan §决策 D9：5xx 透传会让业务误解，改 502 明示"上游问题"）
		h.releaseAndLogIfFailed(actor, key.BusinessAccountID, correlationID, reserveAmount, "upstream_5xx", "upstream returned 5xx")
		SetBusinessAuditOutcomeCode(c, "upstream_5xx")
		h.metrics.safeRequestTotal(entry.GatewayModelName, "5xx")
		WriteErrorJSON(c, http.StatusBadGateway, ErrTypeUpstreamError, "upstream_5xx",
			"上游服务暂时不可用，请稍后重试")
	default:
		// 上游 4xx → release + 透传 status + body（业务方按 OpenAI 协议解析报错）
		h.releaseAndLogIfFailed(actor, key.BusinessAccountID, correlationID, reserveAmount, "upstream_4xx", "upstream returned 4xx")
		SetBusinessAuditOutcomeCode(c, "upstream_4xx")
		h.metrics.safeRequestTotal(entry.GatewayModelName, "4xx")
		h.passthroughResponse(c, upstreamResp)
	}
}

// handleUpstream200 上游 200 处理：解析 usage 计算 actual_cost → Commit；
// usage 缺失时兜底 Commit(reserve_amount)（plan §决策 D2）。
func (h *RelayHandler) handleUpstream200(
	c *gin.Context,
	resp *UpstreamResponse,
	actor ledger.Actor,
	accountID, correlationID string,
	reserveAmount int64,
	entry *ModelEntry,
) {
	var actualCost int64
	if resp.Usage == nil {
		// 上游 200 但缺 usage —— 兜底 commit reserve 全额（防 orphan reserve）
		h.logger.Error("upstream returned 200 but usage missing; falling back to commit reserve amount",
			slog.String("model", entry.GatewayModelName),
			slog.String("correlation_id", correlationID),
			slog.Int64("reserve_amount", reserveAmount),
		)
		h.metrics.safeUpstreamMissingUsage(entry.GatewayModelName)
		actualCost = reserveAmount
	} else {
		SetBusinessAuditTokens(c, resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
		actualCost = computeCostMinor(
			int64(resp.Usage.PromptTokens), int64(resp.Usage.CompletionTokens),
			entry.PriceInputPer1MMinor, entry.PriceOutputPer1MMinor,
		)
		// 兜底：actual ≤ reserve（ledger.Commit 入口校验；越界视作 provider bug，cap 到 reserve）
		if actualCost > reserveAmount {
			h.logger.Warn("usage-based actual_cost exceeds reserve; capping to reserve（疑似 provider 计费异常）",
				slog.String("correlation_id", correlationID),
				slog.Int64("actual_cost", actualCost),
				slog.Int64("reserve", reserveAmount),
			)
			actualCost = reserveAmount
		}
	}

	if err := h.commitWithRetry(actor, accountID, correlationID, actualCost); err != nil {
		// permanent commit failure: orphan reserve；critical log + metric；不向业务返错
		h.logger.Error("relay settle commit failed permanently；orphan reserve（运维 SOP 兜底）",
			slog.String("correlation_id", correlationID),
			slog.String("account_id", accountID),
			slog.Int64("reserve_amount", reserveAmount),
			slog.Int64("actual_cost", actualCost),
			slog.String("err", err.Error()),
		)
		h.metrics.safeSettleFailed("commit", classifySettleErr(err))
		// 仍透传上游响应给业务（业务已"收到"结果）
	}

	SetBusinessAuditCost(c, actualCost)
	SetBusinessAuditOutcomeCode(c, "ok")
	h.metrics.safeTokenCost(entry.GatewayModelName, actualCost)
	h.metrics.safeRequestTotal(entry.GatewayModelName, "200")
	h.passthroughResponse(c, resp)
}

// handleReserveError 把 ledger.Reserve 错误映射为 OpenAI 兼容响应。
func (h *RelayHandler) handleReserveError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, ledger.ErrAccountNotFound):
		// FK CASCADE 应保证不发生；防御性处理
		h.metrics.safeReserveFailed("account_not_found")
		SetBusinessAuditOutcomeCode(c, "account_not_found")
		WriteErrorJSON(c, http.StatusUnauthorized, ErrTypeInvalidAPIKey, "account_not_found",
			"业务账户不存在")
	case errors.Is(err, ledger.ErrAccountFrozen):
		h.metrics.safeReserveFailed("account_frozen")
		SetBusinessAuditOutcomeCode(c, "account_frozen")
		WriteErrorJSON(c, http.StatusPaymentRequired, ErrTypeInsufficientQuota, "account_frozen",
			"账户已冻结，请联系运营")
	case errors.Is(err, ledger.ErrInsufficientBalance):
		h.metrics.safeReserveFailed("insufficient_balance")
		SetBusinessAuditOutcomeCode(c, "insufficient_quota")
		WriteErrorJSON(c, http.StatusPaymentRequired, ErrTypeInsufficientQuota, "insufficient_quota",
			"账户余额不足，请联系运营充值")
	case errors.Is(err, ledger.ErrVersionConflict):
		h.metrics.safeReserveFailed("version_conflict")
		SetBusinessAuditOutcomeCode(c, "version_conflict")
		WriteErrorJSON(c, http.StatusServiceUnavailable, ErrTypeServerError, "temporarily_unavailable",
			"服务繁忙，请重试")
	default:
		h.metrics.safeReserveFailed("internal")
		_ = c.Error(err)
		h.respondInternal(c, "reserve failed")
	}
}

// handleRelayError 上游调用层 error（timeout / unreachable / malformed）→ Release + 错误响应。
func (h *RelayHandler) handleRelayError(
	c *gin.Context,
	relayErr error,
	actor ledger.Actor,
	accountID, correlationID string,
	reserveAmount int64,
	entry *ModelEntry,
) {
	var status int
	var errType, code, msg, outcome string
	switch {
	case errors.Is(relayErr, ErrUpstreamTimeout):
		status = http.StatusGatewayTimeout
		errType = ErrTypeUpstreamTimeout
		code = "upstream_timeout"
		msg = "上游服务响应超时（60s），请稍后重试"
		outcome = "upstream_timeout"
	case errors.Is(relayErr, ErrUpstreamUnreachable):
		status = http.StatusBadGateway
		errType = ErrTypeUpstreamError
		code = "upstream_unreachable"
		msg = "无法连接上游服务"
		outcome = "upstream_unreachable"
	case errors.Is(relayErr, ErrUpstreamMalformed):
		status = http.StatusBadGateway
		errType = ErrTypeUpstreamError
		code = "upstream_malformed"
		msg = "上游响应格式异常"
		outcome = "upstream_malformed"
	default:
		status = http.StatusBadGateway
		errType = ErrTypeUpstreamError
		code = "upstream_unknown"
		msg = "上游调用失败"
		outcome = "upstream_unknown"
		_ = c.Error(relayErr)
	}
	h.releaseAndLogIfFailed(actor, accountID, correlationID, reserveAmount, outcome, relayErr.Error())
	SetBusinessAuditOutcomeCode(c, outcome)
	h.metrics.safeRequestTotal(entry.GatewayModelName, outcome)
	WriteErrorJSON(c, status, errType, code, msg)
}

// releaseAndLogIfFailed Release reserve；permanent failure 仅 log + bump metric，不返业务错。
func (h *RelayHandler) releaseAndLogIfFailed(
	actor ledger.Actor,
	accountID, correlationID string,
	reserveAmount int64,
	context, reason string,
) {
	if err := h.releaseWithRetry(actor, accountID, correlationID, reserveAmount); err != nil {
		h.logger.Error("relay settle release failed permanently；orphan reserve（运维 SOP 兜底）",
			slog.String("correlation_id", correlationID),
			slog.String("account_id", accountID),
			slog.Int64("reserve_amount", reserveAmount),
			slog.String("context", context),
			slog.String("reason", reason),
			slog.String("err", err.Error()),
		)
		h.metrics.safeSettleFailed("release", classifySettleErr(err))
	}
}

// commitWithRetry 调 ledger.Commit；CAS 冲突重试 3 次。
//
// Settle 用**独立 background ctx**（业务 ctx 取消时 settle 仍完成，避免 orphan reserve）。
// 5s 总超时；超时被 metric 反映为 settle_failed。
func (h *RelayHandler) commitWithRetry(
	actor ledger.Actor,
	accountID, correlationID string,
	actualCost int64,
) error {
	settleCtx, cancel := context.WithTimeout(context.Background(), h.settleTO)
	defer cancel()
	return retryOnCASConflict(settleCtx, settleRetryBackoff, func(ctx context.Context) error {
		_, err := h.ledger.Commit(ctx, actor, ledger.CommitParams{
			AccountID:     accountID,
			CorrelationID: correlationID,
			ActualCost:    actualCost,
			ReferenceType: referenceTypeChat,
			ReferenceID:   correlationID,
		})
		return err
	})
}

// releaseWithRetry 调 ledger.Release；同 commitWithRetry 用独立 ctx + 3 次重试。
func (h *RelayHandler) releaseWithRetry(
	actor ledger.Actor,
	accountID, correlationID string,
	reserveAmount int64,
) error {
	settleCtx, cancel := context.WithTimeout(context.Background(), h.settleTO)
	defer cancel()
	return retryOnCASConflict(settleCtx, settleRetryBackoff, func(ctx context.Context) error {
		_, err := h.ledger.Release(ctx, actor, ledger.ReleaseParams{
			AccountID:     accountID,
			CorrelationID: correlationID,
			Amount:        reserveAmount,
			ReferenceType: referenceTypeChat,
			ReferenceID:   correlationID,
		})
		return err
	})
}

// retryOnCASConflict CAS 冲突时重试 + 指数退避；非 CAS 错误立即返。
//
// 设计：
//   - ledger.ErrVersionConflict 即 CAS；其他 error（DB 断 / 账户冻结 / 入参非法）立即返
//   - 总尝试次数 = 1 + len(backoffs)：首次立即调；后续按 backoffs 序列等待后重试
//   - ctx done 时立即返 ctx.Err()
func retryOnCASConflict(ctx context.Context, backoffs []time.Duration, fn func(ctx context.Context) error) error {
	// 首次立即调
	err := fn(ctx)
	if err == nil {
		return nil
	}
	if !errors.Is(err, ledger.ErrVersionConflict) {
		return err
	}
	lastErr := err

	// 后续按 backoffs 序列等待后重试
	for _, backoff := range backoffs {
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return ctx.Err()
		}
		err := fn(ctx)
		if err == nil {
			return nil
		}
		if !errors.Is(err, ledger.ErrVersionConflict) {
			return err
		}
		lastErr = err
	}
	return lastErr
}

// passthroughResponse 把 UpstreamResponse 透传给业务（含 status + body）。
//
// header **不**透传（plan §决策 D3：上游 header 含敏感信息如 x-request-id 等）；
// Content-Type 强制 JSON（OpenAI 协议标准）。
func (h *RelayHandler) passthroughResponse(c *gin.Context, resp *UpstreamResponse) {
	if c.Writer.Written() {
		return // 防御性：已写则跳过
	}
	c.Header("Content-Type", "application/json")
	c.Status(resp.StatusCode)
	_, _ = c.Writer.Write(resp.Body)
}

// respondInternal 500 内部错误响应（不暴露细节）。
func (h *RelayHandler) respondInternal(c *gin.Context, reason string) {
	SetBusinessAuditOutcomeCode(c, "internal_error")
	h.logger.Error("relay internal error", slog.String("reason", reason))
	WriteErrorJSON(c, http.StatusInternalServerError, ErrTypeAPIError, "internal_error",
		"服务内部错误")
}

// =============================================================================
// helpers
// =============================================================================

// readMaxTokens 从业务 body 读 max_tokens；缺失时返 entry.MaxContextTokens 作上界。
//
// OpenAI 协议中 max_tokens 是 int；业务传 float（JSON 数字 unmarshal 默认 float64）
// 也接受，转 int64。
func readMaxTokens(body map[string]any, defaultMax int32) int64 {
	v, ok := body["max_tokens"]
	if !ok || v == nil {
		return int64(defaultMax)
	}
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int:
		return int64(n)
	case int64:
		return n
	case json.Number:
		i, _ := n.Int64()
		return i
	default:
		// 不识别类型时 fallback 默认（plan §决策 D1 保守上界优先 over reject）
		return int64(defaultMax)
	}
}

// computeReserveMinor reserve 上界公式（plan §决策 D1）。
//
// reserve = ceil((input × in_price + max_tokens × out_price) / 1_000_000)
//
// 边界：所有正数 ledger 视作合法；返 0 时 ledger.Reserve 会拒（ErrInvalidAmount）。
func computeReserveMinor(inputTokens, maxTokens, priceInputPer1M, priceOutputPer1M int64) int64 {
	// 防溢出：单笔输入 / 输出 token × price 不应超 int64 上限的一半
	if inputTokens < 0 {
		inputTokens = 0
	}
	if maxTokens < 0 {
		maxTokens = 0
	}
	totalNumerator := inputTokens*priceInputPer1M + maxTokens*priceOutputPer1M
	// ceil division
	return int64(math.Ceil(float64(totalNumerator) / 1_000_000.0))
}

// computeCostMinor settle 阶段按真实 usage 算 actual_cost。
func computeCostMinor(promptTokens, completionTokens, priceInputPer1M, priceOutputPer1M int64) int64 {
	totalNumerator := promptTokens*priceInputPer1M + completionTokens*priceOutputPer1M
	return int64(math.Ceil(float64(totalNumerator) / 1_000_000.0))
}

// classifySettleErr 把 settle 失败 error 分类为 metric reason 字符串。
func classifySettleErr(err error) string {
	if errors.Is(err, ledger.ErrVersionConflict) {
		return "version_conflict"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "internal"
}

// statusLabel HTTP status code → metric label 字符串。
//
// 收敛到 "200" / "4xx" / "5xx" 避免高基数 label。
func statusLabel(status int) string {
	switch {
	case status == 200:
		return "200"
	case status >= 400 && status < 500:
		return "4xx"
	case status >= 500:
		return "5xx"
	default:
		return strconv.Itoa(status)
	}
}

// getBusinessKeyFromCtx 从 gin.Context 读 BusinessKey ValidationResult。
//
// 与 middleware.CtxKeyBusinessKey 同 key（"business_key_validation"）；硬编码常量
// 避免反向 import middleware（middleware → relay 单向）。
// 不存在或类型不匹配时返 nil（fail-closed，handler 兜底 500）。
func getBusinessKeyFromCtx(c *gin.Context) *businesskey.Key {
	v, ok := c.Get("business_key_validation")
	if !ok {
		return nil
	}
	vr, ok := v.(*businesskey.ValidationResult)
	if !ok || vr == nil {
		return nil
	}
	return vr.Key
}

// 编译期断言 io.Discard 保留 import 给未来扩展。
var _ = io.Discard

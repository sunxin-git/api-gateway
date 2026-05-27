package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sunxin-git/api-gateway/internal/audit"
	"github.com/sunxin-git/api-gateway/internal/businesskey"
)

// =============================================================================
// Fakes（避开真 PG / RPM 内部）
// =============================================================================

type fakeBusinessService struct {
	keys      map[string]*businesskey.Key // plaintext → key
	validateF func(plaintext string) (*businesskey.ValidationResult, error)
}

func (f *fakeBusinessService) Create(_ context.Context, _ businesskey.CreateParams) (*businesskey.Key, string, error) {
	return nil, "", errors.New("not implemented in fake")
}
func (f *fakeBusinessService) ValidateByPlaintext(_ context.Context, plaintext string) (*businesskey.ValidationResult, error) {
	if f.validateF != nil {
		return f.validateF(plaintext)
	}
	k, ok := f.keys[plaintext]
	if !ok {
		return nil, businesskey.ErrKeyNotFound
	}
	return &businesskey.ValidationResult{Key: k}, nil
}
func (f *fakeBusinessService) Revoke(_ context.Context, _ int64) (bool, error) {
	return false, errors.New("not implemented in fake")
}
func (f *fakeBusinessService) ListByAccount(_ context.Context, _ string) ([]*businesskey.Key, error) {
	return nil, errors.New("not implemented in fake")
}
func (f *fakeBusinessService) ListAll(_ context.Context) ([]*businesskey.Key, error) {
	return nil, errors.New("not implemented in fake")
}
func (f *fakeBusinessService) GetByID(_ context.Context, _ int64) (*businesskey.Key, error) {
	return nil, errors.New("not implemented in fake")
}
func (f *fakeBusinessService) TouchLastUsed(_ context.Context, _ int64) error { return nil }
func (f *fakeBusinessService) Close() error                                   { return nil }

// =============================================================================
// 公共 helpers
// =============================================================================

func newBusinessQuotaCounter() *prometheus.CounterVec {
	return prometheus.NewCounterVec(prometheus.CounterOpts{Name: "test_biz_rate_limited"}, []string{"key_id"})
}

func newBusinessAuthFailedCounter() *prometheus.CounterVec {
	return prometheus.NewCounterVec(prometheus.CounterOpts{Name: "test_biz_auth_failed"}, []string{"reason"})
}

func newBusinessBodyTooLargeCounter() prometheus.Counter {
	return prometheus.NewCounter(prometheus.CounterOpts{Name: "test_biz_body_too_large"})
}

// injectBusinessKey 测试用：直接把 ValidationResult 注入 ctx，跳过 BusinessKeyAuth。
func injectBusinessKey(k *businesskey.Key) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set(CtxKeyBusinessKey, &businesskey.ValidationResult{Key: k})
		c.Next()
	}
}

// newTestRPM 构造测试用 InProcessRPM（不启 GC goroutine，避免 race）。
// 直接调 businesskey.NewInProcessRPM —— 其内部已用 race-safe pattern。
func newBusinessTestRPM(t *testing.T) *businesskey.InProcessRPM {
	t.Helper()
	rpm := businesskey.NewInProcessRPM(silentSlog(), nil)
	t.Cleanup(func() { _ = rpm.Close() })
	return rpm
}

// =============================================================================
// BusinessBodyLimit
// =============================================================================

func TestBusinessBodyLimit_Under1MB_Passes(t *testing.T) {
	r := newAdminEngine()
	r.Use(RequestID(), BusinessBodyLimit(newBusinessBodyTooLargeCounter()))
	r.POST("/x", func(c *gin.Context) {
		_, _ = c.Writer.Write([]byte("ok"))
	})

	body := bytes.Repeat([]byte("a"), 500*1024) // 500 KB
	req := httptest.NewRequest(http.MethodPost, "/x", bytes.NewReader(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestBusinessBodyLimit_Over1MB_Rejects413(t *testing.T) {
	counter := newBusinessBodyTooLargeCounter()
	r := newAdminEngine()
	r.Use(RequestID(), BusinessBodyLimit(counter))
	r.POST("/x", func(c *gin.Context) {
		_, err := readAllBody(c)
		if err != nil {
			_ = c.Error(err)
			return
		}
		_, _ = c.Writer.Write([]byte("ok"))
	})

	body := bytes.Repeat([]byte("a"), 2*1024*1024) // 2 MiB
	req := httptest.NewRequest(http.MethodPost, "/x", bytes.NewReader(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
	assert.Equal(t, float64(1), testutil.ToFloat64(counter))

	// OpenAI 兼容错误格式断言
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	errMap := resp["error"].(map[string]any)
	assert.Equal(t, "payload_too_large", errMap["code"])
	assert.Equal(t, "invalid_request_error", errMap["type"])
}

// =============================================================================
// BusinessKeyAuth
// =============================================================================

func TestBusinessKeyAuth_MissingHeader_401(t *testing.T) {
	svc := &fakeBusinessService{keys: map[string]*businesskey.Key{}}
	authCnt := newBusinessAuthFailedCounter()

	r := newAdminEngine()
	r.Use(RequestID(), BusinessKeyAuth(svc, authCnt))
	r.POST("/v1/chat/completions", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Equal(t, float64(1), testutil.ToFloat64(authCnt.WithLabelValues("missing_header")))

	// OpenAI 兼容错误
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	errMap := resp["error"].(map[string]any)
	assert.Equal(t, "invalid_api_key", errMap["type"])
}

func TestBusinessKeyAuth_BadScheme_401(t *testing.T) {
	svc := &fakeBusinessService{keys: map[string]*businesskey.Key{}}
	r := newAdminEngine()
	r.Use(RequestID(), BusinessKeyAuth(svc, newBusinessAuthFailedCounter()))
	r.POST("/x", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	req.Header.Set("Authorization", "Basic abc")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestBusinessKeyAuth_UnknownKey_401(t *testing.T) {
	svc := &fakeBusinessService{keys: map[string]*businesskey.Key{}}
	authCnt := newBusinessAuthFailedCounter()
	r := newAdminEngine()
	r.Use(RequestID(), BusinessKeyAuth(svc, authCnt))
	r.POST("/x", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	req.Header.Set("Authorization", "Bearer biz-sk-unknown")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Equal(t, float64(1), testutil.ToFloat64(authCnt.WithLabelValues("invalid_api_key")))
}

func TestBusinessKeyAuth_Happy_InjectsKeyToCtx(t *testing.T) {
	k := &businesskey.Key{
		ID:                42,
		BusinessAccountID: "tenant-001",
		Description:       "happy-key",
	}
	svc := &fakeBusinessService{keys: map[string]*businesskey.Key{"biz-sk-ok": k}}

	var seenKeyID int64
	r := newAdminEngine()
	r.Use(RequestID(), BusinessKeyAuth(svc, newBusinessAuthFailedCounter()))
	r.POST("/x", func(c *gin.Context) {
		vr := GetBusinessKeyValidation(c)
		require.NotNil(t, vr)
		seenKeyID = vr.Key.ID
		c.String(http.StatusOK, "ok")
	})

	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	req.Header.Set("Authorization", "Bearer biz-sk-ok")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, int64(42), seenKeyID)
}

func TestBusinessKeyAuth_InternalError_500(t *testing.T) {
	internalErr := errors.New("db unreachable")
	svc := &fakeBusinessService{
		validateF: func(_ string) (*businesskey.ValidationResult, error) {
			return nil, internalErr
		},
	}
	authCnt := newBusinessAuthFailedCounter()
	r := newAdminEngine()
	r.Use(RequestID(), BusinessKeyAuth(svc, authCnt))
	r.POST("/x", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	req.Header.Set("Authorization", "Bearer biz-sk-x")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Equal(t, float64(1), testutil.ToFloat64(authCnt.WithLabelValues("internal_error")))
}

// =============================================================================
// BusinessRPM
// =============================================================================

func TestBusinessRPM_NoKeyInCtx_500(t *testing.T) {
	rpm := newBusinessTestRPM(t)
	r := newAdminEngine()
	r.Use(RequestID(), BusinessRPM(rpm, newBusinessQuotaCounter()))
	r.POST("/x", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestBusinessRPM_LimitExceeded_429(t *testing.T) {
	rpm := newBusinessTestRPM(t)
	cnt := newBusinessQuotaCounter()
	limit := int32(2)
	k := &businesskey.Key{ID: 7, RequestsPerMinute: &limit}

	r := newAdminEngine()
	r.Use(RequestID(), injectBusinessKey(k), BusinessRPM(rpm, cnt))
	r.POST("/x", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	// 前 2 个通过
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/x", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code)
	}
	// 第 3 个被拒
	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusTooManyRequests, w.Code)
	assert.Equal(t, float64(1), testutil.ToFloat64(cnt.WithLabelValues("7")))

	// OpenAI 兼容错误
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	errMap := resp["error"].(map[string]any)
	assert.Equal(t, "rate_limit_exceeded", errMap["type"])
}

func TestBusinessRPM_NilLimit_AlwaysPasses(t *testing.T) {
	rpm := newBusinessTestRPM(t)
	k := &businesskey.Key{ID: 8, RequestsPerMinute: nil}
	r := newAdminEngine()
	r.Use(RequestID(), injectBusinessKey(k), BusinessRPM(rpm, newBusinessQuotaCounter()))
	r.POST("/x", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	for i := 0; i < 100; i++ {
		req := httptest.NewRequest(http.MethodPost, "/x", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code)
	}
}

// =============================================================================
// BusinessAudit
// =============================================================================

func TestBusinessAudit_HappyPath_EmitsTier2(t *testing.T) {
	k := &businesskey.Key{ID: 11, BusinessAccountID: "tenant-001", Description: "test-key"}
	logger := &fakeAuditLogger{}

	r := newAdminEngine()
	r.Use(RequestID(), injectBusinessKey(k), BusinessAudit(logger, newAuditWriteFailedCounter()))
	r.POST("/v1/chat/completions", func(c *gin.Context) {
		SetBusinessAuditTokens(c, 100, 200)
		SetBusinessAuditCost(c, 480)
		SetBusinessAuditModelInfo(c, "gw-default", "doubao-1-5-pro")
		SetBusinessAuditUpstreamResult(c, 200, 1234*1e6) // 1234ms
		c.String(http.StatusOK, `{"choices":[],"usage":{}}`)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	recs := logger.snapshot()
	require.Len(t, recs, 1)
	rec := recs[0]
	assert.Equal(t, "business_relay", rec.Event)
	assert.Equal(t, audit.Tier2, rec.Tier)
	assert.Equal(t, "ok", rec.OutcomeCode)
	assert.Equal(t, "tenant-001", rec.BusinessAccountID)
	assert.Equal(t, int64(11), rec.APIKeyID)
	assert.Equal(t, "business_key:11", rec.Actor)
	assert.Equal(t, "gw-default", rec.GatewayModel)
	assert.Equal(t, "doubao-1-5-pro", rec.UpstreamModel)
	assert.Equal(t, 100, rec.InputTokens)
	assert.Equal(t, 200, rec.OutputTokens)
	assert.Equal(t, int64(480), rec.CostMinor)
	assert.Equal(t, 200, rec.UpstreamStatus)
	assert.Equal(t, int64(1234), rec.UpstreamDurationMs)
	assert.Equal(t, 200, rec.Status)
	// 关键安全契约：record 不含 messages body
	jsonBytes, _ := json.Marshal(rec)
	assert.NotContains(t, string(jsonBytes), "choices")
}

func TestBusinessAudit_401_EmitsTier1(t *testing.T) {
	logger := &fakeAuditLogger{}
	r := newAdminEngine()
	r.Use(RequestID(), BusinessAudit(logger, newAuditWriteFailedCounter()))
	r.GET("/x", func(c *gin.Context) {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": gin.H{"code": "invalid_api_key"}})
	})

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	recs := logger.snapshot()
	require.Len(t, recs, 1)
	assert.Equal(t, audit.Tier1, recs[0].Tier, "401 应 Tier1（攻击信号）")
	assert.Equal(t, "anonymous", recs[0].Actor)
}

func TestBusinessAudit_402_EmitsTier1(t *testing.T) {
	k := &businesskey.Key{ID: 12, BusinessAccountID: "tenant-002"}
	logger := &fakeAuditLogger{}
	r := newAdminEngine()
	r.Use(RequestID(), injectBusinessKey(k), BusinessAudit(logger, newAuditWriteFailedCounter()))
	r.POST("/x", func(c *gin.Context) {
		c.AbortWithStatusJSON(http.StatusPaymentRequired, gin.H{})
	})

	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	recs := logger.snapshot()
	require.Len(t, recs, 1)
	assert.Equal(t, audit.Tier1, recs[0].Tier, "402 应 Tier1（资金信号）")
}

func TestBusinessAudit_400_StaysTier2(t *testing.T) {
	logger := &fakeAuditLogger{}
	r := newAdminEngine()
	r.Use(RequestID(), BusinessAudit(logger, newAuditWriteFailedCounter()))
	r.POST("/x", func(c *gin.Context) {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{})
	})

	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	recs := logger.snapshot()
	require.Len(t, recs, 1)
	assert.Equal(t, audit.Tier2, recs[0].Tier, "400 应 Tier2（高频低安全意义，避免拖慢 fsync）")
}

func TestBusinessAudit_429_StaysTier2(t *testing.T) {
	logger := &fakeAuditLogger{}
	r := newAdminEngine()
	r.Use(RequestID(), BusinessAudit(logger, newAuditWriteFailedCounter()))
	r.POST("/x", func(c *gin.Context) {
		c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{})
	})

	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	recs := logger.snapshot()
	require.Len(t, recs, 1)
	assert.Equal(t, audit.Tier2, recs[0].Tier, "429 应 Tier2（同 admin 模式）")
}

func TestBusinessAudit_5xx_EmitsTier1(t *testing.T) {
	logger := &fakeAuditLogger{}
	r := newAdminEngine()
	r.Use(RequestID(), BusinessAudit(logger, newAuditWriteFailedCounter()))
	r.POST("/x", func(c *gin.Context) {
		c.AbortWithStatusJSON(http.StatusBadGateway, gin.H{})
	})

	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	recs := logger.snapshot()
	require.Len(t, recs, 1)
	assert.Equal(t, audit.Tier1, recs[0].Tier, "5xx 应 Tier1（故障信号）")
}

func TestBusinessAudit_HandlerPanic_AuditEmittedAndStatus500(t *testing.T) {
	k := &businesskey.Key{ID: 13}
	logger := &fakeAuditLogger{}
	r := newAdminEngine()
	r.Use(
		RequestID(),
		Recover(nil, nil),
		injectBusinessKey(k),
		BusinessAudit(logger, newAuditWriteFailedCounter()),
	)
	r.POST("/boom", func(_ *gin.Context) {
		panic("故意 panic")
	})

	req := httptest.NewRequest(http.MethodPost, "/boom", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	recs := logger.snapshot()
	require.Len(t, recs, 1, "panic 路径 audit 必须 emit")
	assert.Equal(t, 500, recs[0].Status)
	assert.Equal(t, audit.Tier1, recs[0].Tier)
}

func TestBusinessAudit_HandlerOutcomeCodeOverride(t *testing.T) {
	k := &businesskey.Key{ID: 14}
	logger := &fakeAuditLogger{}
	r := newAdminEngine()
	r.Use(RequestID(), injectBusinessKey(k), BusinessAudit(logger, newAuditWriteFailedCounter()))
	r.POST("/x", func(c *gin.Context) {
		// handler 显式注入业务级 outcome code
		SetBusinessAuditOutcomeCode(c, "insufficient_quota")
		c.AbortWithStatusJSON(http.StatusPaymentRequired, gin.H{})
	})

	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	recs := logger.snapshot()
	require.Len(t, recs, 1)
	assert.Equal(t, "insufficient_quota", recs[0].OutcomeCode,
		"handler SetBusinessAuditOutcomeCode 应覆盖默认 HTTP-status 推断")
}

func TestBusinessAudit_Tier1WriteFailure_BumpsMetric(t *testing.T) {
	k := &businesskey.Key{ID: 15}
	logger := &fakeAuditLogger{emitErr: errors.New("disk full")}
	cnt := newAuditWriteFailedCounter()
	r := newAdminEngine()
	r.Use(RequestID(), injectBusinessKey(k), BusinessAudit(logger, cnt))
	r.POST("/x", func(c *gin.Context) {
		c.AbortWithStatusJSON(http.StatusBadGateway, gin.H{})
	})

	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	_ = w
	assert.Equal(t, float64(1), testutil.ToFloat64(cnt.WithLabelValues("tier1", "emit_error")))
}

// =============================================================================
// 4 件套全链路集成
// =============================================================================

func TestBusinessChain_HappyPath_FullChain(t *testing.T) {
	k := &businesskey.Key{
		ID:                21,
		BusinessAccountID: "tenant-chain",
		Description:       "chain-test",
	}
	svc := &fakeBusinessService{keys: map[string]*businesskey.Key{"biz-sk-chain": k}}
	rpm := newBusinessTestRPM(t)
	logger := &fakeAuditLogger{}

	r := newAdminEngine()
	r.Use(RequestID())
	v1 := r.Group("/v1")
	v1.Use(
		BusinessBodyLimit(newBusinessBodyTooLargeCounter()),
		BusinessKeyAuth(svc, newBusinessAuthFailedCounter()),
		BusinessRPM(rpm, newBusinessQuotaCounter()),
		BusinessAudit(logger, newAuditWriteFailedCounter()),
	)
	v1.POST("/chat/completions", func(c *gin.Context) {
		SetBusinessAuditTokens(c, 50, 100)
		SetBusinessAuditCost(c, 240)
		SetBusinessAuditModelInfo(c, "gw-default", "doubao")
		SetBusinessAuditUpstreamResult(c, 200, 500*1e6)
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"x":1}`))
	req.Header.Set("Authorization", "Bearer biz-sk-chain")
	req.RemoteAddr = "127.0.0.1:1234"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	recs := logger.snapshot()
	require.Len(t, recs, 1)
	assert.Equal(t, "tenant-chain", recs[0].BusinessAccountID)
	assert.Equal(t, int64(21), recs[0].APIKeyID)
	assert.Equal(t, audit.Tier2, recs[0].Tier)
}

func TestBusinessChain_RPMExceededAfterAuth_NotConsumeQuotaOnAuthFail(t *testing.T) {
	// 验证关键安全契约（plan §鉴权五件套）：auth 失败时不消耗 RPM 配额
	limit := int32(2)
	k := &businesskey.Key{ID: 22, BusinessAccountID: "tenant-x", RequestsPerMinute: &limit}
	svc := &fakeBusinessService{keys: map[string]*businesskey.Key{"biz-sk-x": k}}
	rpm := newBusinessTestRPM(t)

	r := newAdminEngine()
	r.Use(RequestID())
	v1 := r.Group("/v1")
	v1.Use(
		BusinessKeyAuth(svc, newBusinessAuthFailedCounter()),
		BusinessRPM(rpm, newBusinessQuotaCounter()),
	)
	v1.POST("/chat/completions", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	// 5 次错误 Bearer（auth 失败 → 不触达 RPM）
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		req.Header.Set("Authorization", "Bearer biz-sk-wrong")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		require.Equal(t, http.StatusUnauthorized, w.Code)
	}

	// 正确 Bearer 仍可调 2 次（RPM 未被消耗）
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		req.Header.Set("Authorization", "Bearer biz-sk-x")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code)
	}
	// 第 3 次正确 Bearer → RPM 拒
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer biz-sk-x")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusTooManyRequests, w.Code)
}

// =============================================================================
// helpers — internal
// =============================================================================

// readAllBody 测试 helper：读完 body；EOF 时返累积 bytes + nil；其他错（含 MaxBytesError）原样返。
func readAllBody(c *gin.Context) ([]byte, error) {
	return io.ReadAll(c.Request.Body)
}

// silentSlog 全静默 slog logger，用于 RPM 构造避免污染测试输出。
func silentSlog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

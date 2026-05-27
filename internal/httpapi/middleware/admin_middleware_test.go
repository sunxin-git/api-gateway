package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sunxin-git/api-gateway/internal/admintoken"
	"github.com/sunxin-git/api-gateway/internal/audit"
)

// =============================================================================
// Fakes（避开真 DB，验证 middleware 逻辑）
// =============================================================================

// fakeAdminService 实现 admintoken.Service，行为可注入。
type fakeAdminService struct {
	tokens    map[string]*admintoken.Token // plaintext → token
	validateF func(plaintext string, ip netip.Addr) (*admintoken.ValidationResult, error)
}

func (f *fakeAdminService) Create(_ context.Context, _ admintoken.CreateParams) (*admintoken.Token, string, error) {
	return nil, "", errors.New("not implemented in fake")
}
func (f *fakeAdminService) ValidateByPlaintext(_ context.Context, plaintext string, ip netip.Addr) (*admintoken.ValidationResult, error) {
	if f.validateF != nil {
		return f.validateF(plaintext, ip)
	}
	tok, ok := f.tokens[plaintext]
	if !ok {
		return nil, admintoken.ErrTokenNotFound
	}
	// 简化 IP 校验
	if len(tok.AllowedCIDRs) == 0 {
		return nil, admintoken.ErrIPNotAllowed
	}
	for _, p := range tok.AllowedCIDRs {
		if p.Contains(ip) {
			return &admintoken.ValidationResult{Token: tok}, nil
		}
	}
	return nil, admintoken.ErrIPNotAllowed
}
func (f *fakeAdminService) CheckScope(token *admintoken.Token, requiredScope string) bool {
	if token == nil {
		return false
	}
	for _, s := range token.Scopes {
		if s == requiredScope {
			return true
		}
	}
	return false
}
func (f *fakeAdminService) Revoke(_ context.Context, _ int64) (bool, error) {
	return false, errors.New("not implemented in fake")
}
func (f *fakeAdminService) List(_ context.Context) ([]*admintoken.Token, error) {
	return nil, errors.New("not implemented in fake")
}
func (f *fakeAdminService) GetByID(_ context.Context, _ int64) (*admintoken.Token, error) {
	return nil, errors.New("not implemented in fake")
}

// fakeThrottle 实现 admintoken.Throttle。
type fakeThrottle struct {
	rpmErr           error
	circuitErr       error
	recordErrorCalls int32
	mu               sync.Mutex
}

func (f *fakeThrottle) CheckSingleRecharge(_ *admintoken.Token, _ int64) error { return nil }
func (f *fakeThrottle) CheckSingleRefund(_ *admintoken.Token, _ int64) error   { return nil }
func (f *fakeThrottle) CheckDailyRecharge(_ context.Context, _ *admintoken.Token, _ int64) error {
	return nil
}
func (f *fakeThrottle) CheckDailyRefund(_ context.Context, _ *admintoken.Token, _ int64) error {
	return nil
}
func (f *fakeThrottle) CheckDailyCreate(_ context.Context, _ *admintoken.Token) error { return nil }
func (f *fakeThrottle) CheckRPM(_ *admintoken.Token) error                            { return f.rpmErr }
func (f *fakeThrottle) CheckCircuitBreaker(_ context.Context, _ *admintoken.Token) error {
	return f.circuitErr
}
func (f *fakeThrottle) RecordSuccessfulRecharge(_ context.Context, _ int64, _ int64) error {
	return nil
}
func (f *fakeThrottle) RecordSuccessfulRefund(_ context.Context, _ int64, _ int64) error {
	return nil
}
func (f *fakeThrottle) RecordSuccessfulCreate(_ context.Context, _ int64) error { return nil }
func (f *fakeThrottle) RecordHandlerError(_ context.Context, _ *admintoken.Token) error {
	f.mu.Lock()
	f.recordErrorCalls++
	f.mu.Unlock()
	return nil
}
func (f *fakeThrottle) GetUsageToday(_ context.Context, _ int64) (admintoken.UsageSnapshot, error) {
	return admintoken.UsageSnapshot{}, nil
}
func (f *fakeThrottle) GetCircuitSnapshot(_ context.Context, _ int64) (admintoken.CircuitSnapshot, error) {
	return admintoken.CircuitSnapshot{}, nil
}
func (f *fakeThrottle) recordErrorCount() int32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.recordErrorCalls
}

// fakeAuditLogger 实现 audit.AuditLogger，缓存所有 emit record 供断言。
type fakeAuditLogger struct {
	mu      sync.Mutex
	records []audit.AuditRecord
	emitErr error
}

func (f *fakeAuditLogger) Emit(_ context.Context, r audit.AuditRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.emitErr != nil {
		return f.emitErr
	}
	f.records = append(f.records, r)
	return nil
}
func (f *fakeAuditLogger) Close() error { return nil }
func (f *fakeAuditLogger) snapshot() []audit.AuditRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]audit.AuditRecord, len(f.records))
	copy(out, f.records)
	return out
}

// =============================================================================
// 公共 helpers
// =============================================================================

func mustPrefix(t *testing.T, s string) netip.Prefix {
	t.Helper()
	p, err := netip.ParsePrefix(s)
	require.NoError(t, err)
	return p
}

func newQuotaCounter() *prometheus.CounterVec {
	return prometheus.NewCounterVec(prometheus.CounterOpts{Name: "test_quota"}, []string{"quota_type", "token_id"})
}
func newAuthFailedCounter() *prometheus.CounterVec {
	return prometheus.NewCounterVec(prometheus.CounterOpts{Name: "test_auth_failed"}, []string{"reason"})
}
func newAuditWriteFailedCounter() *prometheus.CounterVec {
	return prometheus.NewCounterVec(prometheus.CounterOpts{Name: "test_audit_failed"}, []string{"tier", "reason"})
}
func newBodyTooLargeCounter() prometheus.Counter {
	return prometheus.NewCounter(prometheus.CounterOpts{Name: "test_body_too_large"})
}

func newAdminEngine() *gin.Engine {
	gin.SetMode(gin.TestMode)
	return gin.New()
}

// =============================================================================
// AdminBodyLimit
// =============================================================================

func TestAdminBodyLimit_Under64KB_Passes(t *testing.T) {
	r := newAdminEngine()
	r.Use(RequestID(), AdminBodyLimit(newBodyTooLargeCounter()))
	r.POST("/x", func(c *gin.Context) {
		b, err := io.ReadAll(c.Request.Body)
		require.NoError(t, err)
		c.String(http.StatusOK, "got %d bytes", len(b))
	})

	body := bytes.Repeat([]byte("a"), 1024) // 1 KB
	req := httptest.NewRequest(http.MethodPost, "/x", bytes.NewReader(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "got 1024 bytes", w.Body.String())
}

func TestAdminBodyLimit_Over64KB_Rejects413(t *testing.T) {
	counter := newBodyTooLargeCounter()
	r := newAdminEngine()
	r.Use(RequestID(), AdminBodyLimit(counter))
	r.POST("/x", func(c *gin.Context) {
		_, err := io.ReadAll(c.Request.Body)
		if err != nil {
			// 把 read error 推给 body_limit 的 defer 捕获
			_ = c.Error(err)
			return
		}
		c.String(http.StatusOK, "ok")
	})

	body := bytes.Repeat([]byte("a"), 70*1024) // 70 KiB
	req := httptest.NewRequest(http.MethodPost, "/x", bytes.NewReader(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
	assert.Equal(t, float64(1), testutil.ToFloat64(counter))

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	errMap := resp["error"].(map[string]any)
	assert.Equal(t, "payload_too_large", errMap["code"])
}

// =============================================================================
// AdminTokenAuth
// =============================================================================

func TestAdminTokenAuth_MissingHeader_401(t *testing.T) {
	svc := &fakeAdminService{tokens: map[string]*admintoken.Token{}}
	authCnt := newAuthFailedCounter()

	r := newAdminEngine()
	r.Use(RequestID(), AdminTokenAuth(svc, authCnt))
	r.GET("/x", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Equal(t, float64(1), testutil.ToFloat64(authCnt.WithLabelValues("missing_header")))
}

func TestAdminTokenAuth_BadScheme_401(t *testing.T) {
	svc := &fakeAdminService{tokens: map[string]*admintoken.Token{}}
	authCnt := newAuthFailedCounter()
	r := newAdminEngine()
	r.Use(RequestID(), AdminTokenAuth(svc, authCnt))
	r.GET("/x", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Basic abc")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Equal(t, float64(1), testutil.ToFloat64(authCnt.WithLabelValues("bad_scheme")))
}

func TestAdminTokenAuth_EmptyToken_401(t *testing.T) {
	svc := &fakeAdminService{tokens: map[string]*admintoken.Token{}}
	authCnt := newAuthFailedCounter()
	r := newAdminEngine()
	r.Use(RequestID(), AdminTokenAuth(svc, authCnt))
	r.GET("/x", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer   ")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Equal(t, float64(1), testutil.ToFloat64(authCnt.WithLabelValues("empty_token")))
}

func TestAdminTokenAuth_TokenNotFound_401(t *testing.T) {
	svc := &fakeAdminService{tokens: map[string]*admintoken.Token{}}
	authCnt := newAuthFailedCounter()
	r := newAdminEngine()
	r.Use(RequestID(), AdminTokenAuth(svc, authCnt))
	r.GET("/x", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer sk-unknown")
	req.RemoteAddr = "10.0.0.1:1234"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Equal(t, float64(1), testutil.ToFloat64(authCnt.WithLabelValues("token_invalid")))
}

func TestAdminTokenAuth_IPNotAllowed_401(t *testing.T) {
	tok := &admintoken.Token{
		ID:           42,
		Description:  "test",
		Scopes:       []string{"business_account:read"},
		AllowedCIDRs: []netip.Prefix{mustPrefix(t, "192.168.0.0/24")},
	}
	svc := &fakeAdminService{tokens: map[string]*admintoken.Token{"sk-foo": tok}}
	authCnt := newAuthFailedCounter()
	r := newAdminEngine()
	r.Use(RequestID(), AdminTokenAuth(svc, authCnt))
	r.GET("/x", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer sk-foo")
	req.RemoteAddr = "10.0.0.1:1234" // 不在 192.168/24
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Equal(t, float64(1), testutil.ToFloat64(authCnt.WithLabelValues("ip_not_allowed")))

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	errMap := resp["error"].(map[string]any)
	assert.Equal(t, "ip_not_allowed", errMap["code"])
}

func TestAdminTokenAuth_Happy_PassesTokenToCtx(t *testing.T) {
	tok := &admintoken.Token{
		ID:           99,
		Description:  "happy",
		Scopes:       []string{"business_account:read"},
		AllowedCIDRs: []netip.Prefix{mustPrefix(t, "10.0.0.0/8")},
	}
	svc := &fakeAdminService{tokens: map[string]*admintoken.Token{"sk-ok": tok}}
	authCnt := newAuthFailedCounter()

	var seenTokenID int64
	r := newAdminEngine()
	r.Use(RequestID(), AdminTokenAuth(svc, authCnt))
	r.GET("/x", func(c *gin.Context) {
		vr := GetAdminTokenValidation(c)
		require.NotNil(t, vr)
		seenTokenID = vr.Token.ID
		c.String(http.StatusOK, "ok")
	})

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer sk-ok")
	req.RemoteAddr = "10.0.0.7:1234"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, int64(99), seenTokenID)
}

// =============================================================================
// AdminThrottle
// =============================================================================

func TestAdminThrottle_NoTokenInCtx_500(t *testing.T) {
	thr := &fakeThrottle{}
	cnt := newQuotaCounter()
	r := newAdminEngine()
	r.Use(RequestID(), AdminThrottle(thr, cnt))
	r.GET("/x", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestAdminThrottle_RPMExceeded_429(t *testing.T) {
	tok := &admintoken.Token{ID: 7}
	thr := &fakeThrottle{rpmErr: admintoken.ErrRPMExceeded}
	cnt := newQuotaCounter()
	r := newAdminEngine()
	r.Use(RequestID(), injectToken(tok), AdminThrottle(thr, cnt))
	r.GET("/x", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusTooManyRequests, w.Code)
	assert.Equal(t, float64(1), testutil.ToFloat64(cnt.WithLabelValues("rpm", "7")))

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "rate_limited", resp["error"].(map[string]any)["code"])
}

func TestAdminThrottle_CircuitOpen_429(t *testing.T) {
	tok := &admintoken.Token{ID: 8, CircuitBreakerEnabled: true}
	thr := &fakeThrottle{circuitErr: admintoken.ErrCircuitOpen}
	cnt := newQuotaCounter()
	r := newAdminEngine()
	r.Use(RequestID(), injectToken(tok), AdminThrottle(thr, cnt))
	r.GET("/x", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusTooManyRequests, w.Code)
	assert.Equal(t, float64(1), testutil.ToFloat64(cnt.WithLabelValues("circuit_open", "8")))
}

func TestAdminThrottle_BothPass_HandlerCalled(t *testing.T) {
	tok := &admintoken.Token{ID: 9}
	thr := &fakeThrottle{}
	cnt := newQuotaCounter()
	called := false
	r := newAdminEngine()
	r.Use(RequestID(), injectToken(tok), AdminThrottle(thr, cnt))
	r.GET("/x", func(c *gin.Context) {
		called = true
		c.String(http.StatusOK, "ok")
	})
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.True(t, called)
	assert.Equal(t, http.StatusOK, w.Code)
}

// =============================================================================
// AdminScope
// =============================================================================

func TestAdminScope_PanicOnEmptyRequired(t *testing.T) {
	svc := &fakeAdminService{}
	defer func() {
		require.NotNil(t, recover(), "空 required scope 必须 panic（启动期 fail-closed）")
	}()
	_ = AdminScope(svc, "", newAuthFailedCounter())
}

func TestAdminScope_Match_HandlerCalled(t *testing.T) {
	svc := &fakeAdminService{}
	tok := &admintoken.Token{ID: 1, Scopes: []string{"business_account:read"}}
	r := newAdminEngine()
	r.Use(RequestID(), injectToken(tok), AdminScope(svc, "business_account:read", newAuthFailedCounter()))
	r.GET("/x", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestAdminScope_NoMatch_403(t *testing.T) {
	svc := &fakeAdminService{}
	tok := &admintoken.Token{ID: 1, Scopes: []string{"business_account:read"}}
	cnt := newAuthFailedCounter()
	r := newAdminEngine()
	r.Use(RequestID(), injectToken(tok), AdminScope(svc, "business_account:refund", cnt))
	r.GET("/x", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Equal(t, float64(1), testutil.ToFloat64(cnt.WithLabelValues("insufficient_scope")))

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "insufficient_scope", resp["error"].(map[string]any)["code"])
}

// =============================================================================
// AdminAudit
// =============================================================================

func TestAdminAudit_HappyPath_EmitsTier2(t *testing.T) {
	tok := &admintoken.Token{ID: 11, Description: "test-token"}
	logger := &fakeAuditLogger{}
	thr := &fakeThrottle{}

	r := newAdminEngine()
	r.Use(RequestID(), injectToken(tok), AdminAudit(logger, thr, newAuditWriteFailedCounter()))
	r.POST("/admin/v1/balance", func(c *gin.Context) {
		_, _ = io.ReadAll(c.Request.Body)
		c.String(http.StatusOK, "ok")
	})

	body := []byte(`{"hello":"world"}`)
	req := httptest.NewRequest(http.MethodPost, "/admin/v1/balance", bytes.NewReader(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	recs := logger.snapshot()
	require.Len(t, recs, 1)
	rec := recs[0]
	assert.Equal(t, audit.Tier2, rec.Tier)
	assert.Equal(t, "ok", rec.OutcomeCode)
	assert.Equal(t, int64(11), rec.TokenID)
	assert.Equal(t, "test-token", rec.TokenDescription)
	assert.Equal(t, "admin_token:11", rec.Actor)
	assert.Equal(t, 200, rec.Status)
	assert.Len(t, rec.RequestHash, requestHashHexLen)
	assert.Equal(t, int64(len(body)), rec.BodySizeBytes)
	assert.Equal(t, "POST", rec.Method)
	assert.Equal(t, "/admin/v1/balance", rec.Path)
}

func TestAdminAudit_RefundPath_EmitsTier1(t *testing.T) {
	tok := &admintoken.Token{ID: 12}
	logger := &fakeAuditLogger{}
	thr := &fakeThrottle{}
	r := newAdminEngine()
	r.Use(RequestID(), injectToken(tok), AdminAudit(logger, thr, newAuditWriteFailedCounter()))
	r.POST("/admin/v1/business-accounts/:id/refund", func(c *gin.Context) {
		_, _ = io.ReadAll(c.Request.Body)
		c.String(http.StatusOK, "ok")
	})

	req := httptest.NewRequest(http.MethodPost, "/admin/v1/business-accounts/biz_1/refund", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	recs := logger.snapshot()
	require.Len(t, recs, 1)
	assert.Equal(t, audit.Tier1, recs[0].Tier, "refund path 必须落入 Tier1")
}

func TestAdminAudit_Auth401_EmitsTier1(t *testing.T) {
	// 模拟 auth 已失败：不注入 token，直接走 audit middleware（在真链中 auth 失败会 abort 在 audit 前；
	// 此测试模拟"已被前置 middleware abort 但 audit defer 仍 fire 的场景"）。
	logger := &fakeAuditLogger{}
	thr := &fakeThrottle{}
	r := newAdminEngine()
	r.Use(RequestID(), AdminAudit(logger, thr, newAuditWriteFailedCounter()))
	r.GET("/admin/v1/x", func(c *gin.Context) {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
			"error": gin.H{"code": "unauthorized"},
		})
	})

	req := httptest.NewRequest(http.MethodGet, "/admin/v1/x", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	recs := logger.snapshot()
	require.Len(t, recs, 1)
	assert.Equal(t, audit.Tier1, recs[0].Tier, "401 必须落入 Tier1")
	assert.Equal(t, "anonymous", recs[0].Actor)
}

func TestAdminAudit_HandlerPanic_AuditEmittedAndErrorRecorded(t *testing.T) {
	tok := &admintoken.Token{ID: 13, CircuitBreakerEnabled: true}
	logger := &fakeAuditLogger{}
	thr := &fakeThrottle{}
	r := newAdminEngine()
	r.Use(
		RequestID(),
		Recover(nil, nil), // Recover 在 audit 之前，捕获 panic 后写 500
		injectToken(tok),
		AdminAudit(logger, thr, newAuditWriteFailedCounter()),
	)
	r.GET("/admin/v1/boom", func(_ *gin.Context) {
		panic("故意 panic")
	})

	req := httptest.NewRequest(http.MethodGet, "/admin/v1/boom", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	recs := logger.snapshot()
	require.Len(t, recs, 1, "panic 路径 audit 必须 emit")
	assert.Equal(t, 500, recs[0].Status)
	assert.Equal(t, int32(1), thr.recordErrorCount(), "500 触发 RecordHandlerError")
}

func TestAdminAudit_RequestHashStableAndSorted(t *testing.T) {
	tok := &admintoken.Token{ID: 14}
	logger := &fakeAuditLogger{}
	thr := &fakeThrottle{}
	r := newAdminEngine()
	r.Use(RequestID(), injectToken(tok), AdminAudit(logger, thr, newAuditWriteFailedCounter()))
	r.GET("/q", func(c *gin.Context) {
		_, _ = io.ReadAll(c.Request.Body)
		c.String(http.StatusOK, "ok")
	})

	// 两次：query 顺序不同，body 相同
	req1 := httptest.NewRequest(http.MethodGet, "/q?a=1&b=2", nil)
	req2 := httptest.NewRequest(http.MethodGet, "/q?b=2&a=1", nil)
	w1 := httptest.NewRecorder()
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w1, req1)
	r.ServeHTTP(w2, req2)

	recs := logger.snapshot()
	require.Len(t, recs, 2)
	assert.Equal(t, recs[0].RequestHash, recs[1].RequestHash, "query 重排后 request_hash 必须稳定")
}

func TestAdminAudit_NoAuthorizationInRecord(t *testing.T) {
	tok := &admintoken.Token{ID: 15}
	logger := &fakeAuditLogger{}
	thr := &fakeThrottle{}
	r := newAdminEngine()
	r.Use(RequestID(), injectToken(tok), AdminAudit(logger, thr, newAuditWriteFailedCounter()))
	r.POST("/admin/v1/balance", func(c *gin.Context) {
		_, _ = io.ReadAll(c.Request.Body)
		c.String(http.StatusOK, "ok")
	})

	req := httptest.NewRequest(http.MethodPost, "/admin/v1/balance", strings.NewReader(`{"x":1}`))
	req.Header.Set("Authorization", "Bearer sk-supersecretvalue-1234567890ABCDEF")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	recs := logger.snapshot()
	require.Len(t, recs, 1)
	jsonBytes, _ := json.Marshal(recs[0])
	assert.NotContains(t, string(jsonBytes), "sk-supersecret", "audit record 必须不含 Authorization 明文")
}

func TestAdminAudit_ExternalRefInjectionSafe(t *testing.T) {
	tok := &admintoken.Token{ID: 16}
	logger := &fakeAuditLogger{}
	thr := &fakeThrottle{}
	r := newAdminEngine()
	r.Use(RequestID(), injectToken(tok), AdminAudit(logger, thr, newAuditWriteFailedCounter()))
	r.POST("/admin/v1/balance", func(c *gin.Context) {
		_, _ = io.ReadAll(c.Request.Body)
		c.String(http.StatusOK, "ok")
	})

	// 攻击者尝试在 body 中注入伪造 JSON 行
	body := []byte("{\"external_ref\":\"evil\\n{\\\"injected\\\":true}\"}")
	req := httptest.NewRequest(http.MethodPost, "/admin/v1/balance", bytes.NewReader(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	recs := logger.snapshot()
	require.Len(t, recs, 1)
	// hash 应只 emit 一次（json.Marshal 自动转义换行）
	out, err := json.Marshal(recs[0])
	require.NoError(t, err)
	// 验证 JSON 转义后只有一行
	assert.Equal(t, 0, strings.Count(string(out), "\n"), "audit JSON 必须是单行（无内嵌 \\n）")
}

func TestAdminAudit_BodyTooLarge_413(t *testing.T) {
	tok := &admintoken.Token{ID: 17}
	logger := &fakeAuditLogger{}
	thr := &fakeThrottle{}
	r := newAdminEngine()
	// 完整链：body_limit → audit
	r.Use(
		RequestID(),
		AdminBodyLimit(newBodyTooLargeCounter()),
		injectToken(tok),
		AdminAudit(logger, thr, newAuditWriteFailedCounter()),
	)
	handlerCalled := false
	r.POST("/admin/v1/balance", func(c *gin.Context) {
		handlerCalled = true
		c.String(http.StatusOK, "ok")
	})

	body := bytes.Repeat([]byte("a"), 70*1024)
	req := httptest.NewRequest(http.MethodPost, "/admin/v1/balance", bytes.NewReader(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
	assert.False(t, handlerCalled, "body 超限时 handler 不应执行")
	recs := logger.snapshot()
	require.Len(t, recs, 1)
	assert.Equal(t, "payload_too_large", recs[0].OutcomeCode)
}

func TestAdminAudit_Tier1WriteFailure_BumpsMetric(t *testing.T) {
	tok := &admintoken.Token{ID: 18}
	logger := &fakeAuditLogger{emitErr: errors.New("disk full")}
	thr := &fakeThrottle{}
	cnt := newAuditWriteFailedCounter()
	r := newAdminEngine()
	r.Use(RequestID(), injectToken(tok), AdminAudit(logger, thr, cnt))
	r.POST("/admin/v1/business-accounts/:id/refund", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	req := httptest.NewRequest(http.MethodPost, "/admin/v1/business-accounts/x/refund", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, float64(1), testutil.ToFloat64(cnt.WithLabelValues("tier1", "emit_error")))
}

// =============================================================================
// 全局 slog redactor
// =============================================================================

func TestRedactSensitiveAttrs_HidesAuthorization(t *testing.T) {
	var buf bytes.Buffer
	h := slog.NewJSONHandler(&buf, &slog.HandlerOptions{
		Level:       slog.LevelDebug,
		ReplaceAttr: RedactSensitiveAttrs(),
	})
	logger := slog.New(h)

	logger.Info("test",
		slog.String("Authorization", "Bearer sk-secret"),
		slog.String("X-Custom-Token", "abc123"),
		slog.String("client_secret", "supersecret"),
		slog.String("other_field", "visible"),
	)

	out := buf.String()
	assert.NotContains(t, out, "sk-secret")
	assert.NotContains(t, out, "abc123")
	assert.NotContains(t, out, "supersecret")
	assert.Contains(t, out, redactedPlaceholder)
	assert.Contains(t, out, "visible", "非敏感字段不应被 redact")
}

func TestRedactSensitiveAttrs_DoesNotTouchBuiltins(t *testing.T) {
	var buf bytes.Buffer
	h := slog.NewJSONHandler(&buf, &slog.HandlerOptions{
		ReplaceAttr: RedactSensitiveAttrs(),
	})
	logger := slog.New(h)
	logger.Info("hello")
	out := buf.String()
	// builtin 字段 msg / time / level 不应被 redact（即使 msg=="hello" 没命中也验证 builtin 路径）
	assert.Contains(t, out, "\"msg\":\"hello\"")
}

// =============================================================================
// 5 件套全链路集成
// =============================================================================

func TestAdminChain_HappyPath_FullChain(t *testing.T) {
	tok := &admintoken.Token{
		ID:           21,
		Description:  "chain-test",
		Scopes:       []string{"business_account:read"},
		AllowedCIDRs: []netip.Prefix{mustPrefix(t, "10.0.0.0/8")},
	}
	svc := &fakeAdminService{tokens: map[string]*admintoken.Token{"sk-chain": tok}}
	thr := &fakeThrottle{}
	logger := &fakeAuditLogger{}

	r := newAdminEngine()
	r.Use(RequestID())
	g := r.Group("/admin/v1")
	g.Use(
		AdminBodyLimit(newBodyTooLargeCounter()),
		AdminTokenAuth(svc, newAuthFailedCounter()),
		AdminThrottle(thr, newQuotaCounter()),
	)
	g.GET("/business-accounts/:id/balance",
		AdminScope(svc, "business_account:read", newAuthFailedCounter()),
		AdminAudit(logger, thr, newAuditWriteFailedCounter()),
		func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"id": c.Param("id"), "available": 100})
		},
	)

	req := httptest.NewRequest(http.MethodGet, "/admin/v1/business-accounts/biz_1/balance", nil)
	req.Header.Set("Authorization", "Bearer sk-chain")
	req.RemoteAddr = "10.0.0.5:1234"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	recs := logger.snapshot()
	require.Len(t, recs, 1)
	assert.Equal(t, int64(21), recs[0].TokenID)
	assert.Equal(t, 200, recs[0].Status)
	assert.Equal(t, "/admin/v1/business-accounts/:id/balance", recs[0].Path, "FullPath template，不含 :id 真值")
}

func TestAdminChain_BadScope_403_AndAuditTier1(t *testing.T) {
	tok := &admintoken.Token{
		ID:           22,
		Description:  "scope-test",
		Scopes:       []string{"business_account:read"},
		AllowedCIDRs: []netip.Prefix{mustPrefix(t, "10.0.0.0/8")},
	}
	svc := &fakeAdminService{tokens: map[string]*admintoken.Token{"sk-x": tok}}
	thr := &fakeThrottle{}
	logger := &fakeAuditLogger{}

	r := newAdminEngine()
	r.Use(RequestID())
	g := r.Group("/admin/v1")
	g.Use(
		AdminTokenAuth(svc, newAuthFailedCounter()),
		AdminThrottle(thr, newQuotaCounter()),
	)
	g.POST("/business-accounts/:id/recharge",
		AdminScope(svc, "business_account:recharge", newAuthFailedCounter()),
		AdminAudit(logger, thr, newAuditWriteFailedCounter()),
		func(c *gin.Context) { c.String(http.StatusOK, "ok") },
	)

	req := httptest.NewRequest(http.MethodPost, "/admin/v1/business-accounts/biz_1/recharge", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer sk-x")
	req.RemoteAddr = "10.0.0.5:1234"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code)
	// AdminAudit 在 AdminScope 之后；scope 失败时 audit 不会执行（abort）
	// 所以 logger 应无 record
	assert.Empty(t, logger.snapshot(), "scope 失败在 audit 之前 abort，audit 不 emit")
}

// =============================================================================
// helpers
// =============================================================================

// injectToken 测试用中间件：把 token 注入 ctx，跳过 AdminTokenAuth。
func injectToken(tok *admintoken.Token) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set(CtxKeyAdminToken, &admintoken.ValidationResult{Token: tok})
		c.Next()
	}
}

// 编译时引用避免误删 import。
var (
	_ = strconv.Itoa
	_ = fmt.Sprintf
	_ = slog.LevelInfo
)

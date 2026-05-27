package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sunxin-git/api-gateway/internal/admintoken"
	"github.com/sunxin-git/api-gateway/internal/audit"
	"github.com/sunxin-git/api-gateway/internal/db"
	"github.com/sunxin-git/api-gateway/internal/httpapi/middleware"
	"github.com/sunxin-git/api-gateway/internal/ledger"
)

// =============================================================================
// 测试基础设施
// =============================================================================

const defaultAdminTestDSN = "postgres://gateway:gateway_dev@127.0.0.1:55432/gateway?sslmode=disable"

func testDSN() string {
	if v := os.Getenv("ADMIN_TEST_PG_DSN"); v != "" {
		return v
	}
	return defaultAdminTestDSN
}

var testPepper = []byte("admin-handler-test-pepper-32-bytes!ZZ")

func mustOpenPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(testDSN())
	require.NoError(t, err)
	// MaxConns 取 20：避免与 ledger/admintoken 测试并行跑时把 PG 默认 100 连接池打满
	cfg.MaxConns = 20
	cfg.MinConns = 2
	cfg.MaxConnLifetime = 30 * time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("跳过：无法连 PG (%s)：%v", testDSN(), err)
	}
	return pool
}

// silentLogger 测试用静默 logger。
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// =============================================================================
// 测试用 outbox publisher（直接 INSERT；与 ledger/testutil.go 同构）
// =============================================================================

type testOutbox struct{}

func (o *testOutbox) PublishInTx(ctx context.Context, tx pgx.Tx, event ledger.Event) error {
	accIDArg := pgtype.Text{}
	if event.BusinessAccountID != "" {
		accIDArg.String = event.BusinessAccountID
		accIDArg.Valid = true
	}
	q := db.New(tx)
	_, err := q.InsertOutboxEvent(ctx, db.InsertOutboxEventParams{
		BusinessAccountID:      accIDArg,
		EventType:              string(event.Type),
		Payload:                event.Payload,
		IsFinancial:            event.IsFinancial,
		RetentionUntil:         event.RetentionUntil,
		DeliveryIdempotencyKey: event.DeliveryIdempotencyKey,
	})
	return err
}

// testSuite 提供 handler + 依赖 + helper。
type testSuite struct {
	pool       *pgxpool.Pool
	ledgerSvc  *ledger.PostgresService
	tokenSvc   *admintoken.PostgresService
	rpm        *admintoken.InProcessRPM
	throttle   *admintoken.PostgresThrottle
	handler    *Handler
	auditMu    sync.Mutex
	auditSink  *recordingAudit
}

type recordingAudit struct {
	mu      sync.Mutex
	records []audit.AuditRecord
}

func (r *recordingAudit) Emit(_ context.Context, rec audit.AuditRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, rec)
	return nil
}
func (r *recordingAudit) Close() error { return nil }
func (r *recordingAudit) snapshot() []audit.AuditRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]audit.AuditRecord, len(r.records))
	copy(out, r.records)
	return out
}

func setupSuite(t *testing.T) *testSuite {
	t.Helper()
	pool := mustOpenPool(t)
	ledgerSvc := ledger.NewPostgresService(pool, &testOutbox{}, silentLogger())
	tokenSvc := admintoken.NewPostgresService(pool, testPepper, silentLogger())
	rpm := admintoken.NewInProcessRPM(silentLogger(), nil)
	throttle := admintoken.NewPostgresThrottle(pool, rpm, silentLogger())

	h := NewHandler(ledgerSvc, throttle, nil, silentLogger())

	s := &testSuite{
		pool:      pool,
		ledgerSvc: ledgerSvc,
		tokenSvc:  tokenSvc,
		rpm:       rpm,
		throttle:  throttle,
		handler:   h,
		auditSink: &recordingAudit{},
	}
	t.Cleanup(func() {
		_ = rpm.Close()
		pool.Close()
	})
	return s
}

// createToken 创建一个测试用 token；t.Cleanup 自动清理。
func (s *testSuite) createToken(t *testing.T, mutate func(*admintoken.CreateParams)) *admintoken.Token {
	t.Helper()
	params := admintoken.CreateParams{
		Description:           "admin-handler-test:" + t.Name(),
		Scopes:                []string{"business_account:create", "business_account:recharge", "business_account:refund", "business_account:read"},
		AllowedCIDRs:          []netip.Prefix{mustPrefix(t, "127.0.0.1/32"), mustPrefix(t, "10.0.0.0/8")},
		CircuitBreakerEnabled: false,
		CreatedBy:             "test:setup",
	}
	if mutate != nil {
		mutate(&params)
	}
	tok, _, err := s.tokenSvc.Create(context.Background(), params)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = s.pool.Exec(context.Background(), "DELETE FROM gateway_admin_token WHERE id = $1", tok.ID)
	})
	return tok
}

// cleanupAccount 删账户 + ledger + outbox + balance（绕过不可变 trigger）。
func (s *testSuite) cleanupAccount(t *testing.T, accountIDs ...string) {
	t.Helper()
	if len(accountIDs) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := s.pool.Acquire(ctx)
	require.NoError(t, err)
	defer conn.Release()
	_, err = conn.Exec(ctx, "SET session_replication_role = 'replica'")
	require.NoError(t, err)
	defer func() { _, _ = conn.Exec(context.Background(), "SET session_replication_role = 'origin'") }()
	for _, id := range accountIDs {
		for _, tbl := range []string{"business_account_ledger", "webhook_event_outbox", "business_account_balance", "business_account"} {
			col := "business_account_id"
			if tbl == "business_account" {
				col = "id"
			}
			_, err := conn.Exec(ctx, fmt.Sprintf("DELETE FROM %s WHERE %s = $1", tbl, col), id)
			require.NoError(t, err)
		}
	}
}

func mustPrefix(t *testing.T, s string) netip.Prefix {
	t.Helper()
	p, err := netip.ParsePrefix(s)
	require.NoError(t, err)
	return p
}

// =============================================================================
// 路由 / 请求 helper
// =============================================================================

// buildEngine 装配一个简化的 Gin engine，模拟 admin 中间件链：
//
//	RequestID → AdminBodyLimit → injectToken(预先 Validate) → AdminThrottle → AdminScope → AdminAudit → handler
//
// 测试用 Validate 直接绕过实际 Bearer 解析（因为本测试关注 handler 行为）；
// 但 AdminThrottle / Scope / Audit 都是真中间件。
func (s *testSuite) buildEngine(t *testing.T, tok *admintoken.Token) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()

	r.Use(middleware.RequestID())

	tokenInjector := func(c *gin.Context) {
		c.Set(middleware.CtxKeyAdminToken, &admintoken.ValidationResult{Token: tok})
		c.Next()
	}
	scopeOK := func(requiredScope string) gin.HandlerFunc {
		return middleware.AdminScope(s.tokenSvc, requiredScope, nil)
	}
	audit := middleware.AdminAudit(s.auditSink, s.throttle, nil)
	throttle := middleware.AdminThrottle(s.throttle, nil)

	g := r.Group("/admin/v1")
	g.Use(tokenInjector, throttle)

	g.POST("/business-accounts", scopeOK("business_account:create"), audit, s.handler.CreateAccount)
	g.POST("/business-accounts/:id/recharge", scopeOK("business_account:recharge"), audit, s.handler.Recharge)
	g.POST("/business-accounts/:id/refund", scopeOK("business_account:refund"), audit, s.handler.Refund)
	g.GET("/business-accounts/:id/balance", scopeOK("business_account:read"), audit, s.handler.GetBalance)
	g.GET("/whoami", audit, s.handler.Whoami)

	return r
}

func doRequest(r *gin.Engine, method, path string, body any) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func parseError(t *testing.T, w *httptest.ResponseRecorder) ErrorBody {
	t.Helper()
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	return resp.Error
}

// =============================================================================
// CreateAccount
// =============================================================================

func TestCreateAccount_Happy(t *testing.T) {
	s := setupSuite(t)
	tok := s.createToken(t, nil)
	r := s.buildEngine(t, tok)
	accID := "biz-create-happy"
	s.cleanupAccount(t, accID)
	t.Cleanup(func() { s.cleanupAccount(t, accID) })

	w := doRequest(r, http.MethodPost, "/admin/v1/business-accounts", map[string]any{
		"id":                 accID,
		"isolation_required": false,
	})
	assert.Equal(t, http.StatusCreated, w.Code)

	var resp AccountResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, accID, resp.ID)
	assert.Equal(t, "active", resp.Status)

	// daily_create_count 累加
	usage, err := s.throttle.GetUsageToday(context.Background(), tok.ID)
	require.NoError(t, err)
	assert.Equal(t, int32(1), usage.AccountCreateCount)
}

func TestCreateAccount_InvalidID(t *testing.T) {
	s := setupSuite(t)
	tok := s.createToken(t, nil)
	r := s.buildEngine(t, tok)

	cases := []struct {
		name string
		id   string
	}{
		{"empty", ""},
		{"whitespace", "   "},
		{"too_long", strings.Repeat("a", 65)},
		{"invalid_chars", "biz@example"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := doRequest(r, http.MethodPost, "/admin/v1/business-accounts", map[string]any{"id": tc.id})
			assert.Equal(t, http.StatusBadRequest, w.Code)
			assert.Equal(t, "invalid_request_body", parseError(t, w).Code)
		})
	}
}

func TestCreateAccount_Duplicate_409(t *testing.T) {
	s := setupSuite(t)
	tok := s.createToken(t, nil)
	r := s.buildEngine(t, tok)
	accID := "biz-dup"
	s.cleanupAccount(t, accID)
	t.Cleanup(func() { s.cleanupAccount(t, accID) })

	w1 := doRequest(r, http.MethodPost, "/admin/v1/business-accounts", map[string]any{"id": accID})
	require.Equal(t, http.StatusCreated, w1.Code)

	w2 := doRequest(r, http.MethodPost, "/admin/v1/business-accounts", map[string]any{"id": accID})
	assert.Equal(t, http.StatusConflict, w2.Code)
	assert.Equal(t, "account_already_exists", parseError(t, w2).Code)
}

func TestCreateAccount_DailyCreateLimit_429(t *testing.T) {
	s := setupSuite(t)
	limit := int32(1)
	tok := s.createToken(t, func(p *admintoken.CreateParams) {
		p.DailyAccountCreateLimit = &limit
	})
	r := s.buildEngine(t, tok)
	a1, a2 := "biz-dlimit-1", "biz-dlimit-2"
	s.cleanupAccount(t, a1, a2)
	t.Cleanup(func() { s.cleanupAccount(t, a1, a2) })

	w1 := doRequest(r, http.MethodPost, "/admin/v1/business-accounts", map[string]any{"id": a1})
	require.Equal(t, http.StatusCreated, w1.Code)

	w2 := doRequest(r, http.MethodPost, "/admin/v1/business-accounts", map[string]any{"id": a2})
	assert.Equal(t, http.StatusTooManyRequests, w2.Code)
	assert.Equal(t, "daily_create_exceeded", parseError(t, w2).Code)
}

// =============================================================================
// Recharge
// =============================================================================

func TestRecharge_Happy(t *testing.T) {
	s := setupSuite(t)
	tok := s.createToken(t, nil)
	r := s.buildEngine(t, tok)
	accID := "biz-rec-happy"
	s.cleanupAccount(t, accID)
	t.Cleanup(func() { s.cleanupAccount(t, accID) })

	require.Equal(t, http.StatusCreated,
		doRequest(r, http.MethodPost, "/admin/v1/business-accounts", map[string]any{"id": accID}).Code)

	w := doRequest(r, http.MethodPost, "/admin/v1/business-accounts/"+accID+"/recharge", map[string]any{
		"amount":       1000,
		"external_ref": "topup-001",
	})
	assert.Equal(t, http.StatusOK, w.Code)
	var entry LedgerEntryResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &entry))
	assert.Equal(t, int64(1000), entry.Amount)
	assert.Equal(t, "recharge", entry.EntryType)
	assert.False(t, entry.Idempotent)

	usage, _ := s.throttle.GetUsageToday(context.Background(), tok.ID)
	assert.Equal(t, int64(1000), usage.RechargeTotalMinor)
}

func TestRecharge_IdempotentReplay_NoDoubleQuota(t *testing.T) {
	s := setupSuite(t)
	tok := s.createToken(t, nil)
	r := s.buildEngine(t, tok)
	accID := "biz-rec-idem"
	s.cleanupAccount(t, accID)
	t.Cleanup(func() { s.cleanupAccount(t, accID) })

	require.Equal(t, http.StatusCreated,
		doRequest(r, http.MethodPost, "/admin/v1/business-accounts", map[string]any{"id": accID}).Code)

	body := map[string]any{"amount": 500, "external_ref": "topup-idem-1"}
	w1 := doRequest(r, http.MethodPost, "/admin/v1/business-accounts/"+accID+"/recharge", body)
	require.Equal(t, http.StatusOK, w1.Code)
	var e1 LedgerEntryResponse
	require.NoError(t, json.Unmarshal(w1.Body.Bytes(), &e1))
	assert.False(t, e1.Idempotent)

	w2 := doRequest(r, http.MethodPost, "/admin/v1/business-accounts/"+accID+"/recharge", body)
	require.Equal(t, http.StatusOK, w2.Code)
	var e2 LedgerEntryResponse
	require.NoError(t, json.Unmarshal(w2.Body.Bytes(), &e2))
	assert.Equal(t, e1.ID, e2.ID, "幂等命中返回原 entry")
	assert.True(t, e2.Idempotent)

	usage, _ := s.throttle.GetUsageToday(context.Background(), tok.ID)
	assert.Equal(t, int64(500), usage.RechargeTotalMinor, "幂等重放必须不双重累加配额")
}

func TestRecharge_IdempotencyConflict_409(t *testing.T) {
	s := setupSuite(t)
	tok := s.createToken(t, nil)
	r := s.buildEngine(t, tok)
	accID := "biz-rec-conflict"
	s.cleanupAccount(t, accID)
	t.Cleanup(func() { s.cleanupAccount(t, accID) })

	require.Equal(t, http.StatusCreated,
		doRequest(r, http.MethodPost, "/admin/v1/business-accounts", map[string]any{"id": accID}).Code)

	w1 := doRequest(r, http.MethodPost, "/admin/v1/business-accounts/"+accID+"/recharge", map[string]any{
		"amount":       100,
		"external_ref": "conflict-1",
	})
	require.Equal(t, http.StatusOK, w1.Code)

	w2 := doRequest(r, http.MethodPost, "/admin/v1/business-accounts/"+accID+"/recharge", map[string]any{
		"amount":       200, // 不同 amount → body sha 不同
		"external_ref": "conflict-1",
	})
	assert.Equal(t, http.StatusConflict, w2.Code)
	assert.Equal(t, "idempotency_conflict", parseError(t, w2).Code)
}

func TestRecharge_SingleRechargeMax_429(t *testing.T) {
	s := setupSuite(t)
	max := int64(500)
	tok := s.createToken(t, func(p *admintoken.CreateParams) { p.SingleRechargeMax = &max })
	r := s.buildEngine(t, tok)
	accID := "biz-rec-cap"
	s.cleanupAccount(t, accID)
	t.Cleanup(func() { s.cleanupAccount(t, accID) })

	require.Equal(t, http.StatusCreated,
		doRequest(r, http.MethodPost, "/admin/v1/business-accounts", map[string]any{"id": accID}).Code)

	w := doRequest(r, http.MethodPost, "/admin/v1/business-accounts/"+accID+"/recharge", map[string]any{
		"amount": 600, "external_ref": "x",
	})
	assert.Equal(t, http.StatusTooManyRequests, w.Code)
	assert.Equal(t, "single_recharge_exceeded", parseError(t, w).Code)

	// 阀门拒掉时 ledger 不应有 entry
	usage, _ := s.throttle.GetUsageToday(context.Background(), tok.ID)
	assert.Equal(t, int64(0), usage.RechargeTotalMinor)
}

func TestRecharge_DailyRechargeQuota_429(t *testing.T) {
	s := setupSuite(t)
	limit := int64(1000)
	tok := s.createToken(t, func(p *admintoken.CreateParams) { p.DailyRechargeQuotaLimit = &limit })
	r := s.buildEngine(t, tok)
	accID := "biz-rec-daily"
	s.cleanupAccount(t, accID)
	t.Cleanup(func() { s.cleanupAccount(t, accID) })

	require.Equal(t, http.StatusCreated,
		doRequest(r, http.MethodPost, "/admin/v1/business-accounts", map[string]any{"id": accID}).Code)

	require.Equal(t, http.StatusOK, doRequest(r, http.MethodPost, "/admin/v1/business-accounts/"+accID+"/recharge",
		map[string]any{"amount": 600, "external_ref": "tu-1"}).Code)
	w := doRequest(r, http.MethodPost, "/admin/v1/business-accounts/"+accID+"/recharge",
		map[string]any{"amount": 500, "external_ref": "tu-2"})
	assert.Equal(t, http.StatusTooManyRequests, w.Code)
	assert.Equal(t, "daily_recharge_quota_exceeded", parseError(t, w).Code)
}

func TestRecharge_AccountNotFound_404(t *testing.T) {
	s := setupSuite(t)
	tok := s.createToken(t, nil)
	r := s.buildEngine(t, tok)

	w := doRequest(r, http.MethodPost, "/admin/v1/business-accounts/nonexistent/recharge",
		map[string]any{"amount": 100, "external_ref": "x"})
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Equal(t, "account_not_found", parseError(t, w).Code)
}

func TestRecharge_InvalidAmount_400(t *testing.T) {
	s := setupSuite(t)
	tok := s.createToken(t, nil)
	r := s.buildEngine(t, tok)

	// amount=0：gin binding "required" 会先拦截（int64 零值 = 未填）。所以期望 400 invalid_request_body
	w := doRequest(r, http.MethodPost, "/admin/v1/business-accounts/x/recharge",
		map[string]any{"amount": 0, "external_ref": "x"})
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "invalid_request_body", parseError(t, w).Code)

	// 负数
	w2 := doRequest(r, http.MethodPost, "/admin/v1/business-accounts/x/recharge",
		map[string]any{"amount": -1, "external_ref": "x"})
	assert.Equal(t, http.StatusBadRequest, w2.Code)
}

// TestRecharge_ConcurrentSameToken_DailyCounterNoLoss 50 个 goroutine 同 token + 各自独立账户 →
// 测试 daily counter 在并发 IncrementTokenUsage 下不丢失。
//
// 注意：同账户高并发充值会触发 balance CAS ErrVersionConflict（503），是设计行为
// （D-min handler 不在 service 层重试 ErrVersionConflict，由业务系统重试）。
// 因此本测试故意拆成"每 goroutine 一账户"，等同于"业务系统对一批账户并发充值"，
// 验证 throttle daily counter（IncrementTokenUsage ON CONFLICT）的原子性。
func TestRecharge_ConcurrentSameToken_DailyCounterNoLoss(t *testing.T) {
	s := setupSuite(t)
	tok := s.createToken(t, nil)
	r := s.buildEngine(t, tok)

	const N = 50
	accIDs := make([]string, N)
	for i := 0; i < N; i++ {
		accIDs[i] = fmt.Sprintf("biz-rec-conc-%d", i)
	}
	s.cleanupAccount(t, accIDs...)
	t.Cleanup(func() { s.cleanupAccount(t, accIDs...) })

	// 先并发创账户
	var wgC sync.WaitGroup
	wgC.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wgC.Done()
			w := doRequest(r, http.MethodPost, "/admin/v1/business-accounts", map[string]any{"id": accIDs[i]})
			require.Equal(t, http.StatusCreated, w.Code, "create %s", accIDs[i])
		}()
	}
	wgC.Wait()

	// 50 goroutine 并发充值各自账户
	var wg sync.WaitGroup
	wg.Add(N)
	failures := make(chan string, N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			w := doRequest(r, http.MethodPost, "/admin/v1/business-accounts/"+accIDs[i]+"/recharge",
				map[string]any{"amount": 10, "external_ref": fmt.Sprintf("conc-%d", i)})
			if w.Code != http.StatusOK {
				failures <- fmt.Sprintf("idx=%d code=%d body=%s", i, w.Code, w.Body.String())
			}
		}()
	}
	wg.Wait()
	close(failures)
	for f := range failures {
		t.Log(f)
	}

	usage, _ := s.throttle.GetUsageToday(context.Background(), tok.ID)
	assert.Equal(t, int64(N*10), usage.RechargeTotalMinor, "并发累加无丢失")
}

// =============================================================================
// Refund
// =============================================================================

func TestRefund_Happy_AfterReserveCommit(t *testing.T) {
	s := setupSuite(t)
	tok := s.createToken(t, nil)
	r := s.buildEngine(t, tok)
	accID := "biz-refund-happy"
	s.cleanupAccount(t, accID)
	t.Cleanup(func() { s.cleanupAccount(t, accID) })

	// create + recharge 500
	require.Equal(t, http.StatusCreated,
		doRequest(r, http.MethodPost, "/admin/v1/business-accounts", map[string]any{"id": accID}).Code)
	require.Equal(t, http.StatusOK,
		doRequest(r, http.MethodPost, "/admin/v1/business-accounts/"+accID+"/recharge",
			map[string]any{"amount": 500, "external_ref": "tu-rfd"}).Code)

	// 通过 ledger 直接做 reserve + commit 200，剩 used=200 available=300
	actor := ledger.Actor{Type: ledger.ActorTypeAdminToken, ID: "test"}
	_, err := s.ledgerSvc.Reserve(context.Background(), actor, ledger.ReserveParams{
		AccountID: accID, Amount: 200, CorrelationID: "rsv-1",
	})
	require.NoError(t, err)
	_, err = s.ledgerSvc.Commit(context.Background(), actor, ledger.CommitParams{
		AccountID: accID, CorrelationID: "rsv-1", ActualCost: 200,
	})
	require.NoError(t, err)

	w := doRequest(r, http.MethodPost, "/admin/v1/business-accounts/"+accID+"/refund",
		map[string]any{"amount": 100, "correlation_id": "rfd-1"})
	assert.Equal(t, http.StatusOK, w.Code)

	var entry LedgerEntryResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &entry))
	assert.Equal(t, "refund", entry.EntryType)
	assert.False(t, entry.Idempotent)

	usage, _ := s.throttle.GetUsageToday(context.Background(), tok.ID)
	assert.Equal(t, int64(100), usage.RefundTotalMinor)
}

func TestRefund_InsufficientUsed_409(t *testing.T) {
	s := setupSuite(t)
	tok := s.createToken(t, nil)
	r := s.buildEngine(t, tok)
	accID := "biz-refund-insuf"
	s.cleanupAccount(t, accID)
	t.Cleanup(func() { s.cleanupAccount(t, accID) })

	require.Equal(t, http.StatusCreated,
		doRequest(r, http.MethodPost, "/admin/v1/business-accounts", map[string]any{"id": accID}).Code)
	require.Equal(t, http.StatusOK,
		doRequest(r, http.MethodPost, "/admin/v1/business-accounts/"+accID+"/recharge",
			map[string]any{"amount": 500, "external_ref": "tu-i"}).Code)
	// used=0；refund 100 失败
	w := doRequest(r, http.MethodPost, "/admin/v1/business-accounts/"+accID+"/refund",
		map[string]any{"amount": 100, "correlation_id": "rfd-x"})
	assert.Equal(t, http.StatusConflict, w.Code)
	assert.Equal(t, "insufficient_used", parseError(t, w).Code)
}

func TestRefund_IdempotentReplay(t *testing.T) {
	s := setupSuite(t)
	tok := s.createToken(t, nil)
	r := s.buildEngine(t, tok)
	accID := "biz-refund-idem"
	s.cleanupAccount(t, accID)
	t.Cleanup(func() { s.cleanupAccount(t, accID) })

	require.Equal(t, http.StatusCreated,
		doRequest(r, http.MethodPost, "/admin/v1/business-accounts", map[string]any{"id": accID}).Code)
	require.Equal(t, http.StatusOK,
		doRequest(r, http.MethodPost, "/admin/v1/business-accounts/"+accID+"/recharge",
			map[string]any{"amount": 500, "external_ref": "tu-rfdi"}).Code)
	actor := ledger.Actor{Type: ledger.ActorTypeAdminToken, ID: "test"}
	_, err := s.ledgerSvc.Reserve(context.Background(), actor, ledger.ReserveParams{
		AccountID: accID, Amount: 200, CorrelationID: "rsv-i",
	})
	require.NoError(t, err)
	_, err = s.ledgerSvc.Commit(context.Background(), actor, ledger.CommitParams{
		AccountID: accID, CorrelationID: "rsv-i", ActualCost: 200,
	})
	require.NoError(t, err)

	body := map[string]any{"amount": 50, "correlation_id": "rfd-replay"}
	w1 := doRequest(r, http.MethodPost, "/admin/v1/business-accounts/"+accID+"/refund", body)
	require.Equal(t, http.StatusOK, w1.Code)

	w2 := doRequest(r, http.MethodPost, "/admin/v1/business-accounts/"+accID+"/refund", body)
	require.Equal(t, http.StatusOK, w2.Code)
	var e2 LedgerEntryResponse
	require.NoError(t, json.Unmarshal(w2.Body.Bytes(), &e2))
	assert.True(t, e2.Idempotent, "同 correlation_id 第二次必须返 Idempotent=true")

	usage, _ := s.throttle.GetUsageToday(context.Background(), tok.ID)
	assert.Equal(t, int64(50), usage.RefundTotalMinor, "幂等命中不双计")
}

// =============================================================================
// GetBalance
// =============================================================================

func TestGetBalance_Happy(t *testing.T) {
	s := setupSuite(t)
	tok := s.createToken(t, nil)
	r := s.buildEngine(t, tok)
	accID := "biz-bal-happy"
	s.cleanupAccount(t, accID)
	t.Cleanup(func() { s.cleanupAccount(t, accID) })

	require.Equal(t, http.StatusCreated,
		doRequest(r, http.MethodPost, "/admin/v1/business-accounts", map[string]any{"id": accID}).Code)
	require.Equal(t, http.StatusOK,
		doRequest(r, http.MethodPost, "/admin/v1/business-accounts/"+accID+"/recharge",
			map[string]any{"amount": 1234, "external_ref": "x"}).Code)

	w := doRequest(r, http.MethodGet, "/admin/v1/business-accounts/"+accID+"/balance", nil)
	assert.Equal(t, http.StatusOK, w.Code)

	var bal BalanceResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &bal))
	assert.Equal(t, accID, bal.AccountID)
	assert.Equal(t, int64(1234), bal.Available)
	assert.Equal(t, int64(1234), bal.RechargeTotal)
	assert.False(t, bal.Frozen)
}

func TestGetBalance_NotFound_404(t *testing.T) {
	s := setupSuite(t)
	tok := s.createToken(t, nil)
	r := s.buildEngine(t, tok)
	w := doRequest(r, http.MethodGet, "/admin/v1/business-accounts/nope/balance", nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Equal(t, "account_not_found", parseError(t, w).Code)
}

// =============================================================================
// Whoami
// =============================================================================

func TestWhoami_Happy(t *testing.T) {
	s := setupSuite(t)
	srm := int64(500_000)
	rpm := int32(600)
	tok := s.createToken(t, func(p *admintoken.CreateParams) {
		p.SingleRechargeMax = &srm
		p.RequestsPerMinute = &rpm
		p.CircuitBreakerEnabled = true
	})
	r := s.buildEngine(t, tok)

	// 触发一次 RecordSuccessfulCreate 让 today_usage 不为零
	accID := "biz-whoami-x"
	s.cleanupAccount(t, accID)
	t.Cleanup(func() { s.cleanupAccount(t, accID) })
	require.Equal(t, http.StatusCreated,
		doRequest(r, http.MethodPost, "/admin/v1/business-accounts", map[string]any{"id": accID}).Code)

	w := doRequest(r, http.MethodGet, "/admin/v1/whoami", nil)
	require.Equal(t, http.StatusOK, w.Code)

	var resp WhoamiResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, tok.ID, resp.TokenID)
	assert.ElementsMatch(t, tok.Scopes, resp.Scopes)
	assert.Equal(t, 2, resp.IPAllowlistCIDRCount, "返回 CIDR 数量但不暴露具体 CIDR")
	assert.NotNil(t, resp.ThrottleLimits.SingleRechargeMax)
	assert.Equal(t, int64(500_000), *resp.ThrottleLimits.SingleRechargeMax)
	assert.True(t, resp.ThrottleLimits.CircuitBreakerEnabled)
	assert.Equal(t, int32(1), resp.TodayUsageUTC.AccountCreateCount)
	assert.False(t, resp.CircuitState.Open)

	// 双保险：response JSON 不应包含 token_hash / CIDR 具体网段（如 "10.0.0.0/8"）
	out := w.Body.String()
	assert.NotContains(t, out, "10.0.0.0/8")
	assert.NotContains(t, out, "token_hash")
}

func TestWhoami_NoScopeRequired(t *testing.T) {
	s := setupSuite(t)
	// 只给一个最小 scope；whoami 不应要求任何 scope
	tok := s.createToken(t, func(p *admintoken.CreateParams) {
		p.Scopes = []string{"business_account:read"}
	})
	r := s.buildEngine(t, tok)
	w := doRequest(r, http.MethodGet, "/admin/v1/whoami", nil)
	assert.Equal(t, http.StatusOK, w.Code)
}

// =============================================================================
// audit 联动
// =============================================================================

func TestAudit_RefundTier1Emitted(t *testing.T) {
	s := setupSuite(t)
	tok := s.createToken(t, nil)
	r := s.buildEngine(t, tok)
	accID := "biz-audit-rfd"
	s.cleanupAccount(t, accID)
	t.Cleanup(func() { s.cleanupAccount(t, accID) })

	require.Equal(t, http.StatusCreated,
		doRequest(r, http.MethodPost, "/admin/v1/business-accounts", map[string]any{"id": accID}).Code)
	require.Equal(t, http.StatusOK,
		doRequest(r, http.MethodPost, "/admin/v1/business-accounts/"+accID+"/recharge",
			map[string]any{"amount": 200, "external_ref": "tu"}).Code)
	actor := ledger.Actor{Type: ledger.ActorTypeAdminToken, ID: "test"}
	_, err := s.ledgerSvc.Reserve(context.Background(), actor, ledger.ReserveParams{
		AccountID: accID, Amount: 100, CorrelationID: "rsv",
	})
	require.NoError(t, err)
	_, err = s.ledgerSvc.Commit(context.Background(), actor, ledger.CommitParams{
		AccountID: accID, CorrelationID: "rsv", ActualCost: 100,
	})
	require.NoError(t, err)

	require.Equal(t, http.StatusOK,
		doRequest(r, http.MethodPost, "/admin/v1/business-accounts/"+accID+"/refund",
			map[string]any{"amount": 50, "correlation_id": "rfd"}).Code)

	recs := s.auditSink.snapshot()
	found := false
	for _, rec := range recs {
		if strings.Contains(rec.Path, "/refund") {
			found = true
			assert.Equal(t, audit.Tier1, rec.Tier, "refund audit 必须 Tier1")
			assert.Equal(t, 200, rec.Status)
			assert.Equal(t, tok.ID, rec.TokenID)
			break
		}
	}
	assert.True(t, found, "应至少一条 refund audit record")
}

// =============================================================================
// DTO validate 边界
// =============================================================================

func TestRechargeRequest_ValidateBoundary(t *testing.T) {
	cases := []struct {
		name string
		req  RechargeRequest
		want string
	}{
		{"amount_zero", RechargeRequest{Amount: 0, ExternalRef: "x"}, "amount 必须大于 0"},
		{"amount_negative", RechargeRequest{Amount: -1, ExternalRef: "x"}, "amount 必须大于 0"},
		{"amount_overflow", RechargeRequest{Amount: MaxRechargeAmount + 1, ExternalRef: "x"}, "amount 超出最大允许金额"},
		{"external_ref_empty", RechargeRequest{Amount: 100, ExternalRef: ""}, "external_ref 不能为空"},
		{"external_ref_whitespace", RechargeRequest{Amount: 100, ExternalRef: "  "}, "external_ref 不能为空"},
		{"external_ref_too_long", RechargeRequest{Amount: 100, ExternalRef: strings.Repeat("a", MaxIdempotencyKeyLen+1)}, "external_ref 长度超过上限"},
		{"reference_type_too_long", RechargeRequest{Amount: 100, ExternalRef: "x", ReferenceType: strings.Repeat("a", MaxRefLen+1)}, "reference_type 长度超过上限"},
		{"reference_id_too_long", RechargeRequest{Amount: 100, ExternalRef: "x", ReferenceID: strings.Repeat("a", MaxRefLen+1)}, "reference_id 长度超过上限"},
		{"ok", RechargeRequest{Amount: 100, ExternalRef: "x"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.req.validate()
			if tc.want == "" {
				assert.Equal(t, "", got)
			} else {
				assert.Contains(t, got, tc.want)
			}
		})
	}
}

func TestRefundRequest_ValidateBoundary(t *testing.T) {
	cases := []struct {
		name string
		req  RefundRequest
		want string
	}{
		{"amount_zero", RefundRequest{Amount: 0, CorrelationID: "x"}, "amount 必须大于 0"},
		{"amount_overflow", RefundRequest{Amount: MaxRechargeAmount + 1, CorrelationID: "x"}, "amount 超出最大允许金额"},
		{"correlation_empty", RefundRequest{Amount: 100, CorrelationID: ""}, "correlation_id 不能为空"},
		{"correlation_too_long", RefundRequest{Amount: 100, CorrelationID: strings.Repeat("a", MaxCorrelationIDLen+1)}, "correlation_id 长度超过上限"},
		{"reference_type_too_long", RefundRequest{Amount: 100, CorrelationID: "x", ReferenceType: strings.Repeat("a", MaxRefLen+1)}, "reference_type 长度超过上限"},
		{"reference_id_too_long", RefundRequest{Amount: 100, CorrelationID: "x", ReferenceID: strings.Repeat("a", MaxRefLen+1)}, "reference_id 长度超过上限"},
		{"ok", RefundRequest{Amount: 100, CorrelationID: "x"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.req.validate()
			if tc.want == "" {
				assert.Equal(t, "", got)
			} else {
				assert.Contains(t, got, tc.want)
			}
		})
	}
}

func TestNormalizeMetadata(t *testing.T) {
	assert.Nil(t, normalizeMetadata(nil))
	assert.Nil(t, normalizeMetadata([]byte{}))
	assert.Nil(t, normalizeMetadata([]byte(`{not json}`)))
	assert.Equal(t, []byte(`{"a":1}`), normalizeMetadata(json.RawMessage(`{"a":1}`)))
}

func TestNewHandler_PanicOnNilDeps(t *testing.T) {
	ledgerSvc := (*ledger.PostgresService)(nil)
	throttle := (*admintoken.PostgresThrottle)(nil)
	log := silentLogger()

	for _, tc := range []struct {
		name string
		fn   func()
	}{
		{"nil_ledger", func() { _ = NewHandler(nil, throttle, nil, log) }},
		{"nil_throttle", func() { _ = NewHandler(ledgerSvc, nil, nil, log) }},
		{"nil_log", func() { _ = NewHandler(ledgerSvc, throttle, nil, nil) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() { require.NotNil(t, recover(), "nil 依赖必须 panic") }()
			tc.fn()
		})
	}
}

func TestRefund_DailyRefundQuota_429(t *testing.T) {
	s := setupSuite(t)
	limit := int64(100)
	tok := s.createToken(t, func(p *admintoken.CreateParams) { p.DailyRefundQuotaLimit = &limit })
	r := s.buildEngine(t, tok)
	accID := "biz-refund-dailyq"
	s.cleanupAccount(t, accID)
	t.Cleanup(func() { s.cleanupAccount(t, accID) })

	require.Equal(t, http.StatusCreated,
		doRequest(r, http.MethodPost, "/admin/v1/business-accounts", map[string]any{"id": accID}).Code)
	require.Equal(t, http.StatusOK,
		doRequest(r, http.MethodPost, "/admin/v1/business-accounts/"+accID+"/recharge",
			map[string]any{"amount": 500, "external_ref": "tu"}).Code)
	actor := ledger.Actor{Type: ledger.ActorTypeAdminToken, ID: "test"}
	_, err := s.ledgerSvc.Reserve(context.Background(), actor, ledger.ReserveParams{
		AccountID: accID, Amount: 200, CorrelationID: "rsv",
	})
	require.NoError(t, err)
	_, err = s.ledgerSvc.Commit(context.Background(), actor, ledger.CommitParams{
		AccountID: accID, CorrelationID: "rsv", ActualCost: 200,
	})
	require.NoError(t, err)

	// 第一笔退款 60 通过
	require.Equal(t, http.StatusOK,
		doRequest(r, http.MethodPost, "/admin/v1/business-accounts/"+accID+"/refund",
			map[string]any{"amount": 60, "correlation_id": "rfd-1"}).Code)
	// 第二笔退款 50 应触发 daily quota
	w := doRequest(r, http.MethodPost, "/admin/v1/business-accounts/"+accID+"/refund",
		map[string]any{"amount": 50, "correlation_id": "rfd-2"})
	assert.Equal(t, http.StatusTooManyRequests, w.Code)
	assert.Equal(t, "daily_refund_quota_exceeded", parseError(t, w).Code)
}

func TestRefund_SingleRefundMax_429(t *testing.T) {
	s := setupSuite(t)
	max := int64(50)
	tok := s.createToken(t, func(p *admintoken.CreateParams) { p.SingleRefundMax = &max })
	r := s.buildEngine(t, tok)
	accID := "biz-refund-singlec"
	s.cleanupAccount(t, accID)
	t.Cleanup(func() { s.cleanupAccount(t, accID) })

	require.Equal(t, http.StatusCreated,
		doRequest(r, http.MethodPost, "/admin/v1/business-accounts", map[string]any{"id": accID}).Code)
	require.Equal(t, http.StatusOK,
		doRequest(r, http.MethodPost, "/admin/v1/business-accounts/"+accID+"/recharge",
			map[string]any{"amount": 500, "external_ref": "tu"}).Code)
	actor := ledger.Actor{Type: ledger.ActorTypeAdminToken, ID: "test"}
	_, err := s.ledgerSvc.Reserve(context.Background(), actor, ledger.ReserveParams{
		AccountID: accID, Amount: 200, CorrelationID: "rsv",
	})
	require.NoError(t, err)
	_, err = s.ledgerSvc.Commit(context.Background(), actor, ledger.CommitParams{
		AccountID: accID, CorrelationID: "rsv", ActualCost: 200,
	})
	require.NoError(t, err)

	w := doRequest(r, http.MethodPost, "/admin/v1/business-accounts/"+accID+"/refund",
		map[string]any{"amount": 100, "correlation_id": "rfd-x"})
	assert.Equal(t, http.StatusTooManyRequests, w.Code)
	assert.Equal(t, "single_refund_exceeded", parseError(t, w).Code)
}

func TestRefund_InvalidBody_400(t *testing.T) {
	s := setupSuite(t)
	tok := s.createToken(t, nil)
	r := s.buildEngine(t, tok)
	// 缺 correlation_id：gin binding required 拦截
	w := doRequest(r, http.MethodPost, "/admin/v1/business-accounts/x/refund",
		map[string]any{"amount": 100})
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "invalid_request_body", parseError(t, w).Code)
}

// =============================================================================
// 原有 audit 测试
// =============================================================================

func TestAudit_IdempotencyConflictTier1(t *testing.T) {
	s := setupSuite(t)
	tok := s.createToken(t, nil)
	r := s.buildEngine(t, tok)
	accID := "biz-audit-idemc"
	s.cleanupAccount(t, accID)
	t.Cleanup(func() { s.cleanupAccount(t, accID) })

	require.Equal(t, http.StatusCreated,
		doRequest(r, http.MethodPost, "/admin/v1/business-accounts", map[string]any{"id": accID}).Code)
	require.Equal(t, http.StatusOK,
		doRequest(r, http.MethodPost, "/admin/v1/business-accounts/"+accID+"/recharge",
			map[string]any{"amount": 100, "external_ref": "k1"}).Code)
	require.Equal(t, http.StatusConflict,
		doRequest(r, http.MethodPost, "/admin/v1/business-accounts/"+accID+"/recharge",
			map[string]any{"amount": 200, "external_ref": "k1"}).Code)

	recs := s.auditSink.snapshot()
	found := false
	for _, rec := range recs {
		if rec.OutcomeCode == "idempotency_conflict" {
			found = true
			assert.Equal(t, audit.Tier1, rec.Tier, "idempotency_conflict 必须 Tier1")
			break
		}
	}
	assert.True(t, found, "应至少一条 idempotency_conflict audit record")
}

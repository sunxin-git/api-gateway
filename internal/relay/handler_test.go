package relay

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
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sunxin-git/api-gateway/internal/businesskey"
	"github.com/sunxin-git/api-gateway/internal/db"
	"github.com/sunxin-git/api-gateway/internal/ledger"
)

// =============================================================================
// 测试基础设施
// =============================================================================

const defaultRelayTestDSN = "postgres://gateway:gateway_dev@127.0.0.1:55432/gateway?sslmode=disable"

func relayTestDSN() string {
	if v := os.Getenv("RELAY_TEST_PG_DSN"); v != "" {
		return v
	}
	return defaultRelayTestDSN
}

func silentLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// testOutbox 与 ledger/testutil.go 同构（避免循环依赖，本包独立）。
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

// relaySuite 测试套件：真 PG + 真 ledger + mock upstream + RelayHandler。
type relaySuite struct {
	pool          *pgxpool.Pool
	ledgerSvc     *ledger.PostgresService
	handler       *RelayHandler
	catalog       *EnvCatalog
	upstreamSrv   *httptest.Server
	upstreamHits  *atomic.Int64
	upstreamFunc  func(w http.ResponseWriter, r *http.Request)
	upstreamFuncM sync.Mutex
}

// setupRelaySuite 准备完整测试套件（pool + ledger + mock upstream + catalog + handler）。
func setupRelaySuite(t *testing.T) *relaySuite {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(relayTestDSN())
	require.NoError(t, err)
	cfg.MaxConns = 20
	cfg.MinConns = 2
	cfg.MaxConnLifetime = 30 * time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("跳过：无法连 PG: %v", err)
	}

	ledgerSvc := ledger.NewPostgresService(pool, &testOutbox{}, silentLog())

	// mock upstream server
	s := &relaySuite{
		pool:         pool,
		ledgerSvc:    ledgerSvc,
		upstreamHits: &atomic.Int64{},
	}
	s.upstreamSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.upstreamHits.Add(1)
		s.upstreamFuncM.Lock()
		fn := s.upstreamFunc
		s.upstreamFuncM.Unlock()
		if fn == nil {
			// 默认 200 + usage
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"chatcmpl-default","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`))
			return
		}
		fn(w, r)
	}))

	catalog, err := NewEnvCatalog(CatalogConfig{
		ModelName:             "gw-default",
		UpstreamProviderType:  "openai_compat",
		UpstreamBaseURL:       s.upstreamSrv.URL,
		UpstreamAPIKey:        "upstream-test-key",
		UpstreamModelName:     "doubao-mock",
		PriceInputPer1MMinor:  800,
		PriceOutputPer1MMinor: 2000,
		MaxContextTokens:      32768,
	})
	require.NoError(t, err)
	s.catalog = catalog

	adapter := NewOpenAICompatAdapter(&http.Client{Timeout: 5 * time.Second})
	s.handler = NewRelayHandler(catalog, adapter, ledgerSvc, nil, silentLog())
	s.handler.settleTO = 2 * time.Second // 测试加速

	t.Cleanup(func() {
		s.upstreamSrv.Close()
		pool.Close()
	})
	return s
}

func (s *relaySuite) setUpstream(fn func(w http.ResponseWriter, r *http.Request)) {
	s.upstreamFuncM.Lock()
	defer s.upstreamFuncM.Unlock()
	s.upstreamFunc = fn
}

// buildEngine 构造路由 + 注入 BusinessKey ValidationResult（绕过 BusinessKeyAuth）。
func (s *relaySuite) buildEngine(key *businesskey.Key) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("request_id", "test-rid-"+strings.ReplaceAll(time.Now().Format("150405.000000000"), ".", ""))
		c.Set("business_key_validation", &businesskey.ValidationResult{Key: key})
		c.Next()
	})
	r.POST("/v1/chat/completions", s.handler.ChatCompletion)
	return r
}

// createAccount + Recharge 让账户有余额。
func (s *relaySuite) seedAccount(t *testing.T, accountID string, balanceMinor int64) {
	t.Helper()
	ctx := context.Background()
	actor := ledger.Actor{Type: ledger.ActorTypeCLI, ID: "test"}
	_, err := s.ledgerSvc.CreateAccount(ctx, actor, ledger.CreateAccountParams{ID: accountID})
	require.NoError(t, err)
	if balanceMinor > 0 {
		_, _, err := s.ledgerSvc.Recharge(ctx, actor, ledger.RechargeParams{
			AccountID:      accountID,
			Amount:         balanceMinor,
			IdempotencyKey: accountID + ":seed",
			CanonicalBody:  &ledger.RechargeBody{AccountID: accountID, Amount: balanceMinor},
		})
		require.NoError(t, err)
	}
	t.Cleanup(func() { s.cleanupAccount(accountID) })
}

func (s *relaySuite) cleanupAccount(accountID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return
	}
	defer conn.Release()
	_, _ = conn.Exec(ctx, "SET session_replication_role = 'replica'")
	defer func() { _, _ = conn.Exec(context.Background(), "SET session_replication_role = 'origin'") }()
	for _, q := range []string{
		"DELETE FROM business_account_ledger WHERE business_account_id = $1",
		"DELETE FROM webhook_event_outbox WHERE business_account_id = $1",
		"DELETE FROM business_account_balance WHERE business_account_id = $1",
		"DELETE FROM business_account WHERE id = $1",
	} {
		_, _ = conn.Exec(ctx, q, accountID)
	}
}

// fakeKey 构造测试用 Key（与 businesskey 包 DB 模型同 struct）。
func (s *relaySuite) fakeKey(id int64, accountID string, rpm *int32) *businesskey.Key {
	return &businesskey.Key{ID: id, BusinessAccountID: accountID, RequestsPerMinute: rpm}
}

func uniqAccount(t *testing.T, suffix string) string {
	t.Helper()
	return "relay-" + strings.ReplaceAll(t.Name(), "/", "_") + "-" + suffix + "-" + time.Now().Format("150405.000")
}

func postReq(body any) *http.Request {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func parseErr(t *testing.T, w *httptest.ResponseRecorder) (errType, code string) {
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	if errMap, ok := resp["error"].(map[string]any); ok {
		errType, _ = errMap["type"].(string)
		code, _ = errMap["code"].(string)
	}
	return
}

func (s *relaySuite) getBalance(t *testing.T, accountID string) *ledger.Balance {
	t.Helper()
	bal, err := s.ledgerSvc.GetBalance(context.Background(), accountID)
	require.NoError(t, err)
	return bal
}

// =============================================================================
// Happy path
// =============================================================================

func TestRelay_HappyPath_Reserve_Commit_Settle(t *testing.T) {
	s := setupRelaySuite(t)
	accID := uniqAccount(t, "happy")
	s.seedAccount(t, accID, 1_000_000) // 1 元

	key := s.fakeKey(1, accID, nil)
	r := s.buildEngine(key)

	s.setUpstream(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":50,"completion_tokens":100,"total_tokens":150}}`))
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, postReq(map[string]any{
		"model":      "gw-default",
		"messages":   []any{map[string]any{"role": "user", "content": "hello"}},
		"max_tokens": 200,
	}))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "chatcmpl-1")
	assert.Contains(t, w.Body.String(), `"prompt_tokens":50`)

	bal := s.getBalance(t, accID)
	// 50*800 + 100*2000 = 40k+200k = 240k → ceil(240k/1M) = 1 minor
	assert.Equal(t, int64(1), bal.UsedTotal)
	assert.Equal(t, int64(1_000_000-1), bal.Available)
	assert.Equal(t, int64(0), bal.Reserved, "Commit 后多余 reserve 已 release")
}

// =============================================================================
// 入参校验
// =============================================================================

func TestRelay_InvalidBody_NotJSON(t *testing.T) {
	s := setupRelaySuite(t)
	key := s.fakeKey(1, "any", nil)
	r := s.buildEngine(key)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	et, code := parseErr(t, w)
	assert.Equal(t, ErrTypeInvalidRequest, et)
	assert.Equal(t, "invalid_request_body", code)
}

func TestRelay_MissingModel(t *testing.T) {
	s := setupRelaySuite(t)
	key := s.fakeKey(1, "any", nil)
	r := s.buildEngine(key)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, postReq(map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "x"}},
	}))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	_, code := parseErr(t, w)
	assert.Equal(t, "missing_model", code)
}

func TestRelay_StreamTrue_Rejected(t *testing.T) {
	s := setupRelaySuite(t)
	key := s.fakeKey(1, "any", nil)
	r := s.buildEngine(key)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, postReq(map[string]any{
		"model":    "gw-default",
		"stream":   true,
		"messages": []any{map[string]any{"role": "user", "content": "x"}},
	}))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	_, code := parseErr(t, w)
	assert.Equal(t, "streaming_not_supported", code)
}

func TestRelay_EmptyMessages(t *testing.T) {
	s := setupRelaySuite(t)
	key := s.fakeKey(1, "any", nil)
	r := s.buildEngine(key)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, postReq(map[string]any{
		"model":    "gw-default",
		"messages": []any{},
	}))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	_, code := parseErr(t, w)
	assert.Equal(t, "empty_messages", code)
}

func TestRelay_MaxTokensExceedsContext(t *testing.T) {
	s := setupRelaySuite(t)
	key := s.fakeKey(1, "any", nil)
	r := s.buildEngine(key)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, postReq(map[string]any{
		"model":      "gw-default",
		"messages":   []any{map[string]any{"role": "user", "content": "x"}},
		"max_tokens": 50000, // > MaxContextTokens=32768
	}))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	_, code := parseErr(t, w)
	assert.Equal(t, "max_tokens_exceeds_context", code)
}

// =============================================================================
// Reserve 错误
// =============================================================================

func TestRelay_InsufficientBalance_402(t *testing.T) {
	s := setupRelaySuite(t)
	accID := uniqAccount(t, "no-money")
	s.seedAccount(t, accID, 0) // 余额为 0

	key := s.fakeKey(1, accID, nil)
	r := s.buildEngine(key)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, postReq(map[string]any{
		"model":      "gw-default",
		"messages":   []any{map[string]any{"role": "user", "content": "x"}},
		"max_tokens": 100,
	}))
	assert.Equal(t, http.StatusPaymentRequired, w.Code)
	et, code := parseErr(t, w)
	assert.Equal(t, ErrTypeInsufficientQuota, et)
	assert.Equal(t, "insufficient_quota", code)
}

func TestRelay_AccountNotFound_401(t *testing.T) {
	s := setupRelaySuite(t)
	key := s.fakeKey(1, "non-existent-account", nil) // 账户从未 seed
	r := s.buildEngine(key)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, postReq(map[string]any{
		"model":      "gw-default",
		"messages":   []any{map[string]any{"role": "user", "content": "x"}},
		"max_tokens": 100,
	}))
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	et, code := parseErr(t, w)
	assert.Equal(t, ErrTypeInvalidAPIKey, et)
	assert.Equal(t, "account_not_found", code)
}

// =============================================================================
// 上游错误
// =============================================================================

func TestRelay_Upstream4xx_PassthroughAndReleaseReserve(t *testing.T) {
	s := setupRelaySuite(t)
	accID := uniqAccount(t, "u4xx")
	s.seedAccount(t, accID, 1_000_000)

	key := s.fakeKey(1, accID, nil)
	r := s.buildEngine(key)

	s.setUpstream(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid model"}}`))
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, postReq(map[string]any{
		"model":      "gw-default",
		"messages":   []any{map[string]any{"role": "user", "content": "x"}},
		"max_tokens": 100,
	}))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "invalid model")

	// reserve 必须被 release：available 恢复，used_total 不变
	bal := s.getBalance(t, accID)
	assert.Equal(t, int64(0), bal.UsedTotal)
	assert.Equal(t, int64(1_000_000), bal.Available)
	assert.Equal(t, int64(0), bal.Reserved)
}

func TestRelay_Upstream5xx_502AndReleaseReserve(t *testing.T) {
	s := setupRelaySuite(t)
	accID := uniqAccount(t, "u5xx")
	s.seedAccount(t, accID, 1_000_000)

	key := s.fakeKey(1, accID, nil)
	r := s.buildEngine(key)

	s.setUpstream(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"internal"}}`))
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, postReq(map[string]any{
		"model":      "gw-default",
		"messages":   []any{map[string]any{"role": "user", "content": "x"}},
		"max_tokens": 100,
	}))
	assert.Equal(t, http.StatusBadGateway, w.Code) // 5xx → 502
	_, code := parseErr(t, w)
	assert.Equal(t, "upstream_5xx", code)

	bal := s.getBalance(t, accID)
	assert.Equal(t, int64(0), bal.Reserved)
	assert.Equal(t, int64(1_000_000), bal.Available)
}

func TestRelay_UpstreamTimeout_504(t *testing.T) {
	s := setupRelaySuite(t)
	accID := uniqAccount(t, "timeout")
	s.seedAccount(t, accID, 1_000_000)

	key := s.fakeKey(1, accID, nil)
	r := s.buildEngine(key)

	// 上游 sleep 远超 client timeout
	s.setUpstream(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(8 * time.Second)
		w.WriteHeader(http.StatusOK)
	})

	// 覆盖 handler 用更短 client timeout
	s.handler.adapter = NewOpenAICompatAdapter(&http.Client{Timeout: 200 * time.Millisecond})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, postReq(map[string]any{
		"model":      "gw-default",
		"messages":   []any{map[string]any{"role": "user", "content": "x"}},
		"max_tokens": 100,
	}))
	assert.Equal(t, http.StatusGatewayTimeout, w.Code)
	et, code := parseErr(t, w)
	assert.Equal(t, ErrTypeUpstreamTimeout, et)
	assert.Equal(t, "upstream_timeout", code)

	// reserve 必须被 release
	bal := s.getBalance(t, accID)
	assert.Equal(t, int64(0), bal.Reserved)
}

// =============================================================================
// 上游 200 但缺 usage 兜底
// =============================================================================

func TestRelay_Upstream200_MissingUsage_CommitReserveFallback(t *testing.T) {
	s := setupRelaySuite(t)
	accID := uniqAccount(t, "no-usage")
	s.seedAccount(t, accID, 1_000_000)

	key := s.fakeKey(1, accID, nil)
	r := s.buildEngine(key)

	s.setUpstream(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		// 上游 200 但 body 不含 usage 字段
		_, _ = w.Write([]byte(`{"id":"chatcmpl-no-usage","choices":[]}`))
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, postReq(map[string]any{
		"model":      "gw-default",
		"messages":   []any{map[string]any{"role": "user", "content": "x"}},
		"max_tokens": 100,
	}))
	// 兜底：业务仍 200，透传上游 body
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "chatcmpl-no-usage")

	// 兜底 commit reserve 全额；used_total = reserve amount
	bal := s.getBalance(t, accID)
	// reserve = ceil((input_est × 800 + 100 × 2000) / 1M)；input_est 来自 messages JSON / 4
	// 至少 200_000 / 1M = 1 minor （max_tokens=100 × 2000 = 200_000）
	assert.GreaterOrEqual(t, bal.UsedTotal, int64(1))
	assert.Equal(t, int64(0), bal.Reserved)
}

// =============================================================================
// 透传契约
// =============================================================================

func TestRelay_PassthroughResponseBody(t *testing.T) {
	s := setupRelaySuite(t)
	accID := uniqAccount(t, "passthrough")
	s.seedAccount(t, accID, 1_000_000)

	key := s.fakeKey(1, accID, nil)
	r := s.buildEngine(key)

	upstreamBody := `{"id":"chatcmpl-pt","model":"doubao-mock","object":"chat.completion","created":1234567890,"choices":[{"index":0,"message":{"role":"assistant","content":"response"}}],"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30},"system_fingerprint":"fp_xxx"}`
	s.setUpstream(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(upstreamBody))
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, postReq(map[string]any{
		"model":      "gw-default",
		"messages":   []any{map[string]any{"role": "user", "content": "x"}},
		"max_tokens": 100,
	}))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, upstreamBody, w.Body.String(), "body 必须字节级透传")
	assert.Contains(t, w.Header().Get("Content-Type"), "application/json")
}

func TestRelay_PassthroughBusinessRequestFields(t *testing.T) {
	s := setupRelaySuite(t)
	accID := uniqAccount(t, "bizfields")
	s.seedAccount(t, accID, 1_000_000)

	key := s.fakeKey(1, accID, nil)
	r := s.buildEngine(key)

	var seenBody map[string]any
	s.setUpstream(func(w http.ResponseWriter, req *http.Request) {
		_ = json.NewDecoder(req.Body).Decode(&seenBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	})

	bizReq := map[string]any{
		"model":           "gw-default",
		"messages":        []any{map[string]any{"role": "user", "content": "x"}},
		"max_tokens":      100,
		"temperature":     0.7,
		"top_p":           0.9,
		"tools":           []any{map[string]any{"type": "function", "function": map[string]any{"name": "f"}}},
		"response_format": map[string]any{"type": "json_object"},
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, postReq(bizReq))
	require.Equal(t, http.StatusOK, w.Code)

	// 关键透传断言：model 改写为 upstream，其他字段保留
	assert.Equal(t, "doubao-mock", seenBody["model"])
	assert.Equal(t, float64(0.7), seenBody["temperature"])
	assert.Equal(t, float64(0.9), seenBody["top_p"])
	assert.NotNil(t, seenBody["tools"])
	assert.NotNil(t, seenBody["response_format"])
}

// =============================================================================
// 并发 Reserve / Settle
// =============================================================================

func TestRelay_ConcurrentRequests_DifferentAccounts(t *testing.T) {
	s := setupRelaySuite(t)
	const N = 30
	accIDs := make([]string, N)
	for i := 0; i < N; i++ {
		accIDs[i] = uniqAccount(t, fmt.Sprintf("conc-%d", i))
		s.seedAccount(t, accIDs[i], 1_000_000)
	}

	s.setUpstream(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`))
	})

	var wg sync.WaitGroup
	wg.Add(N)
	results := make([]int, N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			key := s.fakeKey(int64(i+100), accIDs[i], nil)
			r := s.buildEngine(key)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, postReq(map[string]any{
				"model":      "gw-default",
				"messages":   []any{map[string]any{"role": "user", "content": "x"}},
				"max_tokens": 100,
			}))
			results[i] = w.Code
		}()
	}
	wg.Wait()

	count := 0
	for _, code := range results {
		if code == http.StatusOK {
			count++
		}
	}
	require.Equal(t, N, count, "%d 并发独立账户 relay 全成功", N)
}

// =============================================================================
// 上游连接错误
// =============================================================================

func TestRelay_UpstreamConnectionRefused_502(t *testing.T) {
	s := setupRelaySuite(t)
	accID := uniqAccount(t, "unreachable")
	s.seedAccount(t, accID, 1_000_000)

	// 改 catalog 指向已关闭端口
	closedCatalog, err := NewEnvCatalog(CatalogConfig{
		ModelName:             "gw-default",
		UpstreamProviderType:  "openai_compat",
		UpstreamBaseURL:       "http://127.0.0.1:1", // 几乎肯定关闭
		UpstreamAPIKey:        "x",
		UpstreamModelName:     "m",
		PriceInputPer1MMinor:  1,
		PriceOutputPer1MMinor: 1,
		MaxContextTokens:      100,
	})
	require.NoError(t, err)
	s.handler = NewRelayHandler(closedCatalog, NewOpenAICompatAdapter(&http.Client{Timeout: 2 * time.Second}), s.ledgerSvc, nil, silentLog())

	key := s.fakeKey(1, accID, nil)
	r := s.buildEngine(key)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, postReq(map[string]any{
		"model":      "gw-default",
		"messages":   []any{map[string]any{"role": "user", "content": "x"}},
		"max_tokens": 50,
	}))
	assert.Equal(t, http.StatusBadGateway, w.Code)
	_, code := parseErr(t, w)
	assert.Equal(t, "upstream_unreachable", code)

	// reserve 必须 release
	bal := s.getBalance(t, accID)
	assert.Equal(t, int64(0), bal.Reserved)
}

// =============================================================================
// Constructor panic 路径
// =============================================================================

func TestNewRelayHandler_PanicOnNilDeps(t *testing.T) {
	cases := []struct {
		name string
		fn   func()
	}{
		{"nil_catalog", func() { _ = NewRelayHandler(nil, NewOpenAICompatAdapter(&http.Client{}), nil, nil, silentLog()) }},
		{"nil_adapter", func() {
			_ = NewRelayHandler(&EnvCatalog{}, nil, nil, nil, silentLog())
		}},
		{"nil_log", func() {
			_ = NewRelayHandler(&EnvCatalog{}, NewOpenAICompatAdapter(&http.Client{}), nil, nil, nil)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() { require.NotNil(t, recover()) }()
			tc.fn()
		})
	}
}

// =============================================================================
// retry helper unit test
// =============================================================================

func TestRetryOnCASConflict_RetriesAndSucceeds(t *testing.T) {
	attempts := 0
	err := retryOnCASConflict(context.Background(),
		[]time.Duration{10 * time.Millisecond, 10 * time.Millisecond, 10 * time.Millisecond},
		func(_ context.Context) error {
			attempts++
			if attempts < 3 {
				return ledger.ErrVersionConflict
			}
			return nil
		},
	)
	require.NoError(t, err)
	assert.Equal(t, 3, attempts)
}

func TestRetryOnCASConflict_NonCASErrorReturnedImmediately(t *testing.T) {
	attempts := 0
	other := errors.New("db down")
	err := retryOnCASConflict(context.Background(),
		[]time.Duration{10 * time.Millisecond, 10 * time.Millisecond},
		func(_ context.Context) error {
			attempts++
			return other
		},
	)
	require.ErrorIs(t, err, other)
	assert.Equal(t, 1, attempts, "非 CAS 错误不应重试")
}

func TestRetryOnCASConflict_ExhaustedReturnsLastErr(t *testing.T) {
	attempts := 0
	err := retryOnCASConflict(context.Background(),
		[]time.Duration{1 * time.Millisecond, 1 * time.Millisecond},
		func(_ context.Context) error {
			attempts++
			return ledger.ErrVersionConflict
		},
	)
	require.ErrorIs(t, err, ledger.ErrVersionConflict)
	assert.Equal(t, 3, attempts, "3 次都返 CAS 后退出")
}

func TestRetryOnCASConflict_CtxCanceledStopsRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := retryOnCASConflict(ctx,
		[]time.Duration{50 * time.Millisecond, 50 * time.Millisecond},
		func(_ context.Context) error {
			return ledger.ErrVersionConflict
		},
	)
	require.Error(t, err)
}

func TestStatusLabel(t *testing.T) {
	assert.Equal(t, "200", statusLabel(200))
	assert.Equal(t, "4xx", statusLabel(404))
	assert.Equal(t, "4xx", statusLabel(429))
	assert.Equal(t, "5xx", statusLabel(503))
	assert.Equal(t, "100", statusLabel(100))
}

func TestReadMaxTokens(t *testing.T) {
	def := int32(32768)
	assert.Equal(t, int64(def), readMaxTokens(nil, def))
	assert.Equal(t, int64(def), readMaxTokens(map[string]any{}, def))
	assert.Equal(t, int64(100), readMaxTokens(map[string]any{"max_tokens": 100}, def))
	assert.Equal(t, int64(100), readMaxTokens(map[string]any{"max_tokens": float64(100)}, def))
	assert.Equal(t, int64(def), readMaxTokens(map[string]any{"max_tokens": "not-int"}, def))
}

// =============================================================================
// 内部 helper unit tests（补覆盖率）
// =============================================================================

func TestClassifySettleErr(t *testing.T) {
	assert.Equal(t, "version_conflict", classifySettleErr(ledger.ErrVersionConflict))
	assert.Equal(t, "version_conflict", classifySettleErr(fmt.Errorf("wrap: %w", ledger.ErrVersionConflict)))
	assert.Equal(t, "timeout", classifySettleErr(context.DeadlineExceeded))
	assert.Equal(t, "internal", classifySettleErr(errors.New("random")))
}

func TestAuditMetaGettersDefaults(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/x", func(c *gin.Context) {
		// 未 Set 时所有 getter 返零值
		assert.Equal(t, "", GetBusinessAuditOutcomeCode(c))
		assert.Equal(t, 0, GetBusinessAuditInputTokens(c))
		assert.Equal(t, 0, GetBusinessAuditOutputTokens(c))
		assert.Equal(t, int64(0), GetBusinessAuditCostMinor(c))
		assert.Equal(t, "", GetBusinessAuditGatewayModel(c))
		assert.Equal(t, "", GetBusinessAuditUpstreamModel(c))
		assert.Equal(t, 0, GetBusinessAuditUpstreamStatus(c))
		assert.Equal(t, int64(0), GetBusinessAuditUpstreamDurationMs(c))
		c.Status(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
}

func TestAuditMetaGettersAfterSet(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/x", func(c *gin.Context) {
		SetBusinessAuditOutcomeCode(c, "ok")
		SetBusinessAuditTokens(c, 10, 20)
		SetBusinessAuditCost(c, 480)
		SetBusinessAuditModelInfo(c, "gw", "up")
		SetBusinessAuditUpstreamResult(c, 200, 1500*time.Millisecond)
		assert.Equal(t, "ok", GetBusinessAuditOutcomeCode(c))
		assert.Equal(t, 10, GetBusinessAuditInputTokens(c))
		assert.Equal(t, 20, GetBusinessAuditOutputTokens(c))
		assert.Equal(t, int64(480), GetBusinessAuditCostMinor(c))
		assert.Equal(t, "gw", GetBusinessAuditGatewayModel(c))
		assert.Equal(t, "up", GetBusinessAuditUpstreamModel(c))
		assert.Equal(t, 200, GetBusinessAuditUpstreamStatus(c))
		assert.Equal(t, int64(1500), GetBusinessAuditUpstreamDurationMs(c))
		c.Status(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
}

func TestRelay_MissingKeyContext_500(t *testing.T) {
	s := setupRelaySuite(t)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		// 故意不注入 business_key_validation
		c.Set("request_id", "test")
		c.Next()
	})
	r.POST("/v1/chat/completions", s.handler.ChatCompletion)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, postReq(map[string]any{
		"model":    "gw-default",
		"messages": []any{map[string]any{"role": "user", "content": "x"}},
	}))
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	et, _ := parseErr(t, w)
	assert.Equal(t, ErrTypeAPIError, et)
}

func TestRelay_UpstreamMalformed_502(t *testing.T) {
	s := setupRelaySuite(t)
	accID := uniqAccount(t, "malformed")
	s.seedAccount(t, accID, 1_000_000)

	key := s.fakeKey(1, accID, nil)
	r := s.buildEngine(key)

	s.setUpstream(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<html>not json</html>`)) // 200 + 非 JSON
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, postReq(map[string]any{
		"model":      "gw-default",
		"messages":   []any{map[string]any{"role": "user", "content": "x"}},
		"max_tokens": 100,
	}))
	assert.Equal(t, http.StatusBadGateway, w.Code)
	_, code := parseErr(t, w)
	assert.Equal(t, "upstream_malformed", code)

	bal := s.getBalance(t, accID)
	assert.Equal(t, int64(0), bal.Reserved, "Reserve 应被 release")
}

func TestRelay_AccountFrozen_402(t *testing.T) {
	s := setupRelaySuite(t)
	accID := uniqAccount(t, "frozen")
	s.seedAccount(t, accID, 1_000_000)

	// 手工 freeze
	ctx := context.Background()
	require.NoError(t, s.ledgerSvc.Freeze(ctx, ledger.Actor{Type: ledger.ActorTypeCLI, ID: "test"}, accID, ledger.ReasonCodeManualFreeze))

	key := s.fakeKey(1, accID, nil)
	r := s.buildEngine(key)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, postReq(map[string]any{
		"model":      "gw-default",
		"messages":   []any{map[string]any{"role": "user", "content": "x"}},
		"max_tokens": 100,
	}))
	assert.Equal(t, http.StatusPaymentRequired, w.Code)
	_, code := parseErr(t, w)
	assert.Equal(t, "account_frozen", code)
}

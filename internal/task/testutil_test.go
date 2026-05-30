package task

import (
	"context"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/sunxin-git/api-gateway/internal/channel"
	"github.com/sunxin-git/api-gateway/internal/db"
	"github.com/sunxin-git/api-gateway/internal/ledger"
	"github.com/sunxin-git/api-gateway/internal/outbox"
	"github.com/sunxin-git/api-gateway/internal/relay/video"
)

// 测试 PG DSN：优先 TASK_TEST_PG_DSN，回落 LEDGER_TEST_PG_DSN（用户持久化指向 5432），再回落默认。
const defaultTaskTestDSN = "postgres://gateway:gateway_dev@127.0.0.1:55432/gateway?sslmode=disable"

func taskTestDSN() string {
	if v := os.Getenv("TASK_TEST_PG_DSN"); v != "" {
		return v
	}
	if v := os.Getenv("LEDGER_TEST_PG_DSN"); v != "" {
		return v
	}
	return defaultTaskTestDSN
}

func silentLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// ---------- mock / fake 依赖 ----------

type mockAdapter struct {
	submitFn func(ctx context.Context, entry *video.VideoModelEntry, creds video.UpstreamCredentials, req *video.ValidatedRequest, cb string) (string, error)
	pollFn   func(ctx context.Context, entry *video.VideoModelEntry, creds video.UpstreamCredentials, upstreamTaskID string) (*video.PollResult, error)
	submits  int64
	mu       sync.Mutex
}

func (m *mockAdapter) Submit(ctx context.Context, entry *video.VideoModelEntry, creds video.UpstreamCredentials, req *video.ValidatedRequest, cb string) (string, error) {
	m.mu.Lock()
	m.submits++
	m.mu.Unlock()
	return m.submitFn(ctx, entry, creds, req, cb)
}

func (m *mockAdapter) Poll(ctx context.Context, entry *video.VideoModelEntry, creds video.UpstreamCredentials, upstreamTaskID string) (*video.PollResult, error) {
	return m.pollFn(ctx, entry, creds, upstreamTaskID)
}

func (m *mockAdapter) submitCount() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.submits
}

type fakeEnqueuer struct {
	mu      sync.Mutex
	submits []string
	settles []string
}

func (f *fakeEnqueuer) EnqueueSubmit(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.submits = append(f.submits, id)
	return nil
}

func (f *fakeEnqueuer) EnqueueSettle(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settles = append(f.settles, id)
	return nil
}

func (f *fakeEnqueuer) settleCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.settles)
}

type fakeCreds struct{ apiKey string }

func (f fakeCreds) GetCredentialsForUpstream(_ context.Context, _ int64) (*channel.ChannelCredentials, error) {
	return &channel.ChannelCredentials{APIKey: f.apiKey}, nil
}

// ---------- 测试套件 ----------

type taskSuite struct {
	pool      *pgxpool.Pool
	q         *db.Queries
	ledgerSvc *ledger.PostgresService
	svc       *Service
	adapter   *mockAdapter
	enq       *fakeEnqueuer
	catalog   *video.EnvVideoCatalog
	channelID int64
	accountID string
}

func setupTaskSuite(t *testing.T) *taskSuite {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(taskTestDSN())
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

	catalog, err := video.NewEnvVideoCatalog(video.CatalogConfig{
		GatewayModelName:       "gw-video",
		UpstreamProviderType:   video.ProviderTypeVolcSeedance,
		UpstreamBaseURL:        "https://ark.example/api/v3",
		UpstreamModelName:      "doubao-seedance-mock",
		ChannelName:            "test-ch",
		Price720pPer1MMinor:    6000,    // 便于算钱：6000 minor / 百万 token
		BillingMultiplierBP:    bpScale, // 1.0× 简化断言
		DurationMinSeconds:     4,
		DurationMaxSeconds:     15,
		DurationDefaultSeconds: 5,
		FpsDefault:             24,
		FpsMax:                 30,
		Ratios:                 []string{"16:9", "1:1"},
		RatioDefault:           "16:9",
		ResolutionDefault:      "720p",
	})
	require.NoError(t, err)

	adapter := &mockAdapter{
		submitFn: func(_ context.Context, _ *video.VideoModelEntry, _ video.UpstreamCredentials, _ *video.ValidatedRequest, _ string) (string, error) {
			return "cgt-default", nil
		},
		pollFn: func(_ context.Context, _ *video.VideoModelEntry, _ video.UpstreamCredentials, _ string) (*video.PollResult, error) {
			return &video.PollResult{Status: video.UpstreamSucceeded, Usage: &video.UpstreamUsage{CompletionTokens: 100_000}}, nil
		},
	}
	enq := &fakeEnqueuer{}
	ledgerSvc := ledger.NewPostgresService(pool, outbox.NewPostgresPublisher(), silentLog())

	svc, err := NewService(Config{
		Pool:           pool,
		Ledger:         ledgerSvc,
		Adapter:        adapter,
		Catalog:        catalog,
		Creds:          fakeCreds{apiKey: "test-key"},
		Enqueuer:       enq,
		Logger:         silentLog(),
		ConcurrencyCap: 5,
		SettleTimeout:  2 * time.Second,
		PollTimeout:    2 * time.Second,
		WorkerID:       "test-worker",
	})
	require.NoError(t, err)

	s := &taskSuite{
		pool:      pool,
		q:         db.New(pool),
		ledgerSvc: ledgerSvc,
		svc:       svc,
		adapter:   adapter,
		enq:       enq,
		catalog:   catalog,
		accountID: "task-test-" + newTaskID(),
	}

	// channel 行（满足 task.channel_id FK + 提供 creds 来源；creds 由 fake 返回，密文仅占位）。
	var chID int64
	err = pool.QueryRow(ctx,
		`INSERT INTO channel (name, provider_type, credentials_encrypted) VALUES ($1, 'volc_seedance', '\x00'::bytea) RETURNING id`,
		"test-ch-"+s.accountID,
	).Scan(&chID)
	require.NoError(t, err)
	s.channelID = chID

	t.Cleanup(func() {
		s.cleanup()
		pool.Close()
	})
	return s
}

// seedAccount 建账户 + 充值。
func (s *taskSuite) seedAccount(t *testing.T, balanceMinor int64) {
	t.Helper()
	ctx := context.Background()
	actor := ledger.Actor{Type: ledger.ActorTypeCLI, ID: "test"}
	_, err := s.ledgerSvc.CreateAccount(ctx, actor, ledger.CreateAccountParams{ID: s.accountID})
	require.NoError(t, err)
	if balanceMinor > 0 {
		_, _, err := s.ledgerSvc.Recharge(ctx, actor, ledger.RechargeParams{
			AccountID:      s.accountID,
			Amount:         balanceMinor,
			IdempotencyKey: s.accountID + ":seed",
			CanonicalBody:  &ledger.RechargeBody{AccountID: s.accountID, Amount: balanceMinor},
		})
		require.NoError(t, err)
	}
}

// submitParams 构造一次提交（720p / reserve=1000）。
func (s *taskSuite) submitParams(reserveMinor int64) SubmitParams {
	entry := s.catalog.DefaultEntry()
	chID := s.channelID
	return SubmitParams{
		BusinessAccountID: s.accountID,
		ChannelID:         &chID,
		Entry:             entry,
		Request: &video.ValidatedRequest{
			TaskType: video.TaskTypeTextToVideo, Prompt: "a dog", Duration: 5,
			Resolution: "720p", Ratio: "16:9", Fps: 24,
		},
		ReserveMinor:  reserveMinor,
		ReserveTokens: 1_000_000_000, // 高上界，避免 settle 溢出 cap 误触
		MinTokenFloor: 0,
	}
}

func (s *taskSuite) balance(t *testing.T) *ledger.Balance {
	t.Helper()
	b, err := s.ledgerSvc.GetBalance(context.Background(), s.accountID)
	require.NoError(t, err)
	return b
}

func (s *taskSuite) getTask(t *testing.T, id string) db.Task {
	t.Helper()
	tk, err := s.q.GetTaskByID(context.Background(), id)
	require.NoError(t, err)
	return tk
}

func (s *taskSuite) inflight(t *testing.T) int32 {
	t.Helper()
	row, err := s.q.GetConcurrency(context.Background(), db.GetConcurrencyParams{
		BusinessAccountID: s.accountID, Model: "gw-video",
	})
	if err != nil {
		return 0 // 行不存在 = 0
	}
	return row.Inflight
}

// directSetStatus 测试用：直接 UPDATE task 状态 + upstream_task_id（绕过 CAS 摆精确前置态）。
func (s *taskSuite) directSetStatus(t *testing.T, taskID string, status db.TaskStatus, upstreamTaskID string) {
	t.Helper()
	_, err := s.pool.Exec(context.Background(),
		`UPDATE task SET status = $1, upstream_task_id = $2, updated_at = NOW() WHERE id = $3`,
		status, upstreamTaskID, taskID)
	require.NoError(t, err)
}

func (s *taskSuite) cleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return
	}
	defer conn.Release()
	_, _ = conn.Exec(ctx, "SET session_replication_role = 'replica'")
	defer func() { _, _ = conn.Exec(context.Background(), "SET session_replication_role = 'origin'") }()
	_, _ = conn.Exec(ctx, "DELETE FROM task WHERE business_account_id = $1", s.accountID)
	_, _ = conn.Exec(ctx, "DELETE FROM account_model_concurrency WHERE business_account_id = $1", s.accountID)
	_, _ = conn.Exec(ctx, "DELETE FROM business_account_ledger WHERE business_account_id = $1", s.accountID)
	_, _ = conn.Exec(ctx, "DELETE FROM webhook_event_outbox WHERE business_account_id = $1", s.accountID)
	_, _ = conn.Exec(ctx, "DELETE FROM business_account_balance WHERE business_account_id = $1", s.accountID)
	_, _ = conn.Exec(ctx, "DELETE FROM business_account WHERE id = $1", s.accountID)
	_, _ = conn.Exec(ctx, "DELETE FROM channel WHERE id = $1", s.channelID)
}

// assertInvariant available + reserved + used_total = recharge_total。
func assertInvariant(t *testing.T, b *ledger.Balance) {
	t.Helper()
	require.GreaterOrEqual(t, b.Available, int64(0))
	require.GreaterOrEqual(t, b.Reserved, int64(0))
	require.GreaterOrEqual(t, b.UsedTotal, int64(0))
	require.Equal(t, b.RechargeTotal, b.Available+b.Reserved+b.UsedTotal,
		"账本不变量违反: %+v", b)
}

package businesskey

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// =============================================================================
// 测试用 PG 连接 helpers
//
// 与 internal/admintoken/testutil.go 同风格；business key 测试间靠
// "BusinessAccountID 前缀按 t.Name() 隔离" + "Description 前缀按 t.Name()"。
// =============================================================================

const defaultBusinessKeyTestDSN = "postgres://gateway:gateway_dev@127.0.0.1:55432/gateway?sslmode=disable"

func testPGDSN() string {
	if v := os.Getenv("BUSINESSKEY_TEST_PG_DSN"); v != "" {
		return v
	}
	return defaultBusinessKeyTestDSN
}

// testPepper 测试用 32 字节 pepper；固定值便于跨测试一致性。
// 与 admintoken testPepper **不同**值（验证 pepper 复用机制时各包测试独立）。
var testPepper = []byte("businesskey-test-pepper-32bytes!Y")

// mustOpenTestPool 建立 pgxpool 连接；失败 t.Skip（与 admintoken 一致）。
func mustOpenTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := testPGDSN()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	// MaxConns=20：本包并发测试 ≤ 50 goroutine；与 admin / admintoken / ledger 并行跑时
	// 各取 20-30 conn，总和 < PG 默认 max_connections=100
	cfg.MaxConns = 20
	cfg.MinConns = 2
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnIdleTime = 5 * time.Minute

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pgxpool.NewWithConfig: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("跳过：无法连 PG (%s)：%v", dsn, err)
	}
	return pool
}

// newTestService 构造测试用 PostgresService（固定 pepper + 静默 logger）。
// **不**启动异步 flush goroutine（避免与测试主线 race）；
// 测试需 flush 时显式调 s.flushOnce(ctx)。
func newTestService(t *testing.T, pool *pgxpool.Pool) *PostgresService {
	t.Helper()
	return newPostgresServiceBase(pool, testPepper, newSilentLogger())
}

func newSilentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// setupSuite 准备 pool + service；t.Cleanup 自动 Close pool + Close service（防 goroutine 泄漏）。
func setupSuite(t *testing.T) (*pgxpool.Pool, *PostgresService) {
	t.Helper()
	pool := mustOpenTestPool(t)
	svc := newTestService(t, pool)
	t.Cleanup(func() {
		_ = svc.Close()
		pool.Close()
	})
	return pool, svc
}

// =============================================================================
// 测试隔离辅助
// =============================================================================

// createTestAccount 创建测试用 business_account 行（business_account_api_key 的 FK 目标）；
// t.Cleanup 自动清理 + 级联删除关联 key。
//
// 用 business_account_id = uniqAccountID(t, "suffix") 保证测试间隔离。
func createTestAccount(t *testing.T, pool *pgxpool.Pool, accountID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx,
		`INSERT INTO business_account (id, status) VALUES ($1, 'active')
		 ON CONFLICT (id) DO NOTHING`,
		accountID); err != nil {
		t.Fatalf("createTestAccount %s: %v", accountID, err)
	}
	t.Cleanup(func() { cleanupTestAccount(t, pool, accountID) })
}

// cleanupTestAccount 删除测试账户 + 级联删除关联 key（FK CASCADE）+ balance 等。
//
// 借 session_replication_role='replica' 临时绕过 ledger 不可变 trigger。
func cleanupTestAccount(t *testing.T, pool *pgxpool.Pool, accountID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Logf("cleanup acquire: %v", err)
		return
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SET session_replication_role = 'replica'"); err != nil {
		t.Logf("cleanup set replication_role: %v", err)
		return
	}
	defer func() { _, _ = conn.Exec(context.Background(), "SET session_replication_role = 'origin'") }()

	// FK CASCADE 会自动清 business_account_api_key；但其他无 FK 的表手工清
	for _, q := range []string{
		"DELETE FROM business_account_ledger WHERE business_account_id = $1",
		"DELETE FROM webhook_event_outbox WHERE business_account_id = $1",
		"DELETE FROM business_account_balance WHERE business_account_id = $1",
		"DELETE FROM business_account WHERE id = $1", // 触发 FK CASCADE 删 api_key
	} {
		_, _ = conn.Exec(ctx, q, accountID)
	}
}

// uniqAccountID 测试隔离用：t.Name() + 纳秒戳，避免并行测试相互污染。
func uniqAccountID(t *testing.T, suffix string) string {
	t.Helper()
	clean := make([]byte, 0, len(t.Name()))
	for i := 0; i < len(t.Name()); i++ {
		c := t.Name()[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			clean = append(clean, c)
		} else {
			clean = append(clean, '_')
		}
	}
	return "bk-test-" + string(clean) + "-" + suffix + "-" + time.Now().Format("150405.000000000")
}

func ctxT(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return c
}

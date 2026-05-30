package session

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const defaultSessionTestDSN = "postgres://gateway:gateway_dev@127.0.0.1:55432/gateway?sslmode=disable"

func testPGDSN() string {
	if v := os.Getenv("SESSION_TEST_PG_DSN"); v != "" {
		return v
	}
	if v := os.Getenv("LEDGER_TEST_PG_DSN"); v != "" {
		return v
	}
	return defaultSessionTestDSN
}

// testPepper 固定 32 字节测试 pepper。
var testPepper = []byte("session-test-pepper-32bytes!ABCD0")

func mustOpenTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := testPGDSN()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	cfg.MaxConns = 10
	cfg.MinConns = 1
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

func newSilentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func newTestService(t *testing.T, pool *pgxpool.Pool, ttl time.Duration) *PostgresService {
	t.Helper()
	return NewPostgresService(pool, testPepper, ttl, newSilentLogger())
}

func setupSuite(t *testing.T, ttl time.Duration) (*pgxpool.Pool, *PostgresService) {
	t.Helper()
	pool := mustOpenTestPool(t)
	svc := newTestService(t, pool, ttl)
	t.Cleanup(func() { pool.Close() })
	return pool, svc
}

// createTestOperator 直接 SQL 插入 operator_account（session 测试只需 FK 目标，不认证）。
// 返回 (id, username)；t.Cleanup 删账户（admin_session 由 FK CASCADE 随删）。
func createTestOperator(t *testing.T, pool *pgxpool.Pool) (int64, string) {
	t.Helper()
	username := "sess_op_" + time.Now().Format("150405.000000000")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var id int64
	err := pool.QueryRow(ctx,
		`INSERT INTO operator_account (username, password_hash, enabled, created_by)
		 VALUES ($1, 'x-test-hash', true, 'test') RETURNING id`,
		username).Scan(&id)
	if err != nil {
		t.Fatalf("createTestOperator: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := pool.Exec(c, "DELETE FROM operator_account WHERE id = $1", id); err != nil {
			t.Logf("cleanup operator %d: %v", id, err)
		}
	})
	return id, username
}

// insertExpiredSession 直接 SQL 插入一个**已过期**会话（expires_at 在过去），返回其明文 token。
// 用 svc.hash(token) 与生产同算法算 token_hash（同包可访问），避免污染构造器的 ttl>0 约束。
func insertExpiredSession(t *testing.T, pool *pgxpool.Pool, svc *PostgresService, operatorID int64) string {
	t.Helper()
	token, err := randToken()
	if err != nil {
		t.Fatalf("randToken: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = pool.Exec(ctx,
		`INSERT INTO admin_session (session_token_hash, operator_id, csrf_token, expires_at)
		 VALUES ($1, $2, 'csrf-expired', NOW() - INTERVAL '1 hour')`,
		svc.hash(token), operatorID)
	if err != nil {
		t.Fatalf("insertExpiredSession: %v", err)
	}
	return token
}

func setOperatorEnabled(t *testing.T, pool *pgxpool.Pool, id int64, enabled bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, "UPDATE operator_account SET enabled = $1 WHERE id = $2", enabled, id); err != nil {
		t.Fatalf("setOperatorEnabled: %v", err)
	}
}

func ctxT(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return c
}

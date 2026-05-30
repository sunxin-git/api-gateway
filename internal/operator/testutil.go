package operator

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

// =============================================================================
// 测试用 PG 连接 helpers（与 internal/businesskey/testutil.go 同风格）
//
// DSN 优先级：OPERATOR_TEST_PG_DSN → LEDGER_TEST_PG_DSN → 默认（docker 55432）。
// 用户本机原生 PG 在 5432，已设 LEDGER_TEST_PG_DSN 指向之，故 fallback 命中正确库。
// =============================================================================

const defaultOperatorTestDSN = "postgres://gateway:gateway_dev@127.0.0.1:55432/gateway?sslmode=disable"

func testPGDSN() string {
	if v := os.Getenv("OPERATOR_TEST_PG_DSN"); v != "" {
		return v
	}
	if v := os.Getenv("LEDGER_TEST_PG_DSN"); v != "" {
		return v
	}
	return defaultOperatorTestDSN
}

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

// newTestService 用 bcrypt 最低 cost（4）提速；安全语义不变（cost 不影响正确性）。
func newTestService(t *testing.T, pool *pgxpool.Pool) *PostgresService {
	t.Helper()
	return NewPostgresServiceWithCost(pool, newSilentLogger(), bcrypt.MinCost)
}

// setupSuite 准备 pool + service；t.Cleanup 自动 Close pool。
func setupSuite(t *testing.T) (*pgxpool.Pool, *PostgresService) {
	t.Helper()
	pool := mustOpenTestPool(t)
	svc := newTestService(t, pool)
	t.Cleanup(func() { pool.Close() })
	return pool, svc
}

// uniqueUsername 生成测试隔离用户名：仅 [a-z0-9_]，长度 ≤ maxUsernameLen。
func uniqueUsername(suffix string) string {
	// 纳秒戳保证唯一；前缀短，总长远小于 64。
	return "op_" + sanitizeLower(suffix) + "_" + time.Now().Format("150405.000000000")
}

func sanitizeLower(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			out = append(out, c)
		case c >= 'A' && c <= 'Z':
			out = append(out, c+('a'-'A'))
		default:
			out = append(out, '_')
		}
	}
	if len(out) > 16 {
		out = out[:16]
	}
	return string(out)
}

// createTestAccount 建账户并注册 t.Cleanup 按 id 删（admin_session 由 FK CASCADE 随删）。
func createTestAccount(t *testing.T, svc *PostgresService, username, password string) *OperatorAccount {
	t.Helper()
	acct, err := svc.Create(ctxT(t), CreateParams{
		Username:  username,
		Password:  password,
		CreatedBy: "test",
	})
	if err != nil {
		t.Fatalf("createTestAccount(%s): %v", username, err)
	}
	t.Cleanup(func() { deleteAccountByID(t, svc.pool, acct.ID) })
	return acct
}

func deleteAccountByID(t *testing.T, pool *pgxpool.Pool, id int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, "DELETE FROM operator_account WHERE id = $1", id); err != nil {
		t.Logf("cleanup delete operator_account %d: %v", id, err)
	}
}

// wipeOperatorAccounts 清空 operator_account（bootstrap「表空」分支测试用）。
// operator_account 是本包独占表 + 测试 -p=1 顺序跑，安全；admin_session FK CASCADE 随删。
func wipeOperatorAccounts(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, "DELETE FROM operator_account"); err != nil {
		t.Fatalf("wipeOperatorAccounts: %v", err)
	}
}

func ctxT(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return c
}

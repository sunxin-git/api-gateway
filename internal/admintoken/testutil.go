package admintoken

import (
	"context"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// =============================================================================
// 测试用 PG 连接 helpers（Unit 2 / Unit 3 共享）
//
// 与 internal/ledger/testutil.go 同风格，但 admin token 没有"账户隔离"概念，
// 测试间隔离靠"描述前缀 + token_id 范围"两路 cleanup。
// =============================================================================

// 测试用 PG DSN：与 docker-compose 本地服务对齐（127.0.0.1:55432）。
// 可通过 env ADMINTOKEN_TEST_PG_DSN 覆盖。
const defaultAdminTokenTestDSN = "postgres://gateway:gateway_dev@127.0.0.1:55432/gateway?sslmode=disable"

// testPGDSN 返回测试用 DSN（先读 env 再 fallback）。
func testPGDSN() string {
	if v := os.Getenv("ADMINTOKEN_TEST_PG_DSN"); v != "" {
		return v
	}
	return defaultAdminTokenTestDSN
}

// testPepper 测试用 32 字节 pepper；固定值便于跨测试一致性。
// **不**与生产 pepper 共用；生产从 env 注入。
var testPepper = []byte("admintoken-test-pepper-32-bytes!XX")

// mustOpenTestPool 建立 pgxpool 连接；失败 t.Skip（与 ledger 包一致：本地无 PG 跳过）。
//
// MaxConns=30：本包并发测试最大 100 goroutine 写 IncrementTokenUsage（行级锁串行化），
// 30 conn 足够；不取更高值是为避免与 admin / ledger 包并行跑时把 PG 默认 100 连接池打满。
func mustOpenTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := testPGDSN()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	cfg.MaxConns = 30
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
func newTestService(t *testing.T, pool *pgxpool.Pool) *PostgresService {
	t.Helper()
	return NewPostgresService(pool, testPepper, newSilentLogger())
}

// newSilentLogger 返回不输出任何内容的 slog logger（避免测试 stdout 污染）。
func newSilentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// setupSuite 准备 pool + service；t.Cleanup 自动 Close pool。
//
// **不**做 TRUNCATE：测试间靠 description 前缀隔离 + 注册 cleanup 删行。
func setupSuite(t *testing.T) (*pgxpool.Pool, *PostgresService) {
	t.Helper()
	pool := mustOpenTestPool(t)
	svc := newTestService(t, pool)
	t.Cleanup(func() {
		pool.Close()
	})
	return pool, svc
}

// cleanupTokensByDescription 按 description 前缀清理（用于按测试名隔离）。
//
// FK CASCADE 会一并清 usage / circuit。
func cleanupTokensByDescription(t *testing.T, pool *pgxpool.Pool, descPrefix string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, "DELETE FROM gateway_admin_token WHERE description LIKE $1", descPrefix+"%"); err != nil {
		t.Fatalf("DELETE gateway_admin_token by description prefix %q: %v", descPrefix, err)
	}
}

// ctxT 5s 超时 ctx；t.Cleanup 自动 cancel。
func ctxT(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return c
}

// mustPrefix 解析 CIDR；测试用，失败 t.Fatal。
func mustPrefix(t *testing.T, s string) netip.Prefix {
	t.Helper()
	p, err := netip.ParsePrefix(s)
	if err != nil {
		t.Fatalf("netip.ParsePrefix(%q): %v", s, err)
	}
	return p
}

// mustAddr 解析单个 IP；测试用，失败 t.Fatal。
func mustAddr(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatalf("netip.ParseAddr(%q): %v", s, err)
	}
	return a
}

// baseCreateParams 返回一个最小合法 CreateParams（test 各 case 在此基础上覆盖字段）。
//
// 注意：description 必须由调用方覆盖为 testDescription(t, "...") 实现隔离。
func baseCreateParams(t *testing.T) CreateParams {
	t.Helper()
	return CreateParams{
		Description:           "PLACEHOLDER-MUST-BE-OVERWRITTEN",
		Scopes:                []string{"business_account:read"},
		AllowedCIDRs:          []netip.Prefix{mustPrefix(t, "10.0.0.0/8"), mustPrefix(t, "127.0.0.1/32")},
		CircuitBreakerEnabled: false,
		CreatedBy:             "cli:bootstrap",
	}
}

// testDescription 生成"测试隔离用"的 description；带 t.Name() 前缀。
//
// 测试 cleanup 用相同前缀 LIKE 删除。
func testDescription(t *testing.T, suffix string) string {
	t.Helper()
	return "admintoken-test:" + t.Name() + ":" + suffix
}

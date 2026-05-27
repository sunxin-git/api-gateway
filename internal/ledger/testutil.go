package ledger

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sunxin-git/api-gateway/internal/db"
)

// 测试用 PG DSN：与 docker-compose 本地服务对齐（127.0.0.1:55432）。
// 可通过 env LEDGER_TEST_PG_DSN 覆盖。
const defaultTestDSN = "postgres://gateway:gateway_dev@127.0.0.1:55432/gateway?sslmode=disable"

// testPGDSN 返回测试用的 PG DSN（先看 env 再 fallback）。
func testPGDSN() string {
	if v := os.Getenv("LEDGER_TEST_PG_DSN"); v != "" {
		return v
	}
	return defaultTestDSN
}

// mustOpenTestPool 建立 pgxpool 连接；失败 t.Fatal。
//
// MaxConns=30：足以支撑本包并发测试（100 goroutine 会在 pool 内部排队），
// 同时与 admintoken / admin 包并行测试时不打满 PG 默认 max_connections=100。
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

// cleanupAccount 删除指定账户相关的 ledger / outbox / balance / account 行（测试用）。
//
// 受 ledger 不可变 trigger 阻拦，需用 `SET session_replication_role = 'replica'` 临时绕过；
// 此设置 session-scoped，必须在同一 conn 上跑 DELETE 才生效。
//
// 仅用于测试 cleanup；生产代码禁止使用。
func cleanupAccount(t *testing.T, pool *pgxpool.Pool, accountIDs ...string) {
	t.Helper()
	if len(accountIDs) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire conn for cleanup: %v", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SET session_replication_role = 'replica'"); err != nil {
		t.Fatalf("set replication_role: %v", err)
	}
	defer func() {
		_, _ = conn.Exec(context.Background(), "SET session_replication_role = 'origin'")
	}()

	for _, id := range accountIDs {
		if _, err := conn.Exec(ctx, "DELETE FROM business_account_ledger WHERE business_account_id = $1", id); err != nil {
			t.Fatalf("delete ledger for %s: %v", id, err)
		}
		if _, err := conn.Exec(ctx, "DELETE FROM webhook_event_outbox WHERE business_account_id = $1", id); err != nil {
			t.Fatalf("delete outbox for %s: %v", id, err)
		}
		if _, err := conn.Exec(ctx, "DELETE FROM business_account_balance WHERE business_account_id = $1", id); err != nil {
			t.Fatalf("delete balance for %s: %v", id, err)
		}
		if _, err := conn.Exec(ctx, "DELETE FROM business_account WHERE id = $1", id); err != nil {
			t.Fatalf("delete account for %s: %v", id, err)
		}
	}
}

// truncateLedgerTables 已不再被默认使用 —— 保留供未来 reconciler/rebuild 测试需要 fresh DB 时调用。
// 默认 cleanup 改为按账户隔离（cleanupAccount），可与其他包测试并行运行。
//
//nolint:unused // 保留作为运维 / 调试 helper，不是 dead code
func truncateLedgerTables(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sqls := []string{
		"SET session_replication_role = 'replica'",
		"TRUNCATE TABLE business_account_ledger RESTART IDENTITY CASCADE",
		"TRUNCATE TABLE webhook_event_outbox RESTART IDENTITY CASCADE",
		"TRUNCATE TABLE business_account_balance CASCADE",
		"TRUNCATE TABLE business_account CASCADE",
		"SET session_replication_role = 'origin'",
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire conn for truncate: %v", err)
	}
	defer conn.Release()

	for _, s := range sqls {
		if _, err := conn.Exec(ctx, s); err != nil {
			t.Fatalf("truncate sql %q 失败: %v", s, err)
		}
	}
}

// newTestService 构造 PostgresService（含真 outbox publisher）。
func newTestService(t *testing.T, pool *pgxpool.Pool) *PostgresService {
	t.Helper()
	return NewPostgresService(pool, &testOutbox{}, newSilentLogger())
}

// newSilentLogger 返回不输出任何内容的 slog logger（避免测试 stdout 污染）。
func newSilentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// testOutbox 是 OutboxPublisher 的真 INSERT 实现（直接调 sqlc 生成的 InsertOutboxEvent）。
//
// 不复用 internal/outbox.PostgresPublisher 是为避免循环依赖
// （outbox 包 import ledger 包；测试若 import outbox 会构成循环）。
// 行为与生产 outbox.PostgresPublisher 完全一致。
type testOutbox struct {
	failNext bool // 测试可注入故障：设 true 让下一次 Publish 直接返 error
}

func (o *testOutbox) PublishInTx(ctx context.Context, tx pgx.Tx, event Event) error {
	if o.failNext {
		o.failNext = false
		return fmt.Errorf("testOutbox: 注入失败")
	}
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

// assertInvariant 校验账本不变量：available + reserved + used_total = recharge_total。
//
// 同时校验三态非负 + refund_total 非负（refund_total 不进等式但必须非负）。
func assertInvariant(t *testing.T, b *Balance) {
	t.Helper()
	if b == nil {
		t.Fatal("assertInvariant: balance 为 nil")
	}
	if b.Available < 0 || b.Reserved < 0 || b.UsedTotal < 0 || b.RechargeTotal < 0 || b.RefundTotal < 0 {
		t.Fatalf("不变量违反：三态有负值 %+v", b)
	}
	if b.Available+b.Reserved+b.UsedTotal != b.RechargeTotal {
		t.Fatalf("不变量违反：available(%d) + reserved(%d) + used_total(%d) != recharge_total(%d)",
			b.Available, b.Reserved, b.UsedTotal, b.RechargeTotal)
	}
}

// assertInvariantDB 从 DB 重读 balance 后做不变量断言（端到端校验）。
func assertInvariantDB(t *testing.T, svc *PostgresService, accountID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b, err := svc.GetBalance(ctx, accountID)
	if err != nil {
		t.Fatalf("assertInvariantDB GetBalance: %v", err)
	}
	assertInvariant(t, b)
}

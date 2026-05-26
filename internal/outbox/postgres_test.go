package outbox

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/sunxin-git/api-gateway/internal/db"
	"github.com/sunxin-git/api-gateway/internal/ledger"
)

const defaultTestDSN = "postgres://gateway:gateway_dev@127.0.0.1:55432/gateway?sslmode=disable"

func testPGDSN() string {
	if v := os.Getenv("LEDGER_TEST_PG_DSN"); v != "" {
		return v
	}
	return defaultTestDSN
}

func mustOpenPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(testPGDSN())
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	cfg.MaxConns = 10
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("PG 不可达：%v", err)
	}
	return pool
}

// uniqAccount 生成测试隔离的账户 ID（带 outbox- 前缀 + 测试名 + 纳秒），
// 与 ledger 包测试的账户 ID 不会冲突，可与 ledger 测试并行运行。
func uniqAccount(t *testing.T) string {
	t.Helper()
	return "outbox-" + sanitize(t.Name()) + "-" + time.Now().Format("150405.000000000")
}

func sanitize(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			out = append(out, c)
		} else {
			out = append(out, '_')
		}
	}
	return string(out)
}

func mustCreateAccount(t *testing.T, pool *pgxpool.Pool, id string) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, "INSERT INTO business_account (id) VALUES ($1) ON CONFLICT DO NOTHING", id)
	require.NoError(t, err)
	t.Cleanup(func() {
		// 仅删自己创建的账户行（不动 ledger）：先删 outbox events，再删 balance（若有），再删 account。
		_, _ = pool.Exec(ctx, "DELETE FROM webhook_event_outbox WHERE business_account_id = $1", id)
		_, _ = pool.Exec(ctx, "DELETE FROM business_account_balance WHERE business_account_id = $1", id)
		_, _ = pool.Exec(ctx, "DELETE FROM business_account WHERE id = $1", id)
	})
}

func TestPublishInTx_Happy(t *testing.T) {
	pool := mustOpenPool(t)
	t.Cleanup(pool.Close)
	acc := uniqAccount(t)
	mustCreateAccount(t, pool, acc)

	ctx := context.Background()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	require.NoError(t, err)
	defer tx.Rollback(ctx)

	pub := NewPostgresPublisher()

	payload, _ := json.Marshal(ledger.AccountCreatedPayload{
		BusinessAccountID: acc,
		OccurredAt:        time.Now().UTC(),
	})

	idemKey := "account.created:" + acc + ":create"
	evt := ledger.Event{
		Type:                   ledger.EventTypeAccountCreated,
		BusinessAccountID:      acc,
		Payload:                payload,
		IsFinancial:            false,
		RetentionUntil:         time.Now().Add(5 * time.Minute),
		DeliveryIdempotencyKey: idemKey,
	}
	require.NoError(t, pub.PublishInTx(ctx, tx, evt))
	require.NoError(t, tx.Commit(ctx))

	var count int
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM webhook_event_outbox WHERE delivery_idempotency_key = $1", idemKey).Scan(&count))
	require.Equal(t, 1, count)
}

func TestPublishInTx_FinancialEventLongRetention(t *testing.T) {
	pool := mustOpenPool(t)
	t.Cleanup(pool.Close)
	acc := uniqAccount(t)
	mustCreateAccount(t, pool, acc)

	ctx := context.Background()
	tx, _ := pool.BeginTx(ctx, pgx.TxOptions{})
	defer tx.Rollback(ctx)

	pub := NewPostgresPublisher()
	retention := time.Now().Add(365 * 24 * time.Hour)
	idemKey := "account.recharged:" + acc + ":1"
	evt := ledger.Event{
		Type:                   ledger.EventTypeAccountRecharged,
		BusinessAccountID:      acc,
		Payload:                []byte(`{"amount":100}`),
		IsFinancial:            true,
		RetentionUntil:         retention,
		DeliveryIdempotencyKey: idemKey,
	}
	require.NoError(t, pub.PublishInTx(ctx, tx, evt))
	require.NoError(t, tx.Commit(ctx))

	var (
		isFinancial bool
		retUntil    time.Time
	)
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT is_financial, retention_until FROM webhook_event_outbox WHERE delivery_idempotency_key = $1",
		idemKey).Scan(&isFinancial, &retUntil))
	require.True(t, isFinancial)
	require.WithinDuration(t, retention, retUntil, time.Second)
}

func TestPublishInTx_DuplicateIdempotencyKeyFails(t *testing.T) {
	pool := mustOpenPool(t)
	t.Cleanup(pool.Close)
	acc := uniqAccount(t)
	mustCreateAccount(t, pool, acc)

	ctx := context.Background()
	pub := NewPostgresPublisher()
	idemKey := "dup-" + acc

	commonEvt := func() ledger.Event {
		return ledger.Event{
			Type:                   ledger.EventTypeAccountCreated,
			BusinessAccountID:      acc,
			Payload:                []byte(`{}`),
			IsFinancial:            false,
			RetentionUntil:         time.Now().Add(5 * time.Minute),
			DeliveryIdempotencyKey: idemKey,
		}
	}

	tx, _ := pool.BeginTx(ctx, pgx.TxOptions{})
	require.NoError(t, pub.PublishInTx(ctx, tx, commonEvt()))
	require.NoError(t, tx.Commit(ctx))

	tx2, _ := pool.BeginTx(ctx, pgx.TxOptions{})
	defer tx2.Rollback(ctx)
	err := pub.PublishInTx(ctx, tx2, commonEvt())
	require.Error(t, err)
}

func TestPublishInTx_RollbackDropsRow(t *testing.T) {
	pool := mustOpenPool(t)
	t.Cleanup(pool.Close)
	acc := uniqAccount(t)
	mustCreateAccount(t, pool, acc)

	ctx := context.Background()
	tx, _ := pool.BeginTx(ctx, pgx.TxOptions{})

	pub := NewPostgresPublisher()
	idemKey := "rb-" + acc
	require.NoError(t, pub.PublishInTx(ctx, tx, ledger.Event{
		Type:                   ledger.EventTypeAccountCreated,
		BusinessAccountID:      acc,
		Payload:                []byte(`{}`),
		RetentionUntil:         time.Now().Add(5 * time.Minute),
		DeliveryIdempotencyKey: idemKey,
	}))
	require.NoError(t, tx.Rollback(ctx))

	var count int
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM webhook_event_outbox WHERE delivery_idempotency_key = $1", idemKey).Scan(&count))
	require.Equal(t, 0, count, "rollback 后 outbox 不应留行")
}

func TestPublishInTx_Validation(t *testing.T) {
	pool := mustOpenPool(t)
	t.Cleanup(pool.Close)

	ctx := context.Background()
	tx, _ := pool.BeginTx(ctx, pgx.TxOptions{})
	defer tx.Rollback(ctx)

	pub := NewPostgresPublisher()

	// 空 Type
	require.Error(t, pub.PublishInTx(ctx, tx, ledger.Event{
		Payload:                []byte(`{}`),
		RetentionUntil:         time.Now().Add(time.Minute),
		DeliveryIdempotencyKey: "k",
	}))
	// 空 DeliveryIdempotencyKey
	require.Error(t, pub.PublishInTx(ctx, tx, ledger.Event{
		Type:           ledger.EventTypeAccountCreated,
		Payload:        []byte(`{}`),
		RetentionUntil: time.Now().Add(time.Minute),
	}))
	// 零 RetentionUntil
	require.Error(t, pub.PublishInTx(ctx, tx, ledger.Event{
		Type:                   ledger.EventTypeAccountCreated,
		Payload:                []byte(`{}`),
		DeliveryIdempotencyKey: "k",
	}))
	// 空 Payload
	require.Error(t, pub.PublishInTx(ctx, tx, ledger.Event{
		Type:                   ledger.EventTypeAccountCreated,
		RetentionUntil:         time.Now().Add(time.Minute),
		DeliveryIdempotencyKey: "k",
	}))
}

// 编译期断言：未被测试直接引用的 sqlc 包仍然必须 import（防止 lint 投诉）。
var _ = db.New

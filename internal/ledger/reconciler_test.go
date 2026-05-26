package ledger

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/sunxin-git/api-gateway/internal/db"
	"github.com/sunxin-git/api-gateway/internal/obs"
)

// =============================================================================
// reconciler 测试 setup
// =============================================================================

// reconcilerHarness 把 reconciler 测试需要的依赖打包到一起。
type reconcilerHarness struct {
	pool       *pgxpool.Pool
	svc        *PostgresService
	metrics    *obs.Metrics
	reconciler *Reconciler
	// 测试 hook 状态
	preConfirmCalled atomic.Int32
	overrideListed   atomic.Int32
}

// newReconcilerHarness 构造 harness：单独的 advisory lock key 让并行测试互不阻塞。
func newReconcilerHarness(t *testing.T, opts ...func(*ReconcilerConfig)) *reconcilerHarness {
	t.Helper()
	pool := mustOpenTestPool(t)
	t.Cleanup(func() { pool.Close() })
	svc := newTestService(t, pool)
	metrics := obs.NewMetrics("test", "test")

	cfg := ReconcilerConfig{
		Interval:                  50 * time.Millisecond,
		InitialDelay:              1 * time.Nanosecond, // 触发默认值回落（NewReconciler 内部把 0 视为未配置）
		ConfirmDelay:              50 * time.Millisecond,
		DriftAction:               "log",
		RebuildStuckThreshold:     10 * time.Minute,
		OverloadAccountsThreshold: 5000,
		OverloadDurationThreshold: 60 * time.Second,
		// 每个测试用唯一 advisory key（基于纳秒时间戳低 32 位 + t.Name() hash，简单：用纳秒）。
		// 避免与生产 reconciler key 冲突，也避免与并行测试冲突。
		AdvisoryLockKey: time.Now().UnixNano() & 0x7FFFFFFFFFFFFFFF,
		Log:             newSilentLogger(),
		Metrics:         metrics,
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	h := &reconcilerHarness{
		pool:    pool,
		svc:     svc,
		metrics: metrics,
	}
	// 包装 hook：让 harness 计数。
	if cfg.PreConfirmHook != nil {
		orig := cfg.PreConfirmHook
		cfg.PreConfirmHook = func(ctx context.Context, accountID string) {
			h.preConfirmCalled.Add(1)
			orig(ctx, accountID)
		}
	}
	if cfg.OverrideListAccountsFn != nil {
		orig := cfg.OverrideListAccountsFn
		cfg.OverrideListAccountsFn = func(ctx context.Context, actual []db.ListAllUnfrozenAccountsForReconcilerRow) []db.ListAllUnfrozenAccountsForReconcilerRow {
			h.overrideListed.Add(1)
			return orig(ctx, actual)
		}
	}

	h.reconciler = NewReconciler(svc, pool, cfg)
	return h
}

// counterVecValue 取标签组合下当前计数值（找不到返 0）。
func counterVecValue(t *testing.T, vec *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	c, err := vec.GetMetricWithLabelValues(labels...)
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues(%v): %v", labels, err)
	}
	return testutil.ToFloat64(c)
}

// reconcilerCorruptAvailable 仅本测试文件使用的 drift 模拟 helper：直接增减 available。
// 不与 rebuild_test.go 的 corruptBalance 重名（后者签名是 sql + args 通用形式）。
//
// 仅用于测试；生产代码禁止此模式。
//
// 实现注意：balance 表的 CHECK 约束强制 `available + reserved + used_total = recharge_total`，
// 直接 SET available = available + N 会违反约束。
// 改为守恒转移：available 减 N，同时 used_total 加 N。
// 这制造了「ledger SUM 与 balance 不一致」的 drift（ledger 没有相应 entry），
// 但保持表级 CHECK 约束满足，绕开 DB 层兜底直接打 reconciler 路径。
//
// 参数 deltaAvailable：制造的 drift 量（正数=balance 比 ledger 多；负数=少）。
func reconcilerCorruptAvailable(t *testing.T, pool *pgxpool.Pool, accountID string, deltaAvailable int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// 守恒转移：available + deltaAvailable, used_total - deltaAvailable
	_, err := pool.Exec(ctx,
		`UPDATE business_account_balance
		    SET available  = available  + $1,
		        used_total = used_total - $1,
		        version    = version + 1,
		        updated_at = NOW()
		  WHERE business_account_id = $2`,
		deltaAvailable, accountID)
	require.NoError(t, err)
}

// setBalanceFrozenForRebuildStuck 把账户改成 rebuild stuck 状态（frozen_reason 含 rebuild_in_progress + frozen_at 过去时间）。
func setBalanceFrozenForRebuildStuck(t *testing.T, pool *pgxpool.Pool, accountID string, frozenAt time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := pool.Exec(ctx,
		`UPDATE business_account_balance
		    SET frozen = true,
		        frozen_reason = 'rebuild_in_progress',
		        frozen_at = $1,
		        version = version + 1,
		        updated_at = NOW()
		  WHERE business_account_id = $2`,
		frozenAt, accountID)
	require.NoError(t, err)
}

// reconcilerCountOutboxByType 计本账户某类型 outbox 事件数。
func reconcilerCountOutboxByType(t *testing.T, pool *pgxpool.Pool, accountID, eventType string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var n int
	err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM webhook_event_outbox WHERE business_account_id = $1 AND event_type = $2`,
		accountID, eventType).Scan(&n)
	require.NoError(t, err)
	return n
}

// readFrozen 读账户 frozen 状态（直查 DB，不经 service）。
func readFrozen(t *testing.T, pool *pgxpool.Pool, accountID string) (bool, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var frozen bool
	var reason pgtype.Text
	err := pool.QueryRow(ctx,
		"SELECT frozen, frozen_reason FROM business_account_balance WHERE business_account_id = $1",
		accountID).Scan(&frozen, &reason)
	require.NoError(t, err)
	return frozen, reason.String
}

// =============================================================================
// TestReconciler_Happy_NoDrift
// =============================================================================

func TestReconciler_Happy_NoDrift(t *testing.T) {
	h := newReconcilerHarness(t)
	accountID := "rec-happy-" + uniqueSuffix()
	newAccountWithBalance(t, h.svc, accountID, 1000)

	res, err := h.reconciler.RunOnce(ctxT(t))
	require.NoError(t, err)
	require.GreaterOrEqual(t, res.Checked, 1)
	require.Equal(t, 0, res.Drifted)
	require.Equal(t, 0, res.FalsePositives)
	// 当前账户应一致：drift counter 不增。
	require.InDelta(t, 0, counterVecValue(t, h.metrics.LedgerDriftTotal, accountID, "drift", "log"), 0)
}

// =============================================================================
// TestReconciler_TrueDrift_DryRun（默认模式只 log，不冻结，不发 outbox）
// =============================================================================

func TestReconciler_DryRunMode(t *testing.T) {
	h := newReconcilerHarness(t)
	accountID := "rec-dryrun-" + uniqueSuffix()
	newAccountWithBalance(t, h.svc, accountID, 1000)

	// 制造真 drift：直接 UPDATE balance -100。
	reconcilerCorruptAvailable(t, h.pool, accountID, -100)

	res, err := h.reconciler.RunOnce(ctxT(t))
	require.NoError(t, err)
	require.GreaterOrEqual(t, res.Drifted, 1)
	// 账户不应被冻结，不应有 outbox event。
	frozen, _ := readFrozen(t, h.pool, accountID)
	require.False(t, frozen, "dry-run 模式不应冻结账户")
	require.Equal(t, 0, reconcilerCountOutboxByType(t, h.pool, accountID, string(EventTypeAccountFrozen)))
	// metric 应 bump。
	require.GreaterOrEqual(t, counterVecValue(t, h.metrics.LedgerDriftTotal, accountID, "drift", "log"), float64(1))
}

// =============================================================================
// TestReconciler_TrueDrift_Freeze
// =============================================================================

func TestReconciler_TrueDrift_Freeze(t *testing.T) {
	h := newReconcilerHarness(t, func(c *ReconcilerConfig) {
		c.DriftAction = "freeze"
	})
	accountID := "rec-true-drift-" + uniqueSuffix()
	newAccountWithBalance(t, h.svc, accountID, 1000)

	// 制造真 drift。
	reconcilerCorruptAvailable(t, h.pool, accountID, -100)

	res, err := h.reconciler.RunOnce(ctxT(t))
	require.NoError(t, err)
	require.GreaterOrEqual(t, res.Drifted, 1)
	// 账户应被冻结。
	frozen, reason := readFrozen(t, h.pool, accountID)
	require.True(t, frozen)
	require.Equal(t, string(ReasonCodeDriftDetected), reason)
	// outbox 应发 account.frozen 一条。
	require.Equal(t, 1, reconcilerCountOutboxByType(t, h.pool, accountID, string(EventTypeAccountFrozen)))
	// metric bump 用 "freeze" 标签。
	require.GreaterOrEqual(t, counterVecValue(t, h.metrics.LedgerDriftTotal, accountID, "drift", "freeze"), float64(1))
}

// =============================================================================
// TestReconciler_FalsePositive
// =============================================================================

func TestReconciler_FalsePositive(t *testing.T) {
	accountID := "rec-fp-" + uniqueSuffix()
	var poolRef *pgxpool.Pool
	var svcRef *PostgresService

	h := newReconcilerHarness(t, func(c *ReconcilerConfig) {
		c.DriftAction = "freeze"
		c.ConfirmDelay = 10 * time.Millisecond
		// 在二次确认前，通过 Recharge 把 available 拉回正确值修正 drift。
		c.PreConfirmHook = func(ctx context.Context, id string) {
			if id != accountID {
				return
			}
			// 直接 UPDATE 把伪造的 -100 加回去（不能用 Recharge，因为那会同时改 ledger 让一致仍不一致）。
			// 这里模拟「另一并发 tx 完成了某个真实操作把 drift 抹平」，故直接守恒反向 UPDATE balance。
			// 守恒：available + 100, used_total - 100（与 reconcilerCorruptAvailable 的守恒转移反向，恢复初始 balance）。
			_, err := poolRef.Exec(ctx,
				`UPDATE business_account_balance
				    SET available  = available  + 100,
				        used_total = used_total - 100,
				        version    = version + 1,
				        updated_at = NOW()
				  WHERE business_account_id = $1`,
				id)
			require.NoError(t, err)
		}
	})
	poolRef = h.pool
	svcRef = h.svc
	_ = svcRef

	newAccountWithBalance(t, h.svc, accountID, 1000)
	reconcilerCorruptAvailable(t, h.pool, accountID, -100)

	res, err := h.reconciler.RunOnce(ctxT(t))
	require.NoError(t, err)
	require.GreaterOrEqual(t, res.FalsePositives, 1)
	require.Equal(t, 0, res.Drifted)
	// hook 被调过。
	require.GreaterOrEqual(t, h.preConfirmCalled.Load(), int32(1))
	// 账户不应被冻结。
	frozen, _ := readFrozen(t, h.pool, accountID)
	require.False(t, frozen)
	// 误报 metric 应 bump。
	require.GreaterOrEqual(t, counterVecValue(t, h.metrics.LedgerDriftFalsePositiveTotal, accountID), float64(1))
}

// =============================================================================
// TestReconciler_AlreadyFrozen
// =============================================================================

func TestReconciler_AlreadyFrozen(t *testing.T) {
	h := newReconcilerHarness(t)
	accountID := "rec-frozen-" + uniqueSuffix()
	newAccountWithBalance(t, h.svc, accountID, 1000)

	// 手工冻结。
	require.NoError(t, h.svc.Freeze(ctxT(t), Actor{Type: ActorTypeCLI, ID: "bootstrap"}, accountID, ReasonCodeManualFreeze))

	// 制造 drift。
	reconcilerCorruptAvailable(t, h.pool, accountID, -100)

	res, err := h.reconciler.RunOnce(ctxT(t))
	require.NoError(t, err)
	// ListAllUnfrozenAccountsForReconciler 应过滤掉本账户；本账户不会被检测。
	require.Equal(t, 0, res.Drifted)
	require.InDelta(t, 0, counterVecValue(t, h.metrics.LedgerDriftTotal, accountID, "drift", "log"), 0)
}

// =============================================================================
// TestReconciler_Shutdown
// =============================================================================

func TestReconciler_Shutdown(t *testing.T) {
	h := newReconcilerHarness(t, func(c *ReconcilerConfig) {
		c.Interval = 5 * time.Second
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.reconciler.Run(ctx) }()

	// 给点时间让 Run 拿到 advisory lock + 启动 ticker。
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("reconciler 未在 2s 内退出")
	}
}

// =============================================================================
// TestReconciler_PanicRecovery
// =============================================================================

func TestReconciler_PanicRecovery(t *testing.T) {
	flag := true
	h := newReconcilerHarness(t, func(c *ReconcilerConfig) {
		c.PanicOnce = &flag
		c.Interval = 50 * time.Millisecond
	})

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.reconciler.Run(ctx) }()

	// 等待至少一轮（含 panic + recover + 下一轮）。
	time.Sleep(300 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("reconciler 未退出")
	}

	// panic counter 应 bump。
	require.GreaterOrEqual(t, testutil.ToFloat64(h.metrics.ReconcilerPanicTotal), float64(1))
}

// =============================================================================
// TestReconciler_TripWire_Accounts（用 OverrideListAccountsFn 模拟 5001 账户）
// =============================================================================

func TestReconciler_TripWire_Accounts(t *testing.T) {
	h := newReconcilerHarness(t, func(c *ReconcilerConfig) {
		// 注：阈值降到 1，让任意 ≥ 2 账户即触发。OverrideList 给 2 个 fake 账户。
		c.OverloadAccountsThreshold = 1
		c.OverrideListAccountsFn = func(ctx context.Context, actual []db.ListAllUnfrozenAccountsForReconcilerRow) []db.ListAllUnfrozenAccountsForReconcilerRow {
			// 返 2 个 fake row（用不存在的账户，readSnapshot 会失败但只 log 不 fatal）。
			return []db.ListAllUnfrozenAccountsForReconcilerRow{
				{BusinessAccountID: "fake-1", Version: 0},
				{BusinessAccountID: "fake-2", Version: 0},
			}
		}
	})

	res, err := h.reconciler.RunOnce(ctxT(t))
	require.NoError(t, err)
	require.Equal(t, 2, res.Checked)
	require.GreaterOrEqual(t, counterVecValue(t, h.metrics.ReconcilerOverloadTotal, "accounts"), float64(1))
}

// =============================================================================
// TestReconciler_TripWire_Duration（阈值很小让 RunOnce 必超）
// =============================================================================

func TestReconciler_TripWire_Duration(t *testing.T) {
	h := newReconcilerHarness(t, func(c *ReconcilerConfig) {
		c.OverloadDurationThreshold = 1 * time.Nanosecond // 任意正常 RunOnce 都会超
	})

	_, err := h.reconciler.RunOnce(ctxT(t))
	require.NoError(t, err)
	require.GreaterOrEqual(t, counterVecValue(t, h.metrics.ReconcilerOverloadTotal, "duration"), float64(1))
}

// =============================================================================
// TestReconciler_RebuildStuckWatchdog
// =============================================================================

func TestReconciler_RebuildStuckWatchdog(t *testing.T) {
	h := newReconcilerHarness(t, func(c *ReconcilerConfig) {
		c.RebuildStuckThreshold = 10 * time.Minute
	})
	accountID := "rec-stuck-" + uniqueSuffix()
	newAccountWithBalance(t, h.svc, accountID, 1000)

	// 手工把账户调到 rebuild_in_progress + frozen_at = 11 分钟前。
	setBalanceFrozenForRebuildStuck(t, h.pool, accountID, time.Now().Add(-11*time.Minute))

	res, err := h.reconciler.RunOnce(ctxT(t))
	require.NoError(t, err)
	require.GreaterOrEqual(t, res.StuckRebuilds, 1)
	require.GreaterOrEqual(t, counterVecValue(t, h.metrics.LedgerRebuildStuckTotal, accountID), float64(1))
}

// =============================================================================
// TestReconciler_AdvisoryLockExclusion
// =============================================================================

func TestReconciler_AdvisoryLockExclusion(t *testing.T) {
	// 用相同 lock key 启两个 Reconciler；第二个应 skip。
	sharedKey := time.Now().UnixNano() & 0x7FFFFFFFFFFFFFFF
	if sharedKey == 0 {
		sharedKey = 1
	}

	h1 := newReconcilerHarness(t, func(c *ReconcilerConfig) {
		c.AdvisoryLockKey = sharedKey
		c.Interval = 5 * time.Second
	})
	h2 := newReconcilerHarness(t, func(c *ReconcilerConfig) {
		c.AdvisoryLockKey = sharedKey
		c.Interval = 5 * time.Second
	})

	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()

	// 启动 h1，让它先拿到锁。
	done1 := make(chan error, 1)
	go func() { done1 <- h1.reconciler.Run(ctx1) }()
	time.Sleep(150 * time.Millisecond)

	// 启动 h2，应立即 skip（return nil 不阻塞）。
	start := time.Now()
	err := h2.reconciler.Run(ctx2)
	require.NoError(t, err, "h2 应直接 return nil")
	require.Less(t, time.Since(start), 500*time.Millisecond, "h2 不应卡住")

	// skipped metric 应 bump。
	require.GreaterOrEqual(t, testutil.ToFloat64(h2.metrics.ReconcilerSkippedTotal), float64(1))

	// 收尾：让 h1 退出。
	cancel1()
	select {
	case <-done1:
	case <-time.After(2 * time.Second):
		t.Fatal("h1 未退出")
	}
}

// =============================================================================
// TestReconciler_RunOnceVsAdminCLI（admin-cli drift-check 复用 RunOnce）
// =============================================================================

func TestReconciler_RunOnceVsAdminCLI(t *testing.T) {
	h := newReconcilerHarness(t)
	accountID := "rec-cli-" + uniqueSuffix()
	newAccountWithBalance(t, h.svc, accountID, 1000)

	res, err := h.reconciler.RunOnce(ctxT(t))
	require.NoError(t, err)
	// RunResult 字段齐备（admin-cli 复用此返回打印）。
	require.GreaterOrEqual(t, res.Checked, 1)
	require.GreaterOrEqual(t, res.Duration, time.Duration(0))
}

// =============================================================================
// TestReconciler_Concurrent_RechargeNoFalsePositive
// （reconciler 跑时另一 goroutine Recharge：REPEATABLE READ 见一致快照，不误报）
// =============================================================================

func TestReconciler_Concurrent_RechargeNoFalsePositive(t *testing.T) {
	h := newReconcilerHarness(t, func(c *ReconcilerConfig) {
		c.ConfirmDelay = 30 * time.Millisecond
	})
	accountID := "rec-conc-" + uniqueSuffix()
	newAccountWithBalance(t, h.svc, accountID, 1000)

	// 并发：在 RunOnce 跑期间 Recharge。Recharge 是原子 CTE，
	// reconciler 用 REPEATABLE READ 拿一致快照，不应误报。
	ctx := ctxT(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		actor := Actor{Type: ActorTypeCLI, ID: "bootstrap"}
		for i := 0; i < 5; i++ {
			_, err := h.svc.Recharge(ctx, actor, RechargeParams{
				AccountID:      accountID,
				Amount:         100,
				IdempotencyKey: "conc-" + uniqueSuffixN(i),
				CanonicalBody:  &RechargeBody{AccountID: accountID, Amount: 100},
			})
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Logf("并发 Recharge: %v", err)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	res, err := h.reconciler.RunOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, res.Drifted)

	<-done
	// 最终一致。
	assertInvariantDB(t, h.svc, accountID)
}

// =============================================================================
// 工具
// =============================================================================

var uniqueCounter atomic.Int64

func uniqueSuffix() string {
	n := uniqueCounter.Add(1)
	return formatN(time.Now().UnixNano()) + "-" + formatN(n)
}

func uniqueSuffixN(i int) string {
	return uniqueSuffix() + "-" + formatN(int64(i))
}

func formatN(n int64) string {
	// 简易 itoa，避免 strconv import（test util 已尽量轻）。
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [24]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

// 编译期断言：reconciler 的 isConsistent 在零值上必为 true（防回归把 == 改成 != 之类）。
var _ = func() bool {
	if !isConsistent(expectedBalance{}, actualBalance{}) {
		panic("isConsistent zero-value 必为 true")
	}
	if isConsistent(expectedBalance{Available: 1}, actualBalance{}) {
		panic("isConsistent 差异检测失效")
	}
	return true
}()

// 上面用到的 pgx 引用：在 testReconcilerSnapshotInRepeatableRead 直接读时验证 tx options。
// 这里加一个静态使用避免 import 被 vet 抹掉（pgxpool 和 pgx 上面都用了，pgtype 在 setBalanceFrozenForRebuildStuck）。
var _ = pgx.RepeatableRead
var _ = pgxpool.Config{}
var _ = pgtype.Text{}

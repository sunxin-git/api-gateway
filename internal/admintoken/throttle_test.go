package admintoken

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// CheckSingleRecharge / CheckSingleRefund —— 纯内存，无需 PG
// =============================================================================

func TestThrottle_CheckSingleRecharge(t *testing.T) {
	thr := &PostgresThrottle{} // 纯内存路径不碰 pool

	limit := int64(1000)
	tok := &Token{ID: 1, SingleRechargeMax: &limit}

	require.NoError(t, thr.CheckSingleRecharge(tok, 500))
	require.NoError(t, thr.CheckSingleRecharge(tok, 1000), "等于上限通过")
	require.ErrorIs(t, thr.CheckSingleRecharge(tok, 1001), ErrSingleRechargeExceeded)

	noLimit := &Token{ID: 2, SingleRechargeMax: nil}
	require.NoError(t, thr.CheckSingleRecharge(noLimit, 1_000_000_000), "nil = 无上限")

	require.NoError(t, thr.CheckSingleRecharge(nil, 100), "nil token 不应 panic")
}

func TestThrottle_CheckSingleRefund(t *testing.T) {
	thr := &PostgresThrottle{}
	limit := int64(500)
	tok := &Token{ID: 1, SingleRefundMax: &limit}

	require.NoError(t, thr.CheckSingleRefund(tok, 500))
	require.ErrorIs(t, thr.CheckSingleRefund(tok, 501), ErrSingleRefundExceeded)
	require.NoError(t, thr.CheckSingleRefund(&Token{ID: 2}, 1_000_000), "nil refund max = 无上限")
}

// TestThrottle_CheckRPM 验证 throttle.CheckRPM 薄包装委托到 InProcessRPM.Check（不重复 RPM 全套测试）。
func TestThrottle_CheckRPM(t *testing.T) {
	pool, _ := setupSuite(t)
	thr := newTestThrottle(t, pool)

	limit := int32(2)
	tok := &Token{ID: 12345, RequestsPerMinute: &limit}
	require.NoError(t, thr.CheckRPM(tok))
	require.NoError(t, thr.CheckRPM(tok))
	require.ErrorIs(t, thr.CheckRPM(tok), ErrRPMExceeded)
}

// =============================================================================
// InProcessRPM —— 不需要 PG，可独立测试
// =============================================================================

func TestRPM_HappyPath(t *testing.T) {
	rpm := newTestRPM()
	t.Cleanup(func() { _ = rpm.Close() })

	limit := int32(5)
	tok := &Token{ID: 100, RequestsPerMinute: &limit}

	for i := 0; i < 5; i++ {
		require.NoError(t, rpm.Check(tok), "第 %d 次必须通过", i+1)
	}
	require.ErrorIs(t, rpm.Check(tok), ErrRPMExceeded, "第 6 次必须拒绝")
}

func TestRPM_NilLimitNoLimit(t *testing.T) {
	rpm := newTestRPM()
	t.Cleanup(func() { _ = rpm.Close() })
	tok := &Token{ID: 1, RequestsPerMinute: nil}
	for i := 0; i < 1000; i++ {
		require.NoError(t, rpm.Check(tok))
	}
}

func TestRPM_ZeroLimitAlwaysDeny(t *testing.T) {
	rpm := newTestRPM()
	t.Cleanup(func() { _ = rpm.Close() })
	zero := int32(0)
	tok := &Token{ID: 2, RequestsPerMinute: &zero}
	require.ErrorIs(t, rpm.Check(tok), ErrRPMExceeded)
}

func TestRPM_RejectedRequestsDoNotConsumeQuota(t *testing.T) {
	rpm := newTestRPM()
	t.Cleanup(func() { _ = rpm.Close() })
	limit := int32(3)
	tok := &Token{ID: 3, RequestsPerMinute: &limit}

	// 用满
	for i := 0; i < 3; i++ {
		require.NoError(t, rpm.Check(tok))
	}
	// 被拒
	require.ErrorIs(t, rpm.Check(tok), ErrRPMExceeded)
	require.ErrorIs(t, rpm.Check(tok), ErrRPMExceeded)

	// 被拒的请求不应追加 timestamp；count 仍是 3
	require.Equal(t, 3, rpm.peekCount(tok.ID))
}

func TestRPM_WindowSliding(t *testing.T) {
	rpm := newTestRPM()
	t.Cleanup(func() { _ = rpm.Close() })

	// 注入可控时钟
	var nowNs int64
	rpm.now = func() time.Time { return time.Unix(0, atomic.LoadInt64(&nowNs)) }
	atomic.StoreInt64(&nowNs, time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC).UnixNano())

	limit := int32(2)
	tok := &Token{ID: 10, RequestsPerMinute: &limit}

	require.NoError(t, rpm.Check(tok))
	require.NoError(t, rpm.Check(tok))
	require.ErrorIs(t, rpm.Check(tok), ErrRPMExceeded)

	// 前进 61s，旧 timestamps 全部滑出窗口
	atomic.AddInt64(&nowNs, int64(61*time.Second))
	require.NoError(t, rpm.Check(tok), "窗口滑出后应再次通过")
	require.NoError(t, rpm.Check(tok))
	require.ErrorIs(t, rpm.Check(tok), ErrRPMExceeded, "新窗口达上限又拒绝")
}

func TestRPM_BurstWithinSecond(t *testing.T) {
	rpm := newTestRPM()
	t.Cleanup(func() { _ = rpm.Close() })

	// 模拟"500ms 内 200 次" burst：注入快速时钟
	nowNs := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC).UnixNano()
	rpm.now = func() time.Time { return time.Unix(0, atomic.LoadInt64(&nowNs)) }

	limit := int32(50)
	tok := &Token{ID: 20, RequestsPerMinute: &limit}

	// 50 次每次推 1ms（仍在 1 分钟窗口内）→ 全通过
	for i := 0; i < 50; i++ {
		require.NoError(t, rpm.Check(tok), "第 %d 次 1ms 步进", i+1)
		atomic.AddInt64(&nowNs, int64(time.Millisecond))
	}
	// 第 51 次（51ms 后，仍在 60s 窗口内）拒绝
	require.ErrorIs(t, rpm.Check(tok), ErrRPMExceeded)
}

func TestRPM_GCRemovesIdleTokens(t *testing.T) {
	rpm := newTestRPM()
	t.Cleanup(func() { _ = rpm.Close() })
	nowNs := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC).UnixNano()
	rpm.now = func() time.Time { return time.Unix(0, atomic.LoadInt64(&nowNs)) }
	rpm.idleThreshold = time.Minute

	limit := int32(10)
	tok := &Token{ID: 99, RequestsPerMinute: &limit}
	require.NoError(t, rpm.Check(tok))
	require.Equal(t, 1, rpm.peekCount(tok.ID))

	// 推进 2 分钟，token 变 idle
	atomic.AddInt64(&nowNs, int64(2*time.Minute))
	rpm.gcOnce()
	require.Equal(t, 0, rpm.peekCount(tok.ID), "idle token 的 state 应被 GC")
}

func TestRPM_ConcurrentSafe(t *testing.T) {
	rpm := newTestRPM()
	t.Cleanup(func() { _ = rpm.Close() })

	limit := int32(1000) // 大于并发 goroutine 数
	tok := &Token{ID: 50, RequestsPerMinute: &limit}

	var wg sync.WaitGroup
	const N = 200
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			_ = rpm.Check(tok)
		}()
	}
	wg.Wait()
	require.Equal(t, N, rpm.peekCount(tok.ID), "200 并发 Check 应全部通过且 count = 200")
}

func TestRPM_ColdStartHookFiresOnce(t *testing.T) {
	var n int32
	rpm := NewInProcessRPM(newSilentLogger(), func() { atomic.AddInt32(&n, 1) })
	t.Cleanup(func() { _ = rpm.Close() })
	require.Equal(t, int32(1), atomic.LoadInt32(&n), "构造时调用一次 onColdStart")
}

func TestRPM_PanicOnNilLogger(t *testing.T) {
	defer func() {
		require.NotNil(t, recover(), "nil logger 必须 panic")
	}()
	_ = NewInProcessRPM(nil, nil)
}

// newTestRPM 构造一个测试用 InProcessRPM；GC interval 调小避免测试卡 5 分钟。
func newTestRPM() *InProcessRPM {
	r := NewInProcessRPM(newSilentLogger(), nil)
	r.gcInterval = time.Hour // 测试用；不让自动 GC 干扰；测试用 gcOnce() 手动触发
	return r
}

// =============================================================================
// CheckCircuitBreaker / RecordHandlerError —— 需要 PG
// =============================================================================

func TestThrottle_CircuitBreaker_DisabledTokenNoOp(t *testing.T) {
	pool, _ := setupSuite(t)
	t.Cleanup(func() { cleanupTokensByDescription(t, pool, "admintoken-test:"+t.Name()) })

	thr := newTestThrottle(t, pool)
	tok := &Token{ID: 1, CircuitBreakerEnabled: false}

	// disabled 时 CheckCircuitBreaker 必定通过（不查 DB 也不会因不存在 row 失败）
	require.NoError(t, thr.CheckCircuitBreaker(context.Background(), tok))

	// RecordHandlerError disabled 同样直接返回
	require.NoError(t, thr.RecordHandlerError(context.Background(), tok))
}

func TestThrottle_CircuitBreaker_TripAfterThreshold(t *testing.T) {
	pool, svc := setupSuite(t)
	ctx := ctxT(t)
	t.Cleanup(func() { cleanupTokensByDescription(t, pool, "admintoken-test:"+t.Name()) })

	thr := newTestThrottle(t, pool)

	// 创建 enabled token；用真 token id 避免 FK 违反
	p := baseCreateParams(t)
	p.Description = testDescription(t, "trip")
	p.CircuitBreakerEnabled = true
	tok, _, err := svc.Create(ctx, p)
	require.NoError(t, err)
	tok.CircuitBreakerEnabled = true

	// 跳闸前未熔断
	require.NoError(t, thr.CheckCircuitBreaker(ctx, tok))

	// 累加到 100 次错误 → 跳闸
	for i := 0; i < circuitErrorThreshold; i++ {
		require.NoError(t, thr.RecordHandlerError(ctx, tok))
	}

	// 现在 CheckCircuitBreaker 必须返 ErrCircuitOpen
	require.ErrorIs(t, thr.CheckCircuitBreaker(ctx, tok), ErrCircuitOpen)

	// DB 中 breaker_tripped_until 应在未来
	var until time.Time
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT breaker_tripped_until FROM gateway_admin_token_circuit WHERE token_id = $1", tok.ID).Scan(&until))
	require.True(t, until.After(time.Now()), "breaker_tripped_until 必须在未来")
}

func TestThrottle_RecordCircuitError_ConcurrentSafe(t *testing.T) {
	pool, svc := setupSuite(t)
	ctx := ctxT(t)
	t.Cleanup(func() { cleanupTokensByDescription(t, pool, "admintoken-test:"+t.Name()) })

	thr := newTestThrottle(t, pool)

	p := baseCreateParams(t)
	p.Description = testDescription(t, "concurrent-circuit")
	p.CircuitBreakerEnabled = true
	tok, _, err := svc.Create(ctx, p)
	require.NoError(t, err)
	tok.CircuitBreakerEnabled = true

	const N = 50
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			_ = thr.RecordHandlerError(ctx, tok)
		}()
	}
	wg.Wait()

	var ec int32
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT error_count FROM gateway_admin_token_circuit WHERE token_id = $1", tok.ID).Scan(&ec))
	require.Equal(t, int32(N), ec, "50 并发 RecordHandlerError 累加必须无丢失")
}

// =============================================================================
// Daily counter —— 两步式（Check 预检 → LedgerService → Record）
// =============================================================================

func TestThrottle_DailyRecharge_HappyPath(t *testing.T) {
	pool, svc := setupSuite(t)
	ctx := ctxT(t)
	t.Cleanup(func() { cleanupTokensByDescription(t, pool, "admintoken-test:"+t.Name()) })

	thr := newTestThrottle(t, pool)

	limit := int64(1000)
	p := baseCreateParams(t)
	p.Description = testDescription(t, "daily-recharge")
	p.DailyRechargeQuotaLimit = &limit
	tok, _, err := svc.Create(ctx, p)
	require.NoError(t, err)
	tok.DailyRechargeQuotaLimit = &limit

	// 初始 0 用量；预检 amount=600 通过
	require.NoError(t, thr.CheckDailyRecharge(ctx, tok, 600))
	require.NoError(t, thr.RecordSuccessfulRecharge(ctx, tok.ID, 600))

	// 剩余 400；预检 401 失败
	require.ErrorIs(t, thr.CheckDailyRecharge(ctx, tok, 401), ErrDailyRechargeExceeded)
	// 预检 400 通过（恰好上限）
	require.NoError(t, thr.CheckDailyRecharge(ctx, tok, 400))
	require.NoError(t, thr.RecordSuccessfulRecharge(ctx, tok.ID, 400))

	// 现在已用满；再 1 块拒
	require.ErrorIs(t, thr.CheckDailyRecharge(ctx, tok, 1), ErrDailyRechargeExceeded)
}

func TestThrottle_DailyRecharge_NilLimitNoLimit(t *testing.T) {
	pool, _ := setupSuite(t)
	thr := newTestThrottle(t, pool)
	tok := &Token{ID: 999_999, DailyRechargeQuotaLimit: nil}
	require.NoError(t, thr.CheckDailyRecharge(context.Background(), tok, 1_000_000_000_000))
}

func TestThrottle_DailyRefund_HappyPath(t *testing.T) {
	pool, svc := setupSuite(t)
	ctx := ctxT(t)
	t.Cleanup(func() { cleanupTokensByDescription(t, pool, "admintoken-test:"+t.Name()) })

	thr := newTestThrottle(t, pool)
	limit := int64(500)
	p := baseCreateParams(t)
	p.Description = testDescription(t, "daily-refund")
	p.DailyRefundQuotaLimit = &limit
	tok, _, err := svc.Create(ctx, p)
	require.NoError(t, err)
	tok.DailyRefundQuotaLimit = &limit

	require.NoError(t, thr.CheckDailyRefund(ctx, tok, 300))
	require.NoError(t, thr.RecordSuccessfulRefund(ctx, tok.ID, 300))
	require.ErrorIs(t, thr.CheckDailyRefund(ctx, tok, 201), ErrDailyRefundExceeded)
}

func TestThrottle_DailyCreate_HappyPath(t *testing.T) {
	pool, svc := setupSuite(t)
	ctx := ctxT(t)
	t.Cleanup(func() { cleanupTokensByDescription(t, pool, "admintoken-test:"+t.Name()) })

	thr := newTestThrottle(t, pool)
	limit := int32(2)
	p := baseCreateParams(t)
	p.Description = testDescription(t, "daily-create")
	p.DailyAccountCreateLimit = &limit
	tok, _, err := svc.Create(ctx, p)
	require.NoError(t, err)
	tok.DailyAccountCreateLimit = &limit

	require.NoError(t, thr.CheckDailyCreate(ctx, tok))
	require.NoError(t, thr.RecordSuccessfulCreate(ctx, tok.ID))

	require.NoError(t, thr.CheckDailyCreate(ctx, tok))
	require.NoError(t, thr.RecordSuccessfulCreate(ctx, tok.ID))

	require.ErrorIs(t, thr.CheckDailyCreate(ctx, tok), ErrDailyCreateExceeded)
}

func TestThrottle_DailyRecharge_ConcurrentNoLoss(t *testing.T) {
	pool, svc := setupSuite(t)
	ctx := ctxT(t)
	t.Cleanup(func() { cleanupTokensByDescription(t, pool, "admintoken-test:"+t.Name()) })

	thr := newTestThrottle(t, pool)

	// 不设上限，专测累加原子性
	p := baseCreateParams(t)
	p.Description = testDescription(t, "concurrent-daily")
	tok, _, err := svc.Create(ctx, p)
	require.NoError(t, err)

	const N = 100
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			require.NoError(t, thr.RecordSuccessfulRecharge(ctx, tok.ID, 1))
		}()
	}
	wg.Wait()

	var total int64
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT recharge_total_minor FROM gateway_admin_token_usage WHERE token_id = $1", tok.ID).Scan(&total))
	require.Equal(t, int64(N), total, "100 并发各 +1 必须无丢失（ON CONFLICT 原子性）")
}

// newTestThrottle 构造测试用 PostgresThrottle（含 InProcessRPM）。
func newTestThrottle(t *testing.T, pool *pgxpool.Pool) *PostgresThrottle {
	t.Helper()
	rpm := newTestRPM()
	t.Cleanup(func() { _ = rpm.Close() })
	return NewPostgresThrottle(pool, rpm, newSilentLogger())
}

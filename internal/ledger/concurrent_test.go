package ledger

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// =============================================================================
// Unit 6: 高并发原子性测试 + 输入校验
//
// 设计原则：
//   - service 不内部重试；测试侧 retry wrapper 包装 ErrVersionConflict（设计文档 §3ter）
//   - ErrInsufficientBalance 不重试（重试无意义，余额本就不够）
//   - 栅栏同步：所有 goroutine 用 close(channel) 同时发车，最大化竞争
//   - 不变量恒守：available + reserved + used_total = recharge_total
// =============================================================================

// reserveWithRetry 在测试侧包装 ErrVersionConflict 重试。
//
// 策略：最多 maxAttempts 次，每次失败按指数 + jitter 退避；
// ErrInsufficientBalance 立即返回不重试；其他 error 立即返回。
func reserveWithRetry(
	ctx context.Context,
	svc *PostgresService,
	actor Actor,
	params ReserveParams,
	maxAttempts int,
) (*LedgerEntry, error) {
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		entry, err := svc.Reserve(ctx, actor, params)
		if err == nil {
			return entry, nil
		}
		lastErr = err
		if errors.Is(err, ErrInsufficientBalance) {
			// 余额不足：重试也无济于事（其他并发同样在抢）。
			return nil, err
		}
		if !errors.Is(err, ErrVersionConflict) {
			// 非预期错误：直接抛回。
			return nil, err
		}
		// version 冲突：指数 + jitter 退避后重试。
		// backoff 上限 128ms（避免长尾在 100 goroutine 风暴下沉默太久）。
		shift := uint(attempt)
		if shift > 7 {
			shift = 7
		}
		backoff := time.Duration(1<<shift) * time.Millisecond
		jitter := time.Duration(rand.Int63n(int64(backoff) + 1))
		time.Sleep(backoff + jitter)
	}
	return nil, fmt.Errorf("重试 %d 次仍失败: %w", maxAttempts, lastErr)
}

// =============================================================================
// 核心测试：100 goroutine 并发 Reserve 不超卖
// =============================================================================

func TestConcurrentReserveNoOversell(t *testing.T) {
	_, svc := setupSuite(t)
	const (
		accountID = "biz-concurrent-001"
		recharge  = int64(1000)
		each      = int64(20) // 50 笔可成功
		nGo       = 100
		maxRetry  = 30 // 100 goroutine 高竞争下需要更多重试吸收 version 冲突
	)
	newAccountWithBalance(t, svc, accountID, recharge)

	var (
		successCount      atomic.Int64
		insufficientCount atomic.Int64
		otherErrCount     atomic.Int64
		errs              sync.Map // 用于审计：goroutine 索引 → 错误说明
	)

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(nGo)

	for i := 0; i < nGo; i++ {
		go func(idx int) {
			defer wg.Done()
			<-start // 栅栏：同时发车

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			actor := Actor{Type: ActorTypeTask, ID: fmt.Sprintf("t-%d", idx)}
			params := ReserveParams{
				AccountID:     accountID,
				Amount:        each,
				CorrelationID: fmt.Sprintf("rsv-%d", idx),
			}

			_, err := reserveWithRetry(ctx, svc, actor, params, maxRetry)
			switch {
			case err == nil:
				successCount.Add(1)
			case errors.Is(err, ErrInsufficientBalance):
				insufficientCount.Add(1)
			default:
				otherErrCount.Add(1)
				errs.Store(idx, err.Error())
			}
		}(i)
	}

	close(start)
	wg.Wait()

	t.Logf("success=%d insufficient=%d other=%d",
		successCount.Load(), insufficientCount.Load(), otherErrCount.Load())

	// 打印未预期错误（如有）。
	errs.Range(func(k, v any) bool {
		t.Logf("goroutine %v 异常错误: %v", k, v)
		return true
	})

	require.Equal(t, int64(nGo), successCount.Load()+insufficientCount.Load()+otherErrCount.Load(),
		"总数应 = nGo")
	require.Equal(t, int64(50), successCount.Load(), "余额 1000/20=50 笔应成功")
	require.Equal(t, int64(50), insufficientCount.Load(), "其余 50 笔余额不足")
	require.Equal(t, int64(0), otherErrCount.Load(), "重试足够后不应有 ErrVersionConflict 残留")

	// 不变量 + 终态校验。
	bal, err := svc.GetBalance(context.Background(), accountID)
	require.NoError(t, err)
	assertInvariant(t, bal)
	require.Equal(t, int64(0), bal.Available, "余额应耗尽")
	require.Equal(t, int64(1000), bal.Reserved, "全部转为 reserved")
	// version 单调递增：newAccountWithBalance 做 1 次 Recharge → version=1；加 50 次 reserve 成功 CAS → 51。
	require.Equal(t, int64(51), bal.Version, "1(recharge) + 50(reserve) = 51")
}

// =============================================================================
// 输入校验：amount > 0
// =============================================================================

func TestReserveAmountZero(t *testing.T) {
	_, svc := setupSuite(t)
	ctx := ctxT(t)
	actor := Actor{Type: ActorTypeTask, ID: "t"}
	_, err := svc.Reserve(ctx, actor, ReserveParams{
		AccountID: "x", Amount: 0, CorrelationID: "c",
	})
	require.ErrorIs(t, err, ErrInvalidAmount)
}

func TestReserveAmountNegative(t *testing.T) {
	_, svc := setupSuite(t)
	ctx := ctxT(t)
	actor := Actor{Type: ActorTypeTask, ID: "t"}
	for _, amt := range []int64{-1, -100, -9999} {
		_, err := svc.Reserve(ctx, actor, ReserveParams{
			AccountID: "x", Amount: amt, CorrelationID: "c",
		})
		require.ErrorIs(t, err, ErrInvalidAmount, "amount=%d 应拒绝", amt)
	}
}

func TestRechargeAmountNegative(t *testing.T) {
	_, svc := setupSuite(t)
	ctx := ctxT(t)
	actor := Actor{Type: ActorTypeCLI, ID: "bootstrap"}
	for _, amt := range []int64{-1, -100, -9999} {
		_, _, err := svc.Recharge(ctx, actor, RechargeParams{
			AccountID: "x", Amount: amt,
		})
		require.ErrorIs(t, err, ErrInvalidAmount, "amount=%d 应拒绝", amt)
	}
}

func TestRefundAmountNegative(t *testing.T) {
	_, svc := setupSuite(t)
	ctx := ctxT(t)
	actor := Actor{Type: ActorTypeCLI, ID: "bootstrap"}
	for _, amt := range []int64{-1, -100, -9999} {
		_, _, err := svc.Refund(ctx, actor, RefundParams{
			AccountID: "x", Amount: amt, CorrelationID: "c",
		})
		require.ErrorIs(t, err, ErrInvalidAmount, "amount=%d 应拒绝", amt)
	}
}

// =============================================================================
// 压力测试：低竞争 + 高竞争
// =============================================================================

// TestStressLowContention 1000 goroutine × reserve 2，初始 5000 → 全部成功 + 不变量。
//
// 用 -short 跳过；专为单独 `go test -run TestStress... -count=1 -timeout=2m` 运行。
func TestStressLowContention(t *testing.T) {
	if testing.Short() {
		t.Skip("skip stress test in -short mode")
	}
	_, svc := setupSuite(t)
	const (
		accountID = "biz-stress-low"
		recharge  = int64(5000)
		each      = int64(2)
		nGo       = 1000
		// 1000 goroutine 在 nGo > MaxConns 时频繁排队抢同一行，需要更多重试吸收。
		maxRetry = 60
	)
	newAccountWithBalance(t, svc, accountID, recharge)

	var (
		successCount atomic.Int64
		failureCount atomic.Int64
		errs         sync.Map
	)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(nGo)

	for i := 0; i < nGo; i++ {
		go func(idx int) {
			defer wg.Done()
			<-start

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			actor := Actor{Type: ActorTypeTask, ID: fmt.Sprintf("t-%d", idx)}
			_, err := reserveWithRetry(ctx, svc, actor, ReserveParams{
				AccountID: accountID, Amount: each, CorrelationID: fmt.Sprintf("low-%d", idx),
			}, maxRetry)
			if err != nil {
				failureCount.Add(1)
				errs.Store(idx, err.Error())
				return
			}
			successCount.Add(1)
		}(i)
	}
	close(start)
	wg.Wait()

	errs.Range(func(k, v any) bool {
		t.Logf("low-contention goroutine %v err: %v", k, v)
		return true
	})

	t.Logf("low-contention: success=%d failure=%d", successCount.Load(), failureCount.Load())
	require.Equal(t, int64(nGo), successCount.Load(), "低竞争下 1000 笔应全成功")
	require.Equal(t, int64(0), failureCount.Load())

	bal, err := svc.GetBalance(context.Background(), accountID)
	require.NoError(t, err)
	assertInvariant(t, bal)
	require.Equal(t, recharge-int64(nGo)*each, bal.Available)
	require.Equal(t, int64(nGo)*each, bal.Reserved)
}

// TestStressHighContention 100 goroutine × reserve 11，初始 100 → success ≤ 9 + 不变量。
//
// 100/11 = 9 笔上限；剩余必须 ErrInsufficientBalance。
func TestStressHighContention(t *testing.T) {
	if testing.Short() {
		t.Skip("skip stress test in -short mode")
	}
	_, svc := setupSuite(t)
	const (
		accountID = "biz-stress-high"
		recharge  = int64(100)
		each      = int64(11)
		nGo       = 100
		maxRetry  = 10
	)
	newAccountWithBalance(t, svc, accountID, recharge)

	var (
		successCount      atomic.Int64
		insufficientCount atomic.Int64
		otherCount        atomic.Int64
		errs              sync.Map
	)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(nGo)

	for i := 0; i < nGo; i++ {
		go func(idx int) {
			defer wg.Done()
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			actor := Actor{Type: ActorTypeTask, ID: fmt.Sprintf("t-%d", idx)}
			_, err := reserveWithRetry(ctx, svc, actor, ReserveParams{
				AccountID: accountID, Amount: each, CorrelationID: fmt.Sprintf("high-%d", idx),
			}, maxRetry)
			switch {
			case err == nil:
				successCount.Add(1)
			case errors.Is(err, ErrInsufficientBalance):
				insufficientCount.Add(1)
			default:
				otherCount.Add(1)
				errs.Store(idx, err.Error())
			}
		}(i)
	}
	close(start)
	wg.Wait()

	errs.Range(func(k, v any) bool {
		t.Logf("high-contention goroutine %v err: %v", k, v)
		return true
	})

	t.Logf("high-contention: success=%d insufficient=%d other=%d",
		successCount.Load(), insufficientCount.Load(), otherCount.Load())

	require.LessOrEqual(t, successCount.Load(), int64(9),
		"100/11=9 上限；不可超卖")
	require.Equal(t, int64(0), otherCount.Load(), "重试后不应有 version 冲突残留")
	require.Equal(t, int64(nGo), successCount.Load()+insufficientCount.Load()+otherCount.Load())

	bal, err := svc.GetBalance(context.Background(), accountID)
	require.NoError(t, err)
	assertInvariant(t, bal)
	require.GreaterOrEqual(t, bal.Available, int64(0), "余额不可负")
	require.Equal(t, successCount.Load()*each, bal.Reserved, "reserved == success * each")
}

// =============================================================================
// 并发 Reserve + Commit：验证账本 5 字段并发下守恒
// =============================================================================

// TestConcurrentReserveCommit 50 goroutine 各自 Reserve 20 + Commit 18 → used_total=900。
//
// 用 -short 跳过；属端到端集成测试。
func TestConcurrentReserveCommit(t *testing.T) {
	if testing.Short() {
		t.Skip("skip stress test in -short mode")
	}
	_, svc := setupSuite(t)
	const (
		accountID  = "biz-rsv-cm-mix"
		recharge   = int64(2000)
		reserveAmt = int64(20)
		commitAmt  = int64(18)
		nGo        = 50
		// 50 goroutine 各 reserve + commit 共 100 次 CAS 在同一 balance 行；高竞争要给足重试。
		maxRetry = 50
	)
	newAccountWithBalance(t, svc, accountID, recharge)

	var (
		ok      atomic.Int64
		failure atomic.Int64
		errs    sync.Map
	)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(nGo)

	for i := 0; i < nGo; i++ {
		go func(idx int) {
			defer wg.Done()
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			actor := Actor{Type: ActorTypeTask, ID: fmt.Sprintf("t-%d", idx)}
			correlationID := fmt.Sprintf("rc-%d", idx)
			// Reserve（带 retry）
			_, err := reserveWithRetry(ctx, svc, actor, ReserveParams{
				AccountID: accountID, Amount: reserveAmt, CorrelationID: correlationID,
			}, maxRetry)
			if err != nil {
				failure.Add(1)
				errs.Store(idx, fmt.Sprintf("reserve: %v", err))
				return
			}
			// Commit（手写 retry：commit 也可能 version 冲突）
			var commitErr error
			for attempt := 0; attempt < maxRetry; attempt++ {
				_, commitErr = svc.Commit(ctx, actor, CommitParams{
					AccountID: accountID, CorrelationID: correlationID, ActualCost: commitAmt,
				})
				if commitErr == nil {
					break
				}
				if !errors.Is(commitErr, ErrVersionConflict) {
					break
				}
				shift := uint(attempt)
				if shift > 7 {
					shift = 7
				}
				time.Sleep(time.Duration(1<<shift) * time.Millisecond)
			}
			if commitErr != nil {
				failure.Add(1)
				errs.Store(idx, fmt.Sprintf("commit: %v", commitErr))
				return
			}
			ok.Add(1)
		}(i)
	}
	close(start)
	wg.Wait()

	errs.Range(func(k, v any) bool {
		t.Logf("mix goroutine %v err: %v", k, v)
		return true
	})

	t.Logf("rsv+commit: ok=%d failure=%d", ok.Load(), failure.Load())
	require.Equal(t, int64(nGo), ok.Load(), "全部应成功完成 reserve+commit")
	require.Equal(t, int64(0), failure.Load())

	bal, err := svc.GetBalance(context.Background(), accountID)
	require.NoError(t, err)
	assertInvariant(t, bal)
	require.Equal(t, int64(nGo)*commitAmt, bal.UsedTotal, "used_total = 50*18 = 900")
	require.Equal(t, int64(0), bal.Reserved, "全部 commit 完应 reserved=0")
	// 残余 = 2 each * 50 = 100；available = recharge - used = 2000 - 900 = 1100
	require.Equal(t, recharge-bal.UsedTotal, bal.Available)
}

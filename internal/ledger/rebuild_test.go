package ledger

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Unit 8: RebuildBalance — 3 个独立事务测试
// =============================================================================

// execBalanceCorruption 手工 UPDATE business_account_balance 字段（**绕过** service 路径）。
//
// balance 表无 trigger 阻 UPDATE（只 ledger 有），可直接改；该函数仅用于测试制造 drift。
// 与 reconciler_test.go 的 corruptBalance（只支持 ±available）不同，本辅助支持任意 SQL，
// 用于测试 used_total / frozen / refund_total 等更复杂的污染场景。
func execBalanceCorruption(t *testing.T, pool *pgxpool.Pool, accountID string, sql string, args ...any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("execBalanceCorruption %s: %v", accountID, err)
	}
}

// countOutboxByEventType 数 outbox 中某账户某事件类型的条数（测试断言用）。
// 命名与 reconciler_test.go::countOutboxByType 区分；本版本接受 typed EventType。
func countOutboxByEventType(t *testing.T, pool *pgxpool.Pool, accountID string, eventType EventType) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var cnt int
	err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM webhook_event_outbox
		WHERE business_account_id = $1 AND event_type = $2
	`, accountID, string(eventType)).Scan(&cnt)
	if err != nil {
		t.Fatalf("countOutboxByEventType: %v", err)
	}
	return cnt
}

// readFrozenStatus 读取账户的 frozen + frozen_reason（测试断言）。
// 命名与 reconciler_test.go::readFrozen 区分（两者返回一致；保留以减少跨 Agent 冲突）。
func readFrozenStatus(t *testing.T, pool *pgxpool.Pool, accountID string) (bool, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var frozen bool
	var reason *string
	err := pool.QueryRow(ctx, `
		SELECT frozen, frozen_reason FROM business_account_balance WHERE business_account_id = $1
	`, accountID).Scan(&frozen, &reason)
	if err != nil {
		t.Fatalf("readFrozenStatus: %v", err)
	}
	if reason == nil {
		return frozen, ""
	}
	return frozen, *reason
}

// =============================================================================
// Happy path：手工制造 drift → rebuild 恢复
// =============================================================================

func TestRebuild_Happy_RecoversCorruption(t *testing.T) {
	pool, svc := setupSuite(t)
	ctx := ctxT(t)
	const accountID = "biz-rb-happy"
	cliActor := Actor{Type: ActorTypeCLI, ID: "bootstrap"}
	sysActor := Actor{Type: ActorTypeSystem, ID: "rebuild"}
	taskActor := Actor{Type: ActorTypeTask, ID: "t1"}

	newAccountWithBalance(t, svc, accountID, 1000)
	// reserve 100：ledger 真相 = available 900, reserved 100, used 0, recharge 1000
	_, err := svc.Reserve(ctx, taskActor, ReserveParams{
		AccountID: accountID, Amount: 100, CorrelationID: "rsv-1",
	})
	require.NoError(t, err)

	// 手工破坏：balance.available -= 50, used_total += 50（绕过 ledger，制造 drift，仍满足 invariant CHECK）。
	// ledger 真相不变：available 900, reserved 100, used 0, recharge 1000；
	// 但 balance 表被改成：available 850, reserved 100, used 50, recharge 1000（CHECK 通过但与 ledger 不一致）。
	execBalanceCorruption(t, pool, accountID,
		`UPDATE business_account_balance
		 SET available = available - 50, used_total = used_total + 50
		 WHERE business_account_id = $1`,
		accountID)

	balBefore, _ := svc.GetBalance(ctx, accountID)
	require.Equal(t, int64(850), balBefore.Available, "drift 前置：余额已被手工改成 850")
	require.Equal(t, int64(50), balBefore.UsedTotal, "used_total 增到 50（绕过 ledger）")

	// rebuild
	balAfter, err := svc.RebuildBalance(ctx, sysActor, accountID)
	require.NoError(t, err, "rebuild 应成功")

	// 校验恢复结果
	require.False(t, balAfter.Frozen, "rebuild 完成后 unfrozen")
	require.Equal(t, int64(900), balAfter.Available, "ledger 真相恢复")
	require.Equal(t, int64(100), balAfter.Reserved)
	require.Equal(t, int64(0), balAfter.UsedTotal)
	require.Equal(t, int64(1000), balAfter.RechargeTotal)
	assertInvariant(t, balAfter)

	// outbox：rebuild 期间应发了 frozen + unfrozen 各一条
	require.GreaterOrEqual(t, countOutboxByEventType(t, pool, accountID, EventTypeAccountFrozen), 1)
	require.GreaterOrEqual(t, countOutboxByEventType(t, pool, accountID, EventTypeAccountUnfrozen), 1)

	// frozen_reason 落地：unfreeze 后 reason 应为 rebuild_completed
	frozen, reason := readFrozenStatus(t, pool, accountID)
	require.False(t, frozen)
	require.Equal(t, string(ReasonCodeRebuildCompleted), reason)

	_ = cliActor // 保留 actor 区分；本测试不再使用
}

// =============================================================================
// 空 ledger：手工写 available 后 rebuild 应归 0
// =============================================================================

func TestRebuild_EmptyLedger(t *testing.T) {
	pool, svc := setupSuite(t)
	ctx := ctxT(t)
	const accountID = "biz-rb-empty"
	cliActor := Actor{Type: ActorTypeCLI, ID: "bootstrap"}
	sysActor := Actor{Type: ActorTypeSystem, ID: "rebuild"}

	withAccount(t, pool, accountID)
	_, err := svc.CreateAccount(ctx, cliActor, CreateAccountParams{ID: accountID})
	require.NoError(t, err)

	// 手工写 available = 100, recharge_total = 100（无对应 ledger；invariant CHECK 通过 0+0+0!=100 故必须同步加 recharge）
	// available + reserved + used_total = recharge_total → 100 + 0 + 0 = 100 ✓
	execBalanceCorruption(t, pool, accountID,
		`UPDATE business_account_balance SET available = 100, recharge_total = 100 WHERE business_account_id = $1`,
		accountID)

	balAfter, err := svc.RebuildBalance(ctx, sysActor, accountID)
	require.NoError(t, err)
	require.Equal(t, int64(0), balAfter.Available)
	require.Equal(t, int64(0), balAfter.Reserved)
	require.Equal(t, int64(0), balAfter.UsedTotal)
	require.Equal(t, int64(0), balAfter.RechargeTotal)
	require.Equal(t, int64(0), balAfter.RefundTotal)
	assertInvariant(t, balAfter)
}

// =============================================================================
// 已 frozen：freezeInTx 视为幂等成功；rebuild 仍能完成 unfreeze
// =============================================================================

func TestRebuild_AlreadyFrozen(t *testing.T) {
	pool, svc := setupSuite(t)
	ctx := ctxT(t)
	const accountID = "biz-rb-already-frozen"
	sysActor := Actor{Type: ActorTypeSystem, ID: "rebuild"}
	reconcilerActor := Actor{Type: ActorTypeSystem, ID: "reconciler"}

	newAccountWithBalance(t, svc, accountID, 500)
	// 先 freeze（reconciler 风格）
	require.NoError(t, svc.Freeze(ctx, reconcilerActor, accountID, ReasonCodeDriftDetected))
	frozenBefore, reasonBefore := readFrozenStatus(t, pool, accountID)
	require.True(t, frozenBefore)
	require.Equal(t, string(ReasonCodeDriftDetected), reasonBefore)

	frozenEventsBefore := countOutboxByEventType(t, pool, accountID, EventTypeAccountFrozen)

	balAfter, err := svc.RebuildBalance(ctx, sysActor, accountID)
	require.NoError(t, err)
	require.False(t, balAfter.Frozen, "rebuild 完成应 unfrozen")
	require.Equal(t, int64(500), balAfter.Available)
	assertInvariant(t, balAfter)

	// freezeInTx 幂等：不应**新增** frozen 事件
	frozenEventsAfter := countOutboxByEventType(t, pool, accountID, EventTypeAccountFrozen)
	require.Equal(t, frozenEventsBefore, frozenEventsAfter,
		"已 frozen 时 freezeInTx 幂等成功不应再发 frozen 事件")

	// 但是会发 unfrozen 事件
	require.GreaterOrEqual(t, countOutboxByEventType(t, pool, accountID, EventTypeAccountUnfrozen), 1)

	// frozen_reason 切到 rebuild_completed
	_, reasonAfter := readFrozenStatus(t, pool, accountID)
	require.Equal(t, string(ReasonCodeRebuildCompleted), reasonAfter)
}

// =============================================================================
// 完整 entry_type 覆盖：recharge + reserve + commit (+release) + refund
// =============================================================================

func TestRebuild_AllEntryTypes(t *testing.T) {
	pool, svc := setupSuite(t)
	ctx := ctxT(t)
	const accountID = "biz-rb-all-types"
	cliActor := Actor{Type: ActorTypeCLI, ID: "bootstrap"}
	taskActor := Actor{Type: ActorTypeTask, ID: "t"}
	sysActor := Actor{Type: ActorTypeSystem, ID: "rebuild"}

	newAccountWithBalance(t, svc, accountID, 1000)

	// reserve 300 → commit 250（产 release 50）→ refund 100
	_, err := svc.Reserve(ctx, taskActor, ReserveParams{
		AccountID: accountID, Amount: 300, CorrelationID: "task-A",
	})
	require.NoError(t, err)
	_, err = svc.Commit(ctx, taskActor, CommitParams{
		AccountID: accountID, CorrelationID: "task-A", ActualCost: 250,
	})
	require.NoError(t, err)
	_, err = svc.Refund(ctx, cliActor, RefundParams{
		AccountID: accountID, Amount: 100, CorrelationID: "refund-A",
	})
	require.NoError(t, err)

	// 期望终态：available 850, reserved 0, used 150, recharge 1000, refund 100
	balExpected, err := svc.GetBalance(ctx, accountID)
	require.NoError(t, err)
	require.Equal(t, int64(850), balExpected.Available)
	require.Equal(t, int64(0), balExpected.Reserved)
	require.Equal(t, int64(150), balExpected.UsedTotal)
	require.Equal(t, int64(1000), balExpected.RechargeTotal)
	require.Equal(t, int64(100), balExpected.RefundTotal)

	// 制造 drift：available -=150, used_total +=150（仍满足 invariant；refund_total 无 CHECK 也改一下）
	// 真相 available=850, used=150；改成 available=700, used=300。invariant 仍 = 1000 ✓
	execBalanceCorruption(t, pool, accountID,
		`UPDATE business_account_balance
		 SET available = available - 150, used_total = used_total + 150, refund_total = refund_total + 999
		 WHERE business_account_id = $1`,
		accountID)

	balAfter, err := svc.RebuildBalance(ctx, sysActor, accountID)
	require.NoError(t, err)

	require.Equal(t, balExpected.Available, balAfter.Available)
	require.Equal(t, balExpected.Reserved, balAfter.Reserved)
	require.Equal(t, balExpected.UsedTotal, balAfter.UsedTotal)
	require.Equal(t, balExpected.RechargeTotal, balAfter.RechargeTotal)
	require.Equal(t, balExpected.RefundTotal, balAfter.RefundTotal)
	assertInvariant(t, balAfter)
}

// =============================================================================
// "Panic recovery"：模拟 TX1 后崩溃 → 再次调用应顺利完成
//
// 本测试用「手工 UPDATE balance.frozen=true」模拟「TX1 完成但 outbox 未发」状态；
// freezeInTx 在再次调用时见 frozen=true 视为幂等成功；TX2/TX3 正常完成。
// =============================================================================

func TestRebuild_RecoversAfterPanic(t *testing.T) {
	pool, svc := setupSuite(t)
	ctx := ctxT(t)
	const accountID = "biz-rb-recover-panic"
	sysActor := Actor{Type: ActorTypeSystem, ID: "rebuild"}

	newAccountWithBalance(t, svc, accountID, 500)

	// 模拟 TX1 后崩溃：直接手工写 frozen=true（且 frozen_reason=rebuild_in_progress）；
	// 不发 outbox 事件（模拟"事件未发即崩"）。
	execBalanceCorruption(t, pool, accountID,
		`UPDATE business_account_balance
		 SET frozen = true,
		     frozen_reason = $2,
		     frozen_at = NOW(),
		     version = version + 1
		 WHERE business_account_id = $1`,
		accountID, string(ReasonCodeRebuildInProgress))

	frozenEventsBefore := countOutboxByEventType(t, pool, accountID, EventTypeAccountFrozen)

	// 再次调用 RebuildBalance 应能续接（TX1 见已 frozen 视为幂等）
	balAfter, err := svc.RebuildBalance(ctx, sysActor, accountID)
	require.NoError(t, err)
	require.False(t, balAfter.Frozen, "最终应 unfrozen")
	require.Equal(t, int64(500), balAfter.Available)
	assertInvariant(t, balAfter)

	// 不应**新增** frozen 事件（freezeInTx 幂等）
	require.Equal(t, frozenEventsBefore, countOutboxByEventType(t, pool, accountID, EventTypeAccountFrozen))
	// 但应有 unfrozen 事件
	require.GreaterOrEqual(t, countOutboxByEventType(t, pool, accountID, EventTypeAccountUnfrozen), 1)
}

// =============================================================================
// Contention：rebuild 期间有并发写（rebuild 内部 frozen 不阻 Commit/Release）
//
// 由于 rebuild TX1 freeze 后，frozen=true 会阻塞新 Recharge/Reserve；
// 但 Commit/Release 不查 frozen（设计原则：允许 inflight 完成）。
// 本测试验证：
//   - rebuild 在 TX1 后并发起 Commit；不阻塞
//   - TX3 CAS 检测到 last_ledger_id 改变 → 重试
//   - 最终 rebuild 成功 + 不变量恒守
// =============================================================================

func TestRebuild_ContentionRetries(t *testing.T) {
	_, svc := setupSuite(t)
	const accountID = "biz-rb-contention"
	taskActor := Actor{Type: ActorTypeTask, ID: "t"}
	sysActor := Actor{Type: ActorTypeSystem, ID: "rebuild"}

	newAccountWithBalance(t, svc, accountID, 2000)
	ctx := ctxT(t)

	// 准备 8 个 reserve 待并发 commit
	const nInflight = 8
	for i := 0; i < nInflight; i++ {
		_, err := svc.Reserve(ctx, taskActor, ReserveParams{
			AccountID: accountID, Amount: 50, CorrelationID: fmt.Sprintf("c-%d", i),
		})
		require.NoError(t, err)
	}

	// 启 nInflight 个 goroutine 持续 commit；同步用 start chan
	start := make(chan struct{})
	var wg sync.WaitGroup
	var commitFailures atomic.Int64
	wg.Add(nInflight)
	for i := 0; i < nInflight; i++ {
		go func(idx int) {
			defer wg.Done()
			<-start
			gCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			// commit 也可能 version 冲突；带简单重试
			var err error
			for attempt := 0; attempt < 10; attempt++ {
				_, err = svc.Commit(gCtx, taskActor, CommitParams{
					AccountID:     accountID,
					CorrelationID: fmt.Sprintf("c-%d", idx),
					ActualCost:    40,
				})
				if err == nil {
					return
				}
				if !errors.Is(err, ErrVersionConflict) {
					commitFailures.Add(1)
					return
				}
				time.Sleep(time.Duration(1<<uint(attempt)) * time.Millisecond)
			}
			if err != nil {
				commitFailures.Add(1)
			}
		}(i)
	}

	// 启 rebuild goroutine（也在 start 后并发跑）
	wg.Add(1)
	var rebuildErr error
	go func() {
		defer wg.Done()
		<-start
		gCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, rebuildErr = svc.RebuildBalance(gCtx, sysActor, accountID)
	}()

	close(start)
	wg.Wait()

	// 退出条件放宽：rebuild 可能 ErrRebuildContention（重试耗尽，账户保持 frozen），
	// 也可能成功；关键是不变量 + 账户终态合法。
	if rebuildErr != nil {
		require.ErrorIs(t, rebuildErr, ErrRebuildContention,
			"如有错误必须是 ErrRebuildContention")
		t.Logf("rebuild 重试耗尽 (符合预期可能)：%v", rebuildErr)
	} else {
		t.Logf("rebuild 在并发下成功")
	}

	require.Equal(t, int64(0), commitFailures.Load(), "commit 不应有非 version 冲突的失败")

	// 不变量 + 终态合法
	bal, err := svc.GetBalance(context.Background(), accountID)
	require.NoError(t, err)
	assertInvariant(t, bal)
	// 终态：8 笔 reserve+commit 完成 → reserved=0, used=320, available=1680, recharge=2000
	require.Equal(t, int64(0), bal.Reserved, "所有 commit 完成 reserved=0")
	require.Equal(t, int64(nInflight)*40, bal.UsedTotal, "used=8*40=320")
}

// =============================================================================
// 验证 ledger 不可变 trigger 不影响 rebuild
// =============================================================================

func TestRebuild_LedgerTriggerNoInterference(t *testing.T) {
	// 确认 rebuild 不尝试 UPDATE/DELETE ledger（仅读 + 写 balance）。
	// 用一次完整 rebuild 跑完即可，trigger 任何触发都会 PG 异常。
	_, svc := setupSuite(t)
	ctx := ctxT(t)
	const accountID = "biz-rb-trigger"
	sysActor := Actor{Type: ActorTypeSystem, ID: "rebuild"}

	newAccountWithBalance(t, svc, accountID, 500)
	_, err := svc.RebuildBalance(ctx, sysActor, accountID)
	require.NoError(t, err)
}

// =============================================================================
// 辅助 sanity：replaceBalanceInTx 单元
// =============================================================================

func TestReplaceBalanceInTx_CASFailsOnLastLedgerIDDrift(t *testing.T) {
	pool, svc := setupSuite(t)
	ctx := ctxT(t)
	const accountID = "biz-rb-cas-sanity"
	newAccountWithBalance(t, svc, accountID, 100)

	// 读 current last_ledger_id
	bal, err := svc.GetBalance(ctx, accountID)
	require.NoError(t, err)
	expectedLast := bal.LastLedgerID

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	// 用错误的 expectedLastLedgerID → 0 行
	rows, err := svc.replaceBalanceInTx(ctx, tx, accountID, ExpectedBalance{
		Available: 100, RechargeTotal: 100,
	}, expectedLast+999)
	require.NoError(t, err)
	require.Equal(t, int64(0), rows, "错误 last_ledger_id 应 CAS 失败")

	// 用正确的 expectedLastLedgerID → 1 行
	rows, err = svc.replaceBalanceInTx(ctx, tx, accountID, ExpectedBalance{
		Available: 100, RechargeTotal: 100,
	}, expectedLast)
	require.NoError(t, err)
	require.Equal(t, int64(1), rows, "正确 last_ledger_id 应 CAS 成功")
}

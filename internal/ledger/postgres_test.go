package ledger

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// 测试 setup
// =============================================================================

// setupSuite 准备 service；不再 TRUNCATE 全表（避免与其他包 outbox 测试并行时互相打架）。
// 测试用的账户 ID 须以 t.Name() 嵌入 + 自身前缀保持唯一；CreateAccount 失败 (duplicate)
// 说明上次测试遗留，需手工 cleanup（一般 cleanupAccount 自动处理）。
func setupSuite(t *testing.T) (*pgxpool.Pool, *PostgresService) {
	t.Helper()
	pool := mustOpenTestPool(t)
	svc := newTestService(t, pool)
	t.Cleanup(func() {
		pool.Close()
	})
	return pool, svc
}

// withAccount 注册 t.Cleanup 钩子，测试结束时清理指定账户。
// 测试代码模式：
//
//	pool, svc := setupSuite(t)
//	acc := withAccount(t, pool, "biz-rec-1")
//	... 业务调用 ...
func withAccount(t *testing.T, pool *pgxpool.Pool, accountID string) string {
	t.Helper()
	// 先清理上次遗留（如果有）。
	cleanupAccount(t, pool, accountID)
	t.Cleanup(func() {
		cleanupAccount(t, pool, accountID)
	})
	return accountID
}

func ctxT(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return c
}

// helper：清理 + 创建账户 + 可选充值；自动注册 t.Cleanup 清理。
func newAccountWithBalance(t *testing.T, svc *PostgresService, accountID string, recharge int64) {
	t.Helper()
	withAccount(t, svc.pool, accountID)

	actor := Actor{Type: ActorTypeCLI, ID: "bootstrap"}
	ctx := ctxT(t)

	_, err := svc.CreateAccount(ctx, actor, CreateAccountParams{ID: accountID})
	require.NoError(t, err, "CreateAccount")
	if recharge > 0 {
		_, err := svc.Recharge(ctx, actor, RechargeParams{
			AccountID:      accountID,
			Amount:         recharge,
			IdempotencyKey: accountID + ":init",
			CanonicalBody: &RechargeBody{
				AccountID: accountID,
				Amount:    recharge,
			},
		})
		require.NoError(t, err, "Recharge")
	}
}

// =============================================================================
// CreateAccount
// =============================================================================

func TestCreateAccount_Happy(t *testing.T) {
	pool, svc := setupSuite(t)
	ctx := ctxT(t)
	actor := Actor{Type: ActorTypeCLI, ID: "bootstrap"}
	withAccount(t, pool, "biz-create-1")

	acc, err := svc.CreateAccount(ctx, actor, CreateAccountParams{ID: "biz-create-1"})
	require.NoError(t, err)
	require.Equal(t, "biz-create-1", acc.ID)
	require.Equal(t, "active", acc.Status)
	require.False(t, acc.IsolationRequired)

	bal, err := svc.GetBalance(ctx, "biz-create-1")
	require.NoError(t, err)
	require.Equal(t, int64(0), bal.Available)
	require.Equal(t, int64(0), bal.Version)
	assertInvariant(t, bal)
}

func TestCreateAccount_InvalidActor(t *testing.T) {
	_, svc := setupSuite(t)
	ctx := ctxT(t)
	_, err := svc.CreateAccount(ctx, Actor{}, CreateAccountParams{ID: "x"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid actor")
}

func TestCreateAccount_DuplicateID(t *testing.T) {
	pool, svc := setupSuite(t)
	ctx := ctxT(t)
	actor := Actor{Type: ActorTypeCLI, ID: "bootstrap"}
	withAccount(t, pool, "biz-dup")

	_, err := svc.CreateAccount(ctx, actor, CreateAccountParams{ID: "biz-dup"})
	require.NoError(t, err)
	_, err = svc.CreateAccount(ctx, actor, CreateAccountParams{ID: "biz-dup"})
	require.Error(t, err) // UNIQUE 冲突由 PG 报告，service 不映射 sentinel
}

// =============================================================================
// Recharge
// =============================================================================

func TestRecharge_Happy(t *testing.T) {
	_, svc := setupSuite(t)
	ctx := ctxT(t)
	actor := Actor{Type: ActorTypeCLI, ID: "bootstrap"}
	newAccountWithBalance(t, svc, "biz-rec-1", 0)

	entry, err := svc.Recharge(ctx, actor, RechargeParams{
		AccountID:      "biz-rec-1",
		Amount:         1000,
		IdempotencyKey: "k1",
		CanonicalBody:  &RechargeBody{AccountID: "biz-rec-1", Amount: 1000},
	})
	require.NoError(t, err)
	require.NotZero(t, entry.ID)
	require.Equal(t, int64(1000), entry.Amount)

	bal, err := svc.GetBalance(ctx, "biz-rec-1")
	require.NoError(t, err)
	require.Equal(t, int64(1000), bal.Available)
	require.Equal(t, int64(1000), bal.RechargeTotal)
	require.Equal(t, int64(1), bal.Version)
	assertInvariant(t, bal)
}

func TestRecharge_IdempotencySuccess(t *testing.T) {
	_, svc := setupSuite(t)
	ctx := ctxT(t)
	actor := Actor{Type: ActorTypeCLI, ID: "bootstrap"}
	newAccountWithBalance(t, svc, "biz-idem", 0)

	body := &RechargeBody{AccountID: "biz-idem", Amount: 500}
	e1, err := svc.Recharge(ctx, actor, RechargeParams{
		AccountID: "biz-idem", Amount: 500, IdempotencyKey: "ik-1", CanonicalBody: body,
	})
	require.NoError(t, err)
	e2, err := svc.Recharge(ctx, actor, RechargeParams{
		AccountID: "biz-idem", Amount: 500, IdempotencyKey: "ik-1", CanonicalBody: body,
	})
	require.NoError(t, err)
	require.Equal(t, e1.ID, e2.ID, "幂等返原 entry")

	bal, _ := svc.GetBalance(ctx, "biz-idem")
	require.Equal(t, int64(500), bal.Available, "余额不应翻倍")
}

func TestRecharge_IdempotencyConflict(t *testing.T) {
	_, svc := setupSuite(t)
	ctx := ctxT(t)
	actor := Actor{Type: ActorTypeCLI, ID: "bootstrap"}
	newAccountWithBalance(t, svc, "biz-conflict", 0)

	_, err := svc.Recharge(ctx, actor, RechargeParams{
		AccountID: "biz-conflict", Amount: 100, IdempotencyKey: "ik-c",
		CanonicalBody: &RechargeBody{AccountID: "biz-conflict", Amount: 100},
	})
	require.NoError(t, err)
	_, err = svc.Recharge(ctx, actor, RechargeParams{
		AccountID: "biz-conflict", Amount: 200, IdempotencyKey: "ik-c", // 不同 amount → body 不同
		CanonicalBody: &RechargeBody{AccountID: "biz-conflict", Amount: 200},
	})
	require.ErrorIs(t, err, ErrIdempotencyConflict)
}

func TestRecharge_InvalidAmount(t *testing.T) {
	_, svc := setupSuite(t)
	ctx := ctxT(t)
	actor := Actor{Type: ActorTypeCLI, ID: "bootstrap"}
	for _, amt := range []int64{0, -1, -100} {
		_, err := svc.Recharge(ctx, actor, RechargeParams{AccountID: "x", Amount: amt})
		require.ErrorIs(t, err, ErrInvalidAmount, "amount=%d 应拒绝", amt)
	}
}

func TestRecharge_AccountNotFound(t *testing.T) {
	_, svc := setupSuite(t)
	ctx := ctxT(t)
	actor := Actor{Type: ActorTypeCLI, ID: "bootstrap"}
	_, err := svc.Recharge(ctx, actor, RechargeParams{
		AccountID: "ghost", Amount: 100,
	})
	require.ErrorIs(t, err, ErrAccountNotFound)
}

// =============================================================================
// Reserve / Commit / Release / Refund —— 全链路
// =============================================================================

func TestReserve_Happy(t *testing.T) {
	_, svc := setupSuite(t)
	ctx := ctxT(t)
	actor := Actor{Type: ActorTypeTask, ID: "t1"}
	newAccountWithBalance(t, svc, "biz-rsv", 1000)

	entry, err := svc.Reserve(ctx, actor, ReserveParams{
		AccountID: "biz-rsv", Amount: 200, CorrelationID: "corr-1",
	})
	require.NoError(t, err)
	require.Equal(t, int64(200), entry.Amount)

	bal, _ := svc.GetBalance(ctx, "biz-rsv")
	require.Equal(t, int64(800), bal.Available)
	require.Equal(t, int64(200), bal.Reserved)
	assertInvariant(t, bal)
}

func TestReserve_InsufficientBalance(t *testing.T) {
	_, svc := setupSuite(t)
	ctx := ctxT(t)
	actor := Actor{Type: ActorTypeTask, ID: "t1"}
	newAccountWithBalance(t, svc, "biz-low", 50)

	_, err := svc.Reserve(ctx, actor, ReserveParams{
		AccountID: "biz-low", Amount: 100, CorrelationID: "c",
	})
	require.ErrorIs(t, err, ErrInsufficientBalance)
}

func TestReserve_ExactBalance(t *testing.T) {
	_, svc := setupSuite(t)
	ctx := ctxT(t)
	actor := Actor{Type: ActorTypeTask, ID: "t1"}
	newAccountWithBalance(t, svc, "biz-exact", 100)
	_, err := svc.Reserve(ctx, actor, ReserveParams{
		AccountID: "biz-exact", Amount: 100, CorrelationID: "c",
	})
	require.NoError(t, err, "余额恰好够应允许")
	bal, _ := svc.GetBalance(ctx, "biz-exact")
	require.Equal(t, int64(0), bal.Available)
	require.Equal(t, int64(100), bal.Reserved)
	assertInvariant(t, bal)
}

func TestReserve_Idempotent(t *testing.T) {
	_, svc := setupSuite(t)
	ctx := ctxT(t)
	actor := Actor{Type: ActorTypeTask, ID: "t1"}
	newAccountWithBalance(t, svc, "biz-rsv-idem", 1000)
	e1, err := svc.Reserve(ctx, actor, ReserveParams{
		AccountID: "biz-rsv-idem", Amount: 50, CorrelationID: "same-c",
	})
	require.NoError(t, err)
	e2, err := svc.Reserve(ctx, actor, ReserveParams{
		AccountID: "biz-rsv-idem", Amount: 50, CorrelationID: "same-c",
	})
	require.NoError(t, err)
	require.Equal(t, e1.ID, e2.ID)
	bal, _ := svc.GetBalance(ctx, "biz-rsv-idem")
	require.Equal(t, int64(950), bal.Available, "幂等 reserve 不应重复扣")
}

func TestCommit_HappyWithRelease(t *testing.T) {
	_, svc := setupSuite(t)
	ctx := ctxT(t)
	actor := Actor{Type: ActorTypeTask, ID: "t1"}
	newAccountWithBalance(t, svc, "biz-cm", 1000)

	_, err := svc.Reserve(ctx, actor, ReserveParams{
		AccountID: "biz-cm", Amount: 100, CorrelationID: "c1",
	})
	require.NoError(t, err)

	entries, err := svc.Commit(ctx, actor, CommitParams{
		AccountID: "biz-cm", CorrelationID: "c1", ActualCost: 80,
	})
	require.NoError(t, err)
	require.Len(t, entries, 2, "expect commit + release entries")
	require.Equal(t, int64(80), entries[0].Amount, "commit amount")
	require.Equal(t, int64(20), entries[1].Amount, "release amount")

	bal, _ := svc.GetBalance(ctx, "biz-cm")
	require.Equal(t, int64(920), bal.Available)
	require.Equal(t, int64(0), bal.Reserved)
	require.Equal(t, int64(80), bal.UsedTotal)
	assertInvariant(t, bal)
}

func TestCommit_HappyExact(t *testing.T) {
	_, svc := setupSuite(t)
	ctx := ctxT(t)
	actor := Actor{Type: ActorTypeTask, ID: "t1"}
	newAccountWithBalance(t, svc, "biz-cm-exact", 500)

	_, err := svc.Reserve(ctx, actor, ReserveParams{
		AccountID: "biz-cm-exact", Amount: 100, CorrelationID: "c",
	})
	require.NoError(t, err)

	entries, err := svc.Commit(ctx, actor, CommitParams{
		AccountID: "biz-cm-exact", CorrelationID: "c", ActualCost: 100,
	})
	require.NoError(t, err)
	require.Len(t, entries, 1, "actualCost==reserve 不产 release entry")
	bal, _ := svc.GetBalance(ctx, "biz-cm-exact")
	require.Equal(t, int64(400), bal.Available)
	require.Equal(t, int64(100), bal.UsedTotal)
	require.Equal(t, int64(0), bal.Reserved)
	assertInvariant(t, bal)
}

func TestCommit_ExceedsReserved(t *testing.T) {
	_, svc := setupSuite(t)
	ctx := ctxT(t)
	actor := Actor{Type: ActorTypeTask, ID: "t1"}
	newAccountWithBalance(t, svc, "biz-cm-over", 1000)
	_, err := svc.Reserve(ctx, actor, ReserveParams{
		AccountID: "biz-cm-over", Amount: 100, CorrelationID: "c",
	})
	require.NoError(t, err)
	_, err = svc.Commit(ctx, actor, CommitParams{
		AccountID: "biz-cm-over", CorrelationID: "c", ActualCost: 120,
	})
	require.ErrorIs(t, err, ErrCommitExceedsReserved)
}

func TestCommit_ReserveNotFound(t *testing.T) {
	_, svc := setupSuite(t)
	ctx := ctxT(t)
	actor := Actor{Type: ActorTypeTask, ID: "t1"}
	newAccountWithBalance(t, svc, "biz-cm-nr", 100)
	_, err := svc.Commit(ctx, actor, CommitParams{
		AccountID: "biz-cm-nr", CorrelationID: "ghost", ActualCost: 10,
	})
	require.ErrorIs(t, err, ErrReserveNotFound)
}

func TestCommit_AlreadySettled(t *testing.T) {
	_, svc := setupSuite(t)
	ctx := ctxT(t)
	actor := Actor{Type: ActorTypeTask, ID: "t1"}
	newAccountWithBalance(t, svc, "biz-cm-as", 1000)
	_, err := svc.Reserve(ctx, actor, ReserveParams{
		AccountID: "biz-cm-as", Amount: 100, CorrelationID: "c",
	})
	require.NoError(t, err)
	_, err = svc.Commit(ctx, actor, CommitParams{
		AccountID: "biz-cm-as", CorrelationID: "c", ActualCost: 80,
	})
	require.NoError(t, err)
	// 第二次 commit 同 correlation：幂等返原 entry（first 个 commit），不报错。
	entries, err := svc.Commit(ctx, actor, CommitParams{
		AccountID: "biz-cm-as", CorrelationID: "c", ActualCost: 80,
	})
	require.NoError(t, err)
	require.Len(t, entries, 1, "幂等返原 commit entry")
	// 然后另起一个 correlation_id 模拟其他业务试图 commit 同一个不存在的 reserve → ErrReserveNotFound
}

func TestRelease_Happy(t *testing.T) {
	_, svc := setupSuite(t)
	ctx := ctxT(t)
	actor := Actor{Type: ActorTypeTask, ID: "t1"}
	newAccountWithBalance(t, svc, "biz-rls", 1000)
	_, err := svc.Reserve(ctx, actor, ReserveParams{
		AccountID: "biz-rls", Amount: 100, CorrelationID: "c",
	})
	require.NoError(t, err)
	_, err = svc.Release(ctx, actor, ReleaseParams{
		AccountID: "biz-rls", Amount: 100, CorrelationID: "c",
	})
	require.NoError(t, err)

	bal, _ := svc.GetBalance(ctx, "biz-rls")
	require.Equal(t, int64(1000), bal.Available)
	require.Equal(t, int64(0), bal.Reserved)
	assertInvariant(t, bal)
}

func TestRefund_Happy(t *testing.T) {
	_, svc := setupSuite(t)
	ctx := ctxT(t)
	actor := Actor{Type: ActorTypeTask, ID: "t1"}
	cliActor := Actor{Type: ActorTypeCLI, ID: "bootstrap"}
	newAccountWithBalance(t, svc, "biz-rf", 1000)

	// 先做完整 reserve → commit 流程产生 used_total
	_, err := svc.Reserve(ctx, actor, ReserveParams{AccountID: "biz-rf", Amount: 200, CorrelationID: "r1"})
	require.NoError(t, err)
	_, err = svc.Commit(ctx, actor, CommitParams{AccountID: "biz-rf", CorrelationID: "r1", ActualCost: 200})
	require.NoError(t, err)

	// 退款 100
	_, err = svc.Refund(ctx, cliActor, RefundParams{
		AccountID: "biz-rf", Amount: 100, CorrelationID: "refund-1",
		ReferenceType: "manual", ReferenceID: "support-ticket-1",
	})
	require.NoError(t, err)

	bal, _ := svc.GetBalance(ctx, "biz-rf")
	require.Equal(t, int64(900), bal.Available, "800 + 100 refund")
	require.Equal(t, int64(100), bal.UsedTotal, "200 - 100")
	require.Equal(t, int64(100), bal.RefundTotal)
	assertInvariant(t, bal)
}

func TestRefund_InsufficientUsed(t *testing.T) {
	_, svc := setupSuite(t)
	ctx := ctxT(t)
	cliActor := Actor{Type: ActorTypeCLI, ID: "bootstrap"}
	newAccountWithBalance(t, svc, "biz-rf-ne", 1000)
	_, err := svc.Refund(ctx, cliActor, RefundParams{
		AccountID: "biz-rf-ne", Amount: 100, CorrelationID: "c",
	})
	require.ErrorIs(t, err, ErrInsufficientUsed)
}

// =============================================================================
// Freeze / Unfreeze
// =============================================================================

func TestFreezeUnfreeze_Lifecycle(t *testing.T) {
	_, svc := setupSuite(t)
	ctx := ctxT(t)
	actor := Actor{Type: ActorTypeCLI, ID: "bootstrap"}
	sysActor := Actor{Type: ActorTypeSystem, ID: "reconciler"}
	newAccountWithBalance(t, svc, "biz-fz", 1000)

	require.NoError(t, svc.Freeze(ctx, sysActor, "biz-fz", ReasonCodeDriftDetected))

	bal, _ := svc.GetBalance(ctx, "biz-fz")
	require.True(t, bal.Frozen)
	require.Equal(t, string(ReasonCodeDriftDetected), bal.FrozenReason)

	// frozen 下 Recharge 拒绝
	_, err := svc.Recharge(ctx, actor, RechargeParams{
		AccountID: "biz-fz", Amount: 100, IdempotencyKey: "x",
		CanonicalBody: &RechargeBody{AccountID: "biz-fz", Amount: 100},
	})
	require.ErrorIs(t, err, ErrAccountFrozen)

	// frozen 下 Reserve 拒绝
	_, err = svc.Reserve(ctx, actor, ReserveParams{
		AccountID: "biz-fz", Amount: 100, CorrelationID: "rsv",
	})
	require.ErrorIs(t, err, ErrAccountFrozen)

	// 解冻
	require.NoError(t, svc.Unfreeze(ctx, sysActor, "biz-fz", ReasonCodeManualUnfreeze))
	bal, _ = svc.GetBalance(ctx, "biz-fz")
	require.False(t, bal.Frozen)

	// 解冻后 Recharge 成功
	_, err = svc.Recharge(ctx, actor, RechargeParams{
		AccountID: "biz-fz", Amount: 100, IdempotencyKey: "y",
		CanonicalBody: &RechargeBody{AccountID: "biz-fz", Amount: 100},
	})
	require.NoError(t, err)
}

func TestFreeze_Idempotent(t *testing.T) {
	_, svc := setupSuite(t)
	ctx := ctxT(t)
	sysActor := Actor{Type: ActorTypeSystem, ID: "reconciler"}
	newAccountWithBalance(t, svc, "biz-fz-i", 0)
	require.NoError(t, svc.Freeze(ctx, sysActor, "biz-fz-i", ReasonCodeDriftDetected))
	require.NoError(t, svc.Freeze(ctx, sysActor, "biz-fz-i", ReasonCodeDriftDetected)) // 第二次幂等
	bal, _ := svc.GetBalance(ctx, "biz-fz-i")
	require.True(t, bal.Frozen)
}

func TestFrozenAccount_CommitStillWorks(t *testing.T) {
	// 设计原则：frozen 不阻 Commit/Release（允许 inflight 完成）。
	_, svc := setupSuite(t)
	ctx := ctxT(t)
	actor := Actor{Type: ActorTypeTask, ID: "t1"}
	sysActor := Actor{Type: ActorTypeSystem, ID: "reconciler"}
	newAccountWithBalance(t, svc, "biz-fz-cm", 500)

	// 先 reserve
	_, err := svc.Reserve(ctx, actor, ReserveParams{AccountID: "biz-fz-cm", Amount: 100, CorrelationID: "c"})
	require.NoError(t, err)
	// 再 freeze
	require.NoError(t, svc.Freeze(ctx, sysActor, "biz-fz-cm", ReasonCodeManualFreeze))
	// Commit 仍能完成
	_, err = svc.Commit(ctx, actor, CommitParams{AccountID: "biz-fz-cm", CorrelationID: "c", ActualCost: 80})
	require.NoError(t, err, "frozen 不阻 Commit（inflight 完成）")

	bal, _ := svc.GetBalance(ctx, "biz-fz-cm")
	require.True(t, bal.Frozen)
	require.Equal(t, int64(80), bal.UsedTotal)
	assertInvariant(t, bal)
}

// =============================================================================
// 全链路 integration
// =============================================================================

func TestFullLifecycle_RechargeReserveCommitRefund(t *testing.T) {
	pool, svc := setupSuite(t)
	ctx := ctxT(t)
	cliActor := Actor{Type: ActorTypeCLI, ID: "bootstrap"}
	taskActor := Actor{Type: ActorTypeTask, ID: "task-1"}
	const acc = "biz-full"
	withAccount(t, pool, acc)

	_, err := svc.CreateAccount(ctx, cliActor, CreateAccountParams{ID: acc})
	require.NoError(t, err)
	assertInvariantDB(t, svc, acc)

	_, err = svc.Recharge(ctx, cliActor, RechargeParams{
		AccountID: acc, Amount: 1000, IdempotencyKey: "topup-1",
		CanonicalBody: &RechargeBody{AccountID: acc, Amount: 1000},
	})
	require.NoError(t, err)
	assertInvariantDB(t, svc, acc)

	_, err = svc.Reserve(ctx, taskActor, ReserveParams{
		AccountID: acc, Amount: 300, CorrelationID: "task-1",
	})
	require.NoError(t, err)
	assertInvariantDB(t, svc, acc)

	_, err = svc.Commit(ctx, taskActor, CommitParams{
		AccountID: acc, CorrelationID: "task-1", ActualCost: 250,
	})
	require.NoError(t, err)
	assertInvariantDB(t, svc, acc)

	_, err = svc.Refund(ctx, cliActor, RefundParams{
		AccountID: acc, Amount: 50, CorrelationID: "refund-A",
	})
	require.NoError(t, err)

	bal, _ := svc.GetBalance(ctx, acc)
	require.Equal(t, int64(800), bal.Available)
	require.Equal(t, int64(0), bal.Reserved)
	require.Equal(t, int64(200), bal.UsedTotal, "250 - 50 refund")
	require.Equal(t, int64(50), bal.RefundTotal)
	assertInvariant(t, bal)
}

// =============================================================================
// 错误路径
// =============================================================================

func TestReserve_InvalidAmount(t *testing.T) {
	_, svc := setupSuite(t)
	ctx := ctxT(t)
	actor := Actor{Type: ActorTypeTask, ID: "t1"}
	_, err := svc.Reserve(ctx, actor, ReserveParams{AccountID: "x", Amount: 0, CorrelationID: "c"})
	require.ErrorIs(t, err, ErrInvalidAmount)
	_, err = svc.Reserve(ctx, actor, ReserveParams{AccountID: "x", Amount: -1, CorrelationID: "c"})
	require.ErrorIs(t, err, ErrInvalidAmount)
}

func TestCommit_InvalidAmount(t *testing.T) {
	_, svc := setupSuite(t)
	ctx := ctxT(t)
	actor := Actor{Type: ActorTypeTask, ID: "t1"}
	_, err := svc.Commit(ctx, actor, CommitParams{AccountID: "x", CorrelationID: "c", ActualCost: 0})
	require.ErrorIs(t, err, ErrInvalidAmount)
}

func TestRefund_InvalidAmount(t *testing.T) {
	_, svc := setupSuite(t)
	ctx := ctxT(t)
	actor := Actor{Type: ActorTypeCLI, ID: "bootstrap"}
	_, err := svc.Refund(ctx, actor, RefundParams{AccountID: "x", Amount: -1, CorrelationID: "c"})
	require.ErrorIs(t, err, ErrInvalidAmount)
}

func TestGetBalance_NotFound(t *testing.T) {
	_, svc := setupSuite(t)
	ctx := ctxT(t)
	_, err := svc.GetBalance(ctx, "ghost")
	require.ErrorIs(t, err, ErrAccountNotFound)
}

// RebuildBalance 已在 Unit 8 实装；具体功能测试见 rebuild_test.go。
// 此处保留入口校验类的快速单测。
func TestRebuildBalance_InvalidActor(t *testing.T) {
	_, svc := setupSuite(t)
	ctx := ctxT(t)
	_, err := svc.RebuildBalance(ctx, Actor{}, "x")
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid actor")
}

func TestRebuildBalance_EmptyAccountID(t *testing.T) {
	_, svc := setupSuite(t)
	ctx := ctxT(t)
	actor := Actor{Type: ActorTypeSystem, ID: "rebuild"}
	_, err := svc.RebuildBalance(ctx, actor, "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "accountID 不能为空")
}

// =============================================================================
// Outbox 失败回滚
// =============================================================================

func TestRecharge_OutboxFailureRollsBackEverything(t *testing.T) {
	pool := mustOpenTestPool(t)
	t.Cleanup(pool.Close)

	outbox := &testOutbox{}
	logger := newSilentLogger()
	svc := NewPostgresService(pool, outbox, logger)
	withAccount(t, pool, "biz-ob-fail")

	cliActor := Actor{Type: ActorTypeCLI, ID: "bootstrap"}
	ctx := ctxT(t)

	_, err := svc.CreateAccount(ctx, cliActor, CreateAccountParams{ID: "biz-ob-fail"})
	require.NoError(t, err)

	// 注入 outbox 失败
	outbox.failNext = true
	_, err = svc.Recharge(ctx, cliActor, RechargeParams{
		AccountID: "biz-ob-fail", Amount: 100, IdempotencyKey: "k",
		CanonicalBody: &RechargeBody{AccountID: "biz-ob-fail", Amount: 100},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "outbox.Publish 失败")

	// 整事务回滚：balance 没变
	bal, _ := svc.GetBalance(ctx, "biz-ob-fail")
	require.Equal(t, int64(0), bal.Available)
	require.Equal(t, int64(0), bal.RechargeTotal)

	// ledger 也没增量（不可变 trigger 不会影响 — 是 tx rollback 让 INSERT 不入库）
	// 通过 SumLedgerDeltas 确认
	sum, _ := svc.queries.SumLedgerDeltasByAccount(ctx, "biz-ob-fail")
	require.Equal(t, int64(0), sum.AvailableSum)
	require.Equal(t, int64(0), sum.RechargeSum)
}

// =============================================================================
// 杂项
// =============================================================================

func TestRechargeWithoutIdempotencyKey(t *testing.T) {
	_, svc := setupSuite(t)
	ctx := ctxT(t)
	actor := Actor{Type: ActorTypeCLI, ID: "bootstrap"}
	newAccountWithBalance(t, svc, "biz-no-idem", 0)
	_, err := svc.Recharge(ctx, actor, RechargeParams{AccountID: "biz-no-idem", Amount: 100})
	require.NoError(t, err)
	_, err = svc.Recharge(ctx, actor, RechargeParams{AccountID: "biz-no-idem", Amount: 100})
	require.NoError(t, err, "无 idempotency_key 时允许重复充值")
	bal, _ := svc.GetBalance(ctx, "biz-no-idem")
	require.Equal(t, int64(200), bal.Available)
	assertInvariant(t, bal)
}

func TestCanonicalizeBody_Deterministic(t *testing.T) {
	b1 := &RechargeBody{AccountID: "a", Amount: 100, ExternalRef: "ref-1"}
	b2 := &RechargeBody{AccountID: "a", Amount: 100, ExternalRef: "ref-1"}
	b3 := &RechargeBody{AccountID: "a", Amount: 101, ExternalRef: "ref-1"}

	h1, err := canonicalizeBody(b1)
	require.NoError(t, err)
	h2, err := canonicalizeBody(b2)
	require.NoError(t, err)
	h3, err := canonicalizeBody(b3)
	require.NoError(t, err)

	require.Len(t, h1, 32)
	require.Equal(t, h1, h2, "相同内容应得到相同 sha256")
	require.NotEqual(t, h1, h3, "不同内容应不同")
}

func TestErrors_AreSentinel(t *testing.T) {
	// 确保 sentinel 与 errors.Is 兼容
	wrapped := errors.New("foo: " + ErrInsufficientBalance.Error())
	require.False(t, errors.Is(wrapped, ErrInsufficientBalance), "纯字符串 wrap 不应匹配")
}

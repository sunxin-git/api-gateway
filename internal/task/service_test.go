package task

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sunxin-git/api-gateway/internal/db"
	"github.com/sunxin-git/api-gateway/internal/ledger"
	"github.com/sunxin-git/api-gateway/internal/relay/video"
)

// TestSubmit_HappyReserveClaimInsert：提交原子性——reserve + claim + 落 task 全成。
func TestSubmit_HappyReserveClaimInsert(t *testing.T) {
	s := setupTaskSuite(t)
	s.seedAccount(t, 100_000)

	taskID, err := s.svc.Submit(context.Background(), s.submitParams(1000))
	require.NoError(t, err)
	require.NotEmpty(t, taskID)

	tk := s.getTask(t, taskID)
	assert.Equal(t, db.TaskStatusSUBMITTED, tk.Status)
	assert.Equal(t, s.accountID, tk.BusinessAccountID)

	b := s.balance(t)
	assert.Equal(t, int64(1000), b.Reserved, "reserve 已预占")
	assert.Equal(t, int64(99_000), b.Available)
	assertInvariant(t, b)

	assert.Equal(t, int32(1), s.inflight(t), "claim 占位 inflight=1")
	assert.Equal(t, []string{taskID}, s.enq.submits, "已入队 submit")
}

// TestSubmit_InsufficientBalance：余额不足 → reserve 失败，无 task/claim/reserve 泄露。
func TestSubmit_InsufficientBalance(t *testing.T) {
	s := setupTaskSuite(t)
	s.seedAccount(t, 500) // < reserve 1000

	_, err := s.svc.Submit(context.Background(), s.submitParams(1000))
	require.Error(t, err)
	assert.ErrorIs(t, err, ledger.ErrInsufficientBalance, "应保留 ledger sentinel 供 handler 映射 402")

	b := s.balance(t)
	assert.Equal(t, int64(0), b.Reserved)
	assert.Equal(t, int32(0), s.inflight(t), "未占 claim")
	assert.Empty(t, s.enq.submits)
	assertInvariant(t, b)
}

// TestSubmit_ConcurrencyLimit_ReservesReleased：claim 占满 → 429，且该次 reserve 已回退（无 orphan）。
func TestSubmit_ConcurrencyLimit_ReservesReleased(t *testing.T) {
	s := setupTaskSuite(t)
	s.svc.cap = 1 // cap=1
	s.seedAccount(t, 100_000)

	// 第 1 次占满
	_, err := s.svc.Submit(context.Background(), s.submitParams(1000))
	require.NoError(t, err)

	// 第 2 次：claim 占不到 → ErrConcurrencyLimit；其 reserve 须已 Release
	_, err = s.svc.Submit(context.Background(), s.submitParams(1000))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrConcurrencyLimit)

	b := s.balance(t)
	assert.Equal(t, int64(1000), b.Reserved, "仅第 1 次的 reserve 在账；第 2 次已回退，无 orphan")
	assert.Equal(t, int32(1), s.inflight(t))
	assertInvariant(t, b)
}

// TestForwardFlow_EndToEnd：提交→submit worker→上游终态→settle→SETTLED 全链路（含 claim 释放 + 差额 release）。
func TestForwardFlow_EndToEnd(t *testing.T) {
	s := setupTaskSuite(t)
	s.seedAccount(t, 100_000)
	s.adapter.submitFn = func(_ context.Context, _ *video.VideoModelEntry, _ video.UpstreamCredentials, _ *video.ValidatedRequest, _ string) (string, error) {
		return "cgt-e2e", nil
	}
	s.adapter.pollFn = func(_ context.Context, _ *video.VideoModelEntry, _ video.UpstreamCredentials, _ string) (*video.PollResult, error) {
		return &video.PollResult{Status: video.UpstreamSucceeded, Usage: &video.UpstreamUsage{CompletionTokens: 100_000}}, nil
	}
	ctx := context.Background()

	// 1. 提交（带回调 token，验证终态置空）
	p := s.submitParams(1000)
	p.CallbackToken = "cb-tok-e2e"
	taskID, err := s.svc.Submit(ctx, p)
	require.NoError(t, err)

	// 2. submit worker → UPSTREAM_SUBMITTED（含 upstream_task_id + upstream_submitted_at）
	require.NoError(t, s.svc.handleSubmit(ctx, taskID))
	tk := s.getTask(t, taskID)
	require.Equal(t, db.TaskStatusUPSTREAMSUBMITTED, tk.Status)
	require.Equal(t, "cgt-e2e", tk.UpstreamTaskID.String)
	assert.True(t, tk.UpstreamSubmittedAt.Valid, "应写 upstream_submitted_at")

	// 3. 上游终态 COMPLETED（释放 claim + 入队 settle + 置空 callback_token + 写 terminal_at）
	won, err := s.svc.markUpstreamTerminal(ctx, taskID, db.TaskStatusUPSTREAMSUBMITTED, db.TaskStatusCOMPLETED, "", "", s.accountID, "gw-video")
	require.NoError(t, err)
	require.True(t, won)
	assert.Equal(t, int32(0), s.inflight(t), "进上游终态释放 claim")
	tk = s.getTask(t, taskID)
	assert.False(t, tk.CallbackToken.Valid, "终态置空 callback_token（防 token 滥用）")
	assert.True(t, tk.TerminalAt.Valid, "应写 terminal_at")
	assert.False(t, tk.ErrorCode.Valid, "COMPLETED 不写 error_code")

	// 4. settle → commit 实际 600（reserve 1000，差额 400 自动 release）→ SETTLED
	require.NoError(t, s.svc.settleTask(ctx, taskID))
	require.Equal(t, db.TaskStatusSETTLED, s.getTask(t, taskID).Status)

	b := s.balance(t)
	assert.Equal(t, int64(600), b.UsedTotal, "commit 真实 usage 600")
	assert.Equal(t, int64(0), b.Reserved, "差额 400 已 release")
	assert.Equal(t, int64(99_400), b.Available)
	assertInvariant(t, b)
}

// TestSubmitWorker_ConcurrentCAS_OnlyOneSubmits：N 并发 handleSubmit 同一 task，仅一个 CAS 成功调上游。
func TestSubmitWorker_ConcurrentCAS_OnlyOneSubmits(t *testing.T) {
	s := setupTaskSuite(t)
	s.seedAccount(t, 100_000)
	taskID, err := s.svc.Submit(context.Background(), s.submitParams(1000))
	require.NoError(t, err)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = s.svc.handleSubmit(context.Background(), taskID) }()
	}
	wg.Wait()

	assert.Equal(t, int64(1), s.adapter.submitCount(), "CAS 保证仅一个 worker 调上游 Submit（不双提交）")
	assert.Equal(t, db.TaskStatusUPSTREAMSUBMITTED, s.getTask(t, taskID).Status)
}

// TestSubmitWorker_Reentrant_NoOp：重复 handleSubmit（重投/重试）幂等放弃，不重复提交。
func TestSubmitWorker_Reentrant_NoOp(t *testing.T) {
	s := setupTaskSuite(t)
	s.seedAccount(t, 100_000)
	taskID, err := s.svc.Submit(context.Background(), s.submitParams(1000))
	require.NoError(t, err)

	require.NoError(t, s.svc.handleSubmit(context.Background(), taskID))
	require.NoError(t, s.svc.handleSubmit(context.Background(), taskID)) // 第二次：状态非 SUBMITTED → no-op
	assert.Equal(t, int64(1), s.adapter.submitCount())
}

// TestSubmitWorker_UpstreamRejected_Failed：上游明确拒绝 → fail-closed FAILED + 入队 settle。
func TestSubmitWorker_UpstreamRejected_Failed(t *testing.T) {
	s := setupTaskSuite(t)
	s.seedAccount(t, 100_000)
	s.adapter.submitFn = func(_ context.Context, _ *video.VideoModelEntry, _ video.UpstreamCredentials, _ *video.ValidatedRequest, _ string) (string, error) {
		return "", video.ErrUpstreamRejected
	}
	taskID, err := s.svc.Submit(context.Background(), s.submitParams(1000))
	require.NoError(t, err)

	require.NoError(t, s.svc.handleSubmit(context.Background(), taskID))
	tk := s.getTask(t, taskID)
	assert.Equal(t, db.TaskStatusFAILED, tk.Status)
	assert.Equal(t, "upstream_rejected", tk.ErrorCode.String)
	assert.Equal(t, int32(0), s.inflight(t), "FAILED 释放 claim")
	assert.GreaterOrEqual(t, s.enq.settleCount(), 1, "FAILED 入队 settle 以 release reserve")
}

// TestSubmitWorker_TransientError_LeavesSubmitting：瞬时错误不标 FAILED、不重投（防双扣），留 UPSTREAM_SUBMITTING。
func TestSubmitWorker_TransientError_LeavesSubmitting(t *testing.T) {
	s := setupTaskSuite(t)
	s.seedAccount(t, 100_000)
	s.adapter.submitFn = func(_ context.Context, _ *video.VideoModelEntry, _ video.UpstreamCredentials, _ *video.ValidatedRequest, _ string) (string, error) {
		return "", video.ErrUpstreamTimeout
	}
	taskID, err := s.svc.Submit(context.Background(), s.submitParams(1000))
	require.NoError(t, err)

	require.NoError(t, s.svc.handleSubmit(context.Background(), taskID), "瞬时错误返 nil（不让 Asynq 重投 → 防双扣）")
	tk := s.getTask(t, taskID)
	assert.Equal(t, db.TaskStatusUPSTREAMSUBMITTING, tk.Status, "保留 UPSTREAM_SUBMITTING 待 recover fail-closed")
	assert.Equal(t, int32(1), s.inflight(t), "claim 仍持有（未进终态）")
}

// TestSettle_MissingUsage_SettleFailed：COMPLETED 但缺 usage → SETTLE_FAILED，reserve 留对账（不 commit/不 release）。
func TestSettle_MissingUsage_SettleFailed(t *testing.T) {
	s := setupTaskSuite(t)
	s.seedAccount(t, 100_000)
	s.adapter.pollFn = func(_ context.Context, _ *video.VideoModelEntry, _ video.UpstreamCredentials, _ string) (*video.PollResult, error) {
		return &video.PollResult{Status: video.UpstreamSucceeded, Usage: nil}, nil // 缺 usage
	}
	taskID, err := s.svc.Submit(context.Background(), s.submitParams(1000))
	require.NoError(t, err)
	s.directSetStatus(t, taskID, db.TaskStatusCOMPLETED, "cgt-1")

	require.NoError(t, s.svc.settleTask(context.Background(), taskID))
	assert.Equal(t, db.TaskStatusSETTLEFAILED, s.getTask(t, taskID).Status)

	b := s.balance(t)
	assert.Equal(t, int64(1000), b.Reserved, "缺 usage 不 commit/不 release，reserve 留对账")
	assert.Equal(t, int64(0), b.UsedTotal)
	assertInvariant(t, b)
}

// TestSettle_FailedTerminal_Release：FAILED 终态 → release 全额回退 → SETTLED。
func TestSettle_FailedTerminal_Release(t *testing.T) {
	s := setupTaskSuite(t)
	s.seedAccount(t, 100_000)
	taskID, err := s.svc.Submit(context.Background(), s.submitParams(1000))
	require.NoError(t, err)
	s.directSetStatus(t, taskID, db.TaskStatusFAILED, "")

	require.NoError(t, s.svc.settleTask(context.Background(), taskID))
	assert.Equal(t, db.TaskStatusSETTLED, s.getTask(t, taskID).Status)

	b := s.balance(t)
	assert.Equal(t, int64(0), b.Reserved, "失败 release 全额回退")
	assert.Equal(t, int64(0), b.UsedTotal)
	assert.Equal(t, int64(100_000), b.Available)
	assertInvariant(t, b)
}

// TestSettle_Reentrant_Idempotent：重复 settle 幂等（已 SETTLED → no-op，不二次扣账）。
func TestSettle_Reentrant_Idempotent(t *testing.T) {
	s := setupTaskSuite(t)
	s.seedAccount(t, 100_000)
	taskID, err := s.svc.Submit(context.Background(), s.submitParams(1000))
	require.NoError(t, err)
	s.directSetStatus(t, taskID, db.TaskStatusFAILED, "")

	require.NoError(t, s.svc.settleTask(context.Background(), taskID))
	require.NoError(t, s.svc.settleTask(context.Background(), taskID)) // 第二次：已 SETTLED → no-op
	assert.Equal(t, db.TaskStatusSETTLED, s.getTask(t, taskID).Status)

	b := s.balance(t)
	assert.Equal(t, int64(100_000), b.Available, "幂等：未二次回退/扣账")
	assertInvariant(t, b)
}

// TestSubmit_ConcurrentClaim_NoOverbooking：N 并发提交同账户×模型，恰好 cap 个成功（TOCTOU 原子 claim），其余 429 且 reserve 回退无 orphan。
func TestSubmit_ConcurrentClaim_NoOverbooking(t *testing.T) {
	s := setupTaskSuite(t)
	s.svc.cap = 3
	s.seedAccount(t, 1_000_000)

	const n = 12
	var success, limited int32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// 并发 Reserve 同账户会撞乐观锁 ErrVersionConflict（生产由 handler 映射 503 + 业务重试）；
			// 测试内重试以隔离出「claim 原子上限」这一被测不变量。
			for attempt := 0; attempt < 50; attempt++ {
				_, err := s.svc.Submit(context.Background(), s.submitParams(1000))
				if errors.Is(err, ledger.ErrVersionConflict) {
					time.Sleep(time.Millisecond)
					continue
				}
				switch {
				case err == nil:
					atomic.AddInt32(&success, 1)
				case errors.Is(err, ErrConcurrencyLimit):
					atomic.AddInt32(&limited, 1)
				}
				return
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, int32(3), atomic.LoadInt32(&success), "恰好 cap 个成功")
	assert.Equal(t, int32(n-3), atomic.LoadInt32(&limited), "其余 ErrConcurrencyLimit")
	assert.Equal(t, int32(3), s.inflight(t))
	b := s.balance(t)
	assert.Equal(t, int64(3000), b.Reserved, "仅 3 个 reserve 在账，其余已回退无 orphan")
	assertInvariant(t, b)
}

// TestMarkUpstreamTerminal_ConcurrentCAS_OneRelease：N 并发推同一 task 进终态，仅一个 CAS 赢、claim 仅释放一次（无 double-release）。
func TestMarkUpstreamTerminal_ConcurrentCAS_OneRelease(t *testing.T) {
	s := setupTaskSuite(t)
	s.seedAccount(t, 100_000)
	taskID, err := s.svc.Submit(context.Background(), s.submitParams(1000))
	require.NoError(t, err)
	require.NoError(t, s.svc.handleSubmit(context.Background(), taskID))
	require.Equal(t, int32(1), s.inflight(t))

	var wins int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			won, _ := s.svc.markUpstreamTerminal(context.Background(), taskID,
				db.TaskStatusUPSTREAMSUBMITTED, db.TaskStatusCOMPLETED, "", "", s.accountID, "gw-video")
			if won {
				atomic.AddInt32(&wins, 1)
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, int32(1), atomic.LoadInt32(&wins), "仅一个 CAS 赢")
	assert.Equal(t, int32(0), s.inflight(t), "claim 仅释放一次（无 double-release）")
}

// TestSettle_ReleaseReserveNotFound_SettleFailed：reserve 不存在(异常)→ settle_failed 对账，非静默 SETTLED（ce-review fix）。
func TestSettle_ReleaseReserveNotFound_SettleFailed(t *testing.T) {
	s := setupTaskSuite(t)
	s.seedAccount(t, 100_000)
	ctx := context.Background()

	// 直接插 task（无对应 reserve）；correlation 指向不存在的 reserve → release 必返 ErrReserveNotFound。
	taskID := newTaskID()
	snap := TaskFinancialSnapshot{
		GatewayModel: "gw-video", ReservationCorrelationID: "no-such-reserve-" + taskID,
		ReserveMinor: 1000, ReserveTokens: 1_000_000_000, PricePerMillionTokensMinor: 6000,
		BillingMultiplierBP: bpScale,
	}
	snapBytes, err := snap.Marshal()
	require.NoError(t, err)
	_, err = s.q.InsertTask(ctx, db.InsertTaskParams{
		ID: taskID, BusinessAccountID: s.accountID, ProviderType: "volc_seedance",
		Model: "gw-video", FinancialSnapshot: snapBytes, AccountingMonth: "2026-05",
	})
	require.NoError(t, err)
	s.directSetStatus(t, taskID, db.TaskStatusFAILED, "")

	require.NoError(t, s.svc.settleTask(ctx, taskID))
	assert.Equal(t, db.TaskStatusSETTLEFAILED, s.getTask(t, taskID).Status)
}

// 编译期断言 mockAdapter 实现接口。
var _ video.AsyncProviderAdapter = (*mockAdapter)(nil)
var _ = errors.Is

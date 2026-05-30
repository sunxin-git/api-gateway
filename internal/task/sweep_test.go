package task

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sunxin-git/api-gateway/internal/channel"
	"github.com/sunxin-git/api-gateway/internal/db"
	"github.com/sunxin-git/api-gateway/internal/ledger"
	"github.com/sunxin-git/api-gateway/internal/relay/video"
)

// ---------- 6b sweep 测试专用 helper ----------

// backdate 把 task 某时间列改为 NOW()-age（绕 CAS，模拟陈旧任务供 sweep 阈值扫描）。
// column 由测试内写死常量（submitted_at / upstream_submitted_at / updated_at），无注入风险。
func (s *taskSuite) backdate(t *testing.T, taskID, column string, age time.Duration) {
	t.Helper()
	_, err := s.pool.Exec(context.Background(),
		fmt.Sprintf("UPDATE task SET %s = $1 WHERE id = $2", column),
		time.Now().Add(-age), taskID)
	require.NoError(t, err)
}

// setRecoverableSubmitting 模拟「崩溃在提交窗口」：UPSTREAM_SUBMITTING + lease 已过期 + 无 upstream_task_id。
func (s *taskSuite) setRecoverableSubmitting(t *testing.T, taskID string, leaseAge time.Duration) {
	t.Helper()
	_, err := s.pool.Exec(context.Background(),
		`UPDATE task SET status='UPSTREAM_SUBMITTING', submit_locked_until=$1,
		        submit_locked_by='dead-worker', upstream_task_id=NULL, updated_at=NOW()
		 WHERE id=$2`,
		time.Now().Add(-leaseAge), taskID)
	require.NoError(t, err)
}

// seedOrphanReserve 造一个 active 视频 reserve（reference_type='video_task'）但**不**落 task 行（孤儿）。
func (s *taskSuite) seedOrphanReserve(t *testing.T, amount int64) string {
	t.Helper()
	corr := "orphan-" + newTaskID()
	actor := ledger.Actor{Type: ledger.ActorTypeTask, ID: corr}
	_, err := s.ledgerSvc.Reserve(context.Background(), actor, ledger.ReserveParams{
		AccountID:     s.accountID,
		Amount:        amount,
		CorrelationID: corr,
		ReferenceType: referenceTypeVideoTask,
		ReferenceID:   corr,
	})
	require.NoError(t, err)
	return corr
}

// errCreds 凭据解密恒失败（测试 pollUsage / resolveCreds 的 fail-closed 路径）。
type errCreds struct{}

func (errCreds) GetCredentialsForUpstream(_ context.Context, _ int64) (*channel.ChannelCredentials, error) {
	return nil, errors.New("test: 凭据解密失败")
}

// =============================================================================
// recover（崩溃恢复 fail-closed）
// =============================================================================

// TestRecover_FailClosed_NoDoubleCharge：提交后崩溃于 UPSTREAM_SUBMITTING（lease 过期）→ recover
// **fail-closed**：标 FAILED + 不重投上游（不双扣）；settle 后全额 release，钱无损、claim 释放。
func TestRecover_FailClosed_NoDoubleCharge(t *testing.T) {
	s := setupTaskSuite(t)
	s.seedAccount(t, 100_000)
	ctx := context.Background()

	taskID, err := s.svc.Submit(ctx, s.submitParams(1000))
	require.NoError(t, err)
	require.Equal(t, int32(1), s.inflight(t), "提交后占 claim")

	// 模拟崩溃：UPSTREAM_SUBMITTING + lease 过期（上游可能已建任务、无幂等键不可反查）。
	s.setRecoverableSubmitting(t, taskID, time.Hour)

	require.NoError(t, s.svc.recoverOnce(ctx))

	tk := s.getTask(t, taskID)
	assert.Equal(t, db.TaskStatusFAILED, tk.Status, "fail-closed 标 FAILED")
	assert.Equal(t, errCodeRecoverFailClosed, tk.ErrorCode.String)
	assert.Equal(t, int32(0), s.inflight(t), "FAILED 释放 claim")
	assert.Equal(t, int64(0), s.adapter.submitCount(), "fail-closed 绝不重投上游（不双扣的核心）")
	assert.GreaterOrEqual(t, s.enq.settleCount(), 1, "FAILED 入队 settle 以 release reserve")

	// settle → 全额 release → 钱无损。
	require.NoError(t, s.svc.settleTask(ctx, taskID))
	assert.Equal(t, db.TaskStatusSETTLED, s.getTask(t, taskID).Status)
	b := s.balance(t)
	assert.Equal(t, int64(0), b.Reserved, "全额 release")
	assert.Equal(t, int64(0), b.UsedTotal, "fail-closed 不扣费")
	assert.Equal(t, int64(100_000), b.Available, "钱全数退回（宁漏不双扣）")
	assertInvariant(t, b)
}

// =============================================================================
// fetch reconciler ① 主动 Poll stuck UPSTREAM_SUBMITTED
// =============================================================================

// TestFetchReconcile_StuckUpstreamSubmitted_PollTerminal：UPSTREAM_SUBMITTED 超时未终态（回调缺失）
// → 主动 Poll 命中 succeeded → 推 COMPLETED + 释放 claim + 入队 settle。
func TestFetchReconcile_StuckUpstreamSubmitted_PollTerminal(t *testing.T) {
	s := setupTaskSuite(t)
	s.seedAccount(t, 100_000)
	ctx := context.Background()

	taskID, err := s.svc.Submit(ctx, s.submitParams(1000))
	require.NoError(t, err)
	require.NoError(t, s.svc.handleSubmit(ctx, taskID)) // → UPSTREAM_SUBMITTED + upstream_submitted_at=NOW()
	require.Equal(t, db.TaskStatusUPSTREAMSUBMITTED, s.getTask(t, taskID).Status)

	// 回调一直没来：upstream_submitted_at 超阈值。
	s.backdate(t, taskID, "upstream_submitted_at", time.Hour)
	s.adapter.pollFn = func(_ context.Context, _ *video.VideoModelEntry, _ video.UpstreamCredentials, _ string) (*video.PollResult, error) {
		return &video.PollResult{Status: video.UpstreamSucceeded, Usage: &video.UpstreamUsage{CompletionTokens: 100_000}}, nil
	}

	require.NoError(t, s.svc.pollStuckUpstreamSubmitted(ctx))

	tk := s.getTask(t, taskID)
	assert.Equal(t, db.TaskStatusCOMPLETED, tk.Status, "主动 Poll 推 COMPLETED")
	assert.False(t, tk.ErrorCode.Valid, "COMPLETED 不写 error_code（维持 COMPLETED⟺空 不变量）")
	assert.Equal(t, int32(0), s.inflight(t), "进终态释放 claim")
	assert.GreaterOrEqual(t, s.enq.settleCount(), 1, "赢家入队 settle")
}

// TestFetchReconcile_StuckUpstreamSubmitted_PollFailed_Terminal：Poll 命中 failed → 推 FAILED + 非空 error_code。
func TestFetchReconcile_StuckUpstreamSubmitted_PollFailed_Terminal(t *testing.T) {
	s := setupTaskSuite(t)
	s.seedAccount(t, 100_000)
	ctx := context.Background()

	taskID, err := s.svc.Submit(ctx, s.submitParams(1000))
	require.NoError(t, err)
	require.NoError(t, s.svc.handleSubmit(ctx, taskID))
	s.backdate(t, taskID, "upstream_submitted_at", time.Hour)
	s.adapter.pollFn = func(_ context.Context, _ *video.VideoModelEntry, _ video.UpstreamCredentials, _ string) (*video.PollResult, error) {
		return &video.PollResult{Status: video.UpstreamFailed, FailureMessage: "content_violation"}, nil
	}

	require.NoError(t, s.svc.pollStuckUpstreamSubmitted(ctx))

	tk := s.getTask(t, taskID)
	assert.Equal(t, db.TaskStatusFAILED, tk.Status)
	assert.Equal(t, errCodeUpstreamFailed, tk.ErrorCode.String, "失败终态写非空 error_code")
	assert.Equal(t, int32(0), s.inflight(t))
}

// TestFetchReconcile_StuckUpstreamSubmitted_StillRunning_NoOp：Poll 仍 running → 不动（不提前终态）。
func TestFetchReconcile_StuckUpstreamSubmitted_StillRunning_NoOp(t *testing.T) {
	s := setupTaskSuite(t)
	s.seedAccount(t, 100_000)
	ctx := context.Background()

	taskID, err := s.svc.Submit(ctx, s.submitParams(1000))
	require.NoError(t, err)
	require.NoError(t, s.svc.handleSubmit(ctx, taskID))
	s.backdate(t, taskID, "upstream_submitted_at", time.Hour)
	s.adapter.pollFn = func(_ context.Context, _ *video.VideoModelEntry, _ video.UpstreamCredentials, _ string) (*video.PollResult, error) {
		return &video.PollResult{Status: video.UpstreamRunning}, nil
	}

	require.NoError(t, s.svc.pollStuckUpstreamSubmitted(ctx))
	assert.Equal(t, db.TaskStatusUPSTREAMSUBMITTED, s.getTask(t, taskID).Status, "仍 running 不提前终态，留 expire 兜底")
	assert.Equal(t, int32(1), s.inflight(t), "claim 仍持有")
}

// =============================================================================
// fetch reconciler ② SUBMITTED 无 job 重投
// =============================================================================

// TestFetchReconcile_SubmittedNoJob_Reenqueue：SUBMITTED 滞留（入队丢失）→ 幂等重投 submit。
func TestFetchReconcile_SubmittedNoJob_Reenqueue(t *testing.T) {
	s := setupTaskSuite(t)
	s.seedAccount(t, 100_000)
	ctx := context.Background()

	taskID, err := s.svc.Submit(ctx, s.submitParams(1000))
	require.NoError(t, err)
	// 模拟入队丢失：submitted_at 超阈值，且清空 enqueuer 记录以观察重投。
	s.backdate(t, taskID, "submitted_at", time.Hour)
	s.enq.submits = nil

	require.NoError(t, s.svc.reenqueueSubmittedNoJob(ctx))

	s.enq.mu.Lock()
	got := append([]string(nil), s.enq.submits...)
	s.enq.mu.Unlock()
	assert.Contains(t, got, taskID, "SUBMITTED 滞留 → 重投 submit job")
	assert.Equal(t, db.TaskStatusSUBMITTED, s.getTask(t, taskID).Status, "重投不改状态（仍 SUBMITTED 等 worker CAS）")
}

// =============================================================================
// fetch reconciler ③ 卡住 SETTLING 恢复
// =============================================================================

// TestFetchReconcile_StuckSettling_CompletedCommitted_Finalize：COMPLETED 已 commit 但卡 SETTLING
// → 探针反查 reserve 已 commit（ErrNoRows）→ 直接 finalize SETTLED，不二次动账、不无谓 Poll。
func TestFetchReconcile_StuckSettling_CompletedCommitted_Finalize(t *testing.T) {
	s := setupTaskSuite(t)
	s.seedAccount(t, 100_000)
	ctx := context.Background()

	taskID, err := s.svc.Submit(ctx, s.submitParams(1000))
	require.NoError(t, err)
	require.NoError(t, s.svc.handleSubmit(ctx, taskID)) // → UPSTREAM_SUBMITTED
	// 正常走到 SETTLED（COMPLETED → commit 600），钱已落账（commit entry 在 base correlation）。
	_, err = s.svc.markUpstreamTerminal(ctx, taskID, db.TaskStatusUPSTREAMSUBMITTED, db.TaskStatusCOMPLETED, "", "", s.accountID, "gw-video")
	require.NoError(t, err)
	require.NoError(t, s.svc.settleTask(ctx, taskID))
	require.Equal(t, db.TaskStatusSETTLED, s.getTask(t, taskID).Status)
	require.Equal(t, int64(600), s.balance(t).UsedTotal)

	// 模拟「硬崩溃于终态 CAS 前」：强推回 SETTLING（error_code 仍空 = COMPLETED）。
	s.directSetStatus(t, taskID, db.TaskStatusSETTLING, "cgt-committed")
	s.backdate(t, taskID, "updated_at", time.Hour)
	// poll 设为失败：验证「已 commit → 不 Poll 直接 finalize」（若误 Poll 会落 settle_failed）。
	s.adapter.pollFn = func(_ context.Context, _ *video.VideoModelEntry, _ video.UpstreamCredentials, _ string) (*video.PollResult, error) {
		return nil, video.ErrUpstreamTimeout
	}

	require.NoError(t, s.svc.recoverStuckSettling(ctx))
	assert.Equal(t, db.TaskStatusSETTLED, s.getTask(t, taskID).Status, "已 commit → finalize SETTLED（不无谓 Poll）")
	b := s.balance(t)
	assert.Equal(t, int64(600), b.UsedTotal, "已 commit，不二次动账")
	assert.Equal(t, int64(99_400), b.Available)
	assertInvariant(t, b)
}

// TestFetchReconcile_StuckSettling_FailedReleased_Finalize：失败终态已 release 但卡 SETTLING
// → settleReleased 重走，ledger.Release 命中既有 :release entry → 幂等 finalize SETTLED，不二次动账。
func TestFetchReconcile_StuckSettling_FailedReleased_Finalize(t *testing.T) {
	s := setupTaskSuite(t)
	s.seedAccount(t, 100_000)
	ctx := context.Background()

	taskID, err := s.svc.Submit(ctx, s.submitParams(1000))
	require.NoError(t, err)
	require.NoError(t, s.svc.handleSubmit(ctx, taskID)) // → UPSTREAM_SUBMITTED
	// 正常走到 SETTLED（FAILED → release 全额），钱已落账（release entry 在 base+":release"）。
	_, err = s.svc.markUpstreamTerminal(ctx, taskID, db.TaskStatusUPSTREAMSUBMITTED, db.TaskStatusFAILED, errCodeUpstreamFailed, "boom", s.accountID, "gw-video")
	require.NoError(t, err)
	require.NoError(t, s.svc.settleTask(ctx, taskID))
	require.Equal(t, db.TaskStatusSETTLED, s.getTask(t, taskID).Status)
	require.Equal(t, int64(100_000), s.balance(t).Available)

	// 模拟「硬崩溃于终态 CAS 前」：强推回 SETTLING（error_code 仍非空 = 失败终态）。
	s.directSetStatus(t, taskID, db.TaskStatusSETTLING, "")
	s.backdate(t, taskID, "updated_at", time.Hour)

	require.NoError(t, s.svc.recoverStuckSettling(ctx))
	assert.Equal(t, db.TaskStatusSETTLED, s.getTask(t, taskID).Status, "ledger.Release 幂等 → finalize SETTLED")
	b := s.balance(t)
	assert.Equal(t, int64(100_000), b.Available, "已 release，不二次动账")
	assert.Equal(t, int64(0), b.Reserved)
	assertInvariant(t, b)
}

// TestFetchReconcile_StuckSettling_MoneyNotLanded_Completed：COMPLETED 但崩溃于 commit 前（reserve 仍 active）
// → 反查 active reserve → 重走 settleCompleted（Poll usage → commit）→ SETTLED。
func TestFetchReconcile_StuckSettling_MoneyNotLanded_Completed(t *testing.T) {
	s := setupTaskSuite(t)
	s.seedAccount(t, 100_000)
	ctx := context.Background()

	taskID, err := s.svc.Submit(ctx, s.submitParams(1000))
	require.NoError(t, err)
	// 模拟崩溃落在 terminal→SETTLING CAS 之后、commit 之前：reserve 仍 active，error_code 空 = COMPLETED。
	s.directSetStatus(t, taskID, db.TaskStatusSETTLING, "cgt-stuck")
	s.backdate(t, taskID, "updated_at", time.Hour)
	s.adapter.pollFn = func(_ context.Context, _ *video.VideoModelEntry, _ video.UpstreamCredentials, _ string) (*video.PollResult, error) {
		return &video.PollResult{Status: video.UpstreamSucceeded, Usage: &video.UpstreamUsage{CompletionTokens: 100_000}}, nil
	}

	require.NoError(t, s.svc.recoverStuckSettling(ctx))
	assert.Equal(t, db.TaskStatusSETTLED, s.getTask(t, taskID).Status, "active reserve → 重走 settleCompleted")
	b := s.balance(t)
	assert.Equal(t, int64(600), b.UsedTotal, "commit 真实 usage 600")
	assert.Equal(t, int64(0), b.Reserved, "差额 release")
	assertInvariant(t, b)
}

// TestRecoverStuckSettling_Idempotent：重复恢复幂等（已 SETTLED 不二次动账）。
func TestRecoverStuckSettling_Idempotent(t *testing.T) {
	s := setupTaskSuite(t)
	s.seedAccount(t, 100_000)
	ctx := context.Background()

	taskID, err := s.svc.Submit(ctx, s.submitParams(1000))
	require.NoError(t, err)
	s.directSetStatus(t, taskID, db.TaskStatusSETTLING, "cgt-idem")
	s.backdate(t, taskID, "updated_at", time.Hour)
	s.adapter.pollFn = func(_ context.Context, _ *video.VideoModelEntry, _ video.UpstreamCredentials, _ string) (*video.PollResult, error) {
		return &video.PollResult{Status: video.UpstreamSucceeded, Usage: &video.UpstreamUsage{CompletionTokens: 100_000}}, nil
	}

	require.NoError(t, s.svc.recoverStuckSettling(ctx))
	require.Equal(t, db.TaskStatusSETTLED, s.getTask(t, taskID).Status)
	bal1 := s.balance(t)

	// 二次恢复：已 SETTLED，无可扫的 SETTLING 行 → no-op，账不变。
	require.NoError(t, s.svc.recoverStuckSettling(ctx))
	assert.Equal(t, db.TaskStatusSETTLED, s.getTask(t, taskID).Status)
	assert.Equal(t, bal1.UsedTotal, s.balance(t).UsedTotal, "幂等：不二次扣账")
	assert.Equal(t, bal1.Available, s.balance(t).Available)
}

// =============================================================================
// expire（终态收敛兜底）
// =============================================================================

// TestExpire_StuckTask_Expired：在途任务超最长执行期 → EXPIRED + 释放 claim + 入队 settle → release。
func TestExpire_StuckTask_Expired(t *testing.T) {
	s := setupTaskSuite(t)
	s.seedAccount(t, 100_000)
	ctx := context.Background()

	taskID, err := s.svc.Submit(ctx, s.submitParams(1000))
	require.NoError(t, err)
	require.NoError(t, s.svc.handleSubmit(ctx, taskID)) // → UPSTREAM_SUBMITTED, claim=1
	// 超最长执行期（默认 48h）：submitted_at 退 49h。
	s.backdate(t, taskID, "submitted_at", 49*time.Hour)

	require.NoError(t, s.svc.expireOnce(ctx))
	tk := s.getTask(t, taskID)
	assert.Equal(t, db.TaskStatusEXPIRED, tk.Status)
	assert.Equal(t, errCodeExecutionExpired, tk.ErrorCode.String)
	assert.Equal(t, int32(0), s.inflight(t), "EXPIRED 释放 claim")
	assert.GreaterOrEqual(t, s.enq.settleCount(), 1, "入队 settle 以 release")

	require.NoError(t, s.svc.settleTask(ctx, taskID))
	b := s.balance(t)
	assert.Equal(t, int64(0), b.Reserved, "EXPIRED settle release 全额")
	assert.Equal(t, int64(100_000), b.Available)
	assertInvariant(t, b)
}

// =============================================================================
// orphan reserve sweep
// =============================================================================

// TestOrphanReserveSweep_Release：陈旧孤儿 reserve（无对应 task）→ Release 回退。
func TestOrphanReserveSweep_Release(t *testing.T) {
	s := setupTaskSuite(t)
	s.seedAccount(t, 100_000)
	ctx := context.Background()

	s.seedOrphanReserve(t, 1000) // reserve 1000，无 task 行
	require.Equal(t, int64(1000), s.balance(t).Reserved)

	// 最小年龄阈值设为负（cutoff 落未来）→ 刚建的孤儿即满足「确陈旧」条件被回收。
	s.svc.orphanReserveMinAge = -time.Hour
	require.NoError(t, s.svc.orphanReserveSweepOnce(ctx))

	b := s.balance(t)
	assert.Equal(t, int64(0), b.Reserved, "孤儿 reserve 已回退")
	assert.Equal(t, int64(100_000), b.Available)
	assertInvariant(t, b)
}

// TestOrphanReserveSweep_MinAgeGuard：未达最小年龄阈值的 reserve **不**回收（防误回收 in-flight）。
func TestOrphanReserveSweep_MinAgeGuard(t *testing.T) {
	s := setupTaskSuite(t)
	s.seedAccount(t, 100_000)
	ctx := context.Background()

	s.seedOrphanReserve(t, 1000)
	require.Equal(t, int64(1000), s.balance(t).Reserved)

	// 最小年龄阈值 1h（cutoff=now-1h）→ 刚建的 reserve 未达陈旧条件，不回收。
	s.svc.orphanReserveMinAge = time.Hour
	require.NoError(t, s.svc.orphanReserveSweepOnce(ctx))

	assert.Equal(t, int64(1000), s.balance(t).Reserved, "in-flight 窗口内 reserve 不被误回收")
}

// =============================================================================
// 回填 6a ce-review 列出的两条测试缺口
// =============================================================================

// TestSubmitWorker_UpstreamSubmittedCASFails_Alarm（6a 缺口①）：上游 Submit 成功但本地 CAS
// UPSTREAM_SUBMITTED 失败（lease 被抢）→ 告警 + 不返 err（防 Asynq 重投再次 Submit → 双提交）。
func TestSubmitWorker_UpstreamSubmittedCASFails_Alarm(t *testing.T) {
	s := setupTaskSuite(t)
	s.seedAccount(t, 100_000)
	ctx := context.Background()

	taskID, err := s.svc.Submit(ctx, s.submitParams(1000))
	require.NoError(t, err)

	// adapter.Submit 成功的「同时」本地态被推走（模拟 lease 被 recover 抢占），
	// 使随后 CAS UPSTREAM_SUBMITTING→UPSTREAM_SUBMITTED 受影响 0 行。
	s.adapter.submitFn = func(_ context.Context, _ *video.VideoModelEntry, _ video.UpstreamCredentials, _ *video.ValidatedRequest, _ string) (string, error) {
		_, e := s.pool.Exec(ctx, `UPDATE task SET status='FAILED', updated_at=NOW() WHERE id=$1`, taskID)
		require.NoError(t, e)
		return "cgt-stolen", nil
	}

	// 不返 err（否则 Asynq 重投会再次 Submit → double-submit）。
	require.NoError(t, s.svc.handleSubmit(ctx, taskID), "CAS 失败仅告警，不返 err")
	assert.Equal(t, int64(1), s.adapter.submitCount(), "仅提交一次上游")
	tk := s.getTask(t, taskID)
	assert.Equal(t, db.TaskStatusFAILED, tk.Status, "本地态停留在被抢走后的状态")
	assert.NotEqual(t, "cgt-stolen", tk.UpstreamTaskID.String, "CAS 失败 → upstream_task_id 未落库")
}

// TestPollUsage_CredentialError_SettleFailed（6a 缺口②）：settle COMPLETED 时凭据解密失败 →
// pollUsage 返 nil（无可信 usage）→ settle_failed，reserve 留对账（不猜扣额）。
func TestPollUsage_CredentialError_SettleFailed(t *testing.T) {
	s := setupTaskSuite(t)
	s.seedAccount(t, 100_000)
	ctx := context.Background()

	taskID, err := s.svc.Submit(ctx, s.submitParams(1000))
	require.NoError(t, err)
	s.directSetStatus(t, taskID, db.TaskStatusCOMPLETED, "cgt-cred")
	s.svc.creds = errCreds{} // 凭据解密恒失败

	require.NoError(t, s.svc.settleTask(ctx, taskID))
	assert.Equal(t, db.TaskStatusSETTLEFAILED, s.getTask(t, taskID).Status, "凭据失败无法 Poll usage → settle_failed")
	b := s.balance(t)
	assert.Equal(t, int64(1000), b.Reserved, "缺可信 usage 不 commit/不 release，reserve 留对账")
	assert.Equal(t, int64(0), b.UsedTotal)
	assertInvariant(t, b)
}

// TestRecoverStuckSettling_ConcurrentRecover_MoneySafe：N 并发恢复同一卡住 SETTLING（COMPLETED）任务，
// 仅 commit 一次（CAS + 账本 ErrAlreadySettled），账本不变量守恒（涉账本/并发必测，CLAUDE.md）。
func TestRecoverStuckSettling_ConcurrentRecover_MoneySafe(t *testing.T) {
	s := setupTaskSuite(t)
	s.seedAccount(t, 100_000)
	ctx := context.Background()

	taskID, err := s.svc.Submit(ctx, s.submitParams(1000))
	require.NoError(t, err)
	s.directSetStatus(t, taskID, db.TaskStatusSETTLING, "cgt-conc") // error_code 空 = COMPLETED；reserve 仍 active
	s.backdate(t, taskID, "updated_at", time.Hour)
	s.adapter.pollFn = func(_ context.Context, _ *video.VideoModelEntry, _ video.UpstreamCredentials, _ string) (*video.PollResult, error) {
		return &video.PollResult{Status: video.UpstreamSucceeded, Usage: &video.UpstreamUsage{CompletionTokens: 100_000}}, nil
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = s.svc.recoverStuckSettling(ctx) }()
	}
	wg.Wait()

	assert.Equal(t, db.TaskStatusSETTLED, s.getTask(t, taskID).Status)
	b := s.balance(t)
	assert.Equal(t, int64(600), b.UsedTotal, "并发恢复仅 commit 一次（无双扣）")
	assert.Equal(t, int64(0), b.Reserved, "差额 release")
	assert.Equal(t, int64(99_400), b.Available)
	assertInvariant(t, b)
}

// TestRecoverStuckSettling_CompletedPollFails_SettleFailed：恢复 COMPLETED 卡住 SETTLING 时（reserve
// 未落账）Poll 失败 → 无可信 usage → settle_failed，reserve 留对账（不猜扣额）。
func TestRecoverStuckSettling_CompletedPollFails_SettleFailed(t *testing.T) {
	s := setupTaskSuite(t)
	s.seedAccount(t, 100_000)
	ctx := context.Background()

	taskID, err := s.svc.Submit(ctx, s.submitParams(1000))
	require.NoError(t, err)
	s.directSetStatus(t, taskID, db.TaskStatusSETTLING, "cgt-pf") // COMPLETED（error_code 空）、reserve active
	s.backdate(t, taskID, "updated_at", time.Hour)
	s.adapter.pollFn = func(_ context.Context, _ *video.VideoModelEntry, _ video.UpstreamCredentials, _ string) (*video.PollResult, error) {
		return nil, video.ErrUpstreamTimeout // Poll 持续失败
	}

	require.NoError(t, s.svc.recoverStuckSettling(ctx))
	assert.Equal(t, db.TaskStatusSETTLEFAILED, s.getTask(t, taskID).Status, "缺可信 usage → settle_failed")
	b := s.balance(t)
	assert.Equal(t, int64(1000), b.Reserved, "reserve 留对账，不 commit/不 release")
	assert.Equal(t, int64(0), b.UsedTotal)
	assertInvariant(t, b)
}

// TestOrphanReserveSweep_AlreadyReleased_NotRescanned：已回收的孤儿 reserve 不再被扫描命中
// （锁定 NOT EXISTS 匹配 ':release' 后缀的修复，ce-review data-migrations P1 回归）。
func TestOrphanReserveSweep_AlreadyReleased_NotRescanned(t *testing.T) {
	s := setupTaskSuite(t)
	s.seedAccount(t, 100_000)
	ctx := context.Background()

	corr := s.seedOrphanReserve(t, 1000)
	s.svc.orphanReserveMinAge = -time.Hour // cutoff 落未来，纳入刚建的孤儿

	// 第一轮：回收。
	require.NoError(t, s.svc.orphanReserveSweepOnce(ctx))
	require.Equal(t, int64(0), s.balance(t).Reserved, "首轮回收孤儿 reserve")

	// 直接查扫描结果：已 release（release entry 记在 corr:release）的孤儿不应再被命中。
	rows, err := s.q.ScanOrphanVideoReserves(ctx, db.ScanOrphanVideoReservesParams{
		MaxCreatedAt: time.Now().Add(time.Hour),
		BatchSize:    100,
	})
	require.NoError(t, err)
	for _, r := range rows {
		assert.NotEqual(t, corr, r.CorrelationID, "已回收孤儿不应再次出现在扫描结果（防反复重扫/误导日志）")
	}

	// 第二轮 sweep：幂等，账不变。
	require.NoError(t, s.svc.orphanReserveSweepOnce(ctx))
	assert.Equal(t, int64(100_000), s.balance(t).Available)
	assertInvariant(t, s.balance(t))
}

// TestFetchReconcileOnce_Smoke：fetchReconcileOnce 三段集成跑通（空库不报错 + 推进一个 stuck 任务）。
func TestFetchReconcileOnce_Smoke(t *testing.T) {
	s := setupTaskSuite(t)
	s.seedAccount(t, 100_000)
	ctx := context.Background()

	taskID, err := s.svc.Submit(ctx, s.submitParams(1000))
	require.NoError(t, err)
	require.NoError(t, s.svc.handleSubmit(ctx, taskID))
	s.backdate(t, taskID, "upstream_submitted_at", time.Hour)
	s.adapter.pollFn = func(_ context.Context, _ *video.VideoModelEntry, _ video.UpstreamCredentials, _ string) (*video.PollResult, error) {
		return &video.PollResult{Status: video.UpstreamSucceeded, Usage: &video.UpstreamUsage{CompletionTokens: 100_000}}, nil
	}

	require.NoError(t, s.svc.fetchReconcileOnce(ctx), "三段 sweep 集成跑通")
	assert.Equal(t, db.TaskStatusCOMPLETED, s.getTask(t, taskID).Status)
}

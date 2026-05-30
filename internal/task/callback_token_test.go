package task

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sunxin-git/api-gateway/internal/db"
)

// ---------- 纯函数：token 生成 + 常量时间校验 ----------

func TestNewCallbackToken(t *testing.T) {
	a := newCallbackToken()
	b := newCallbackToken()
	assert.Len(t, a, callbackTokenBytes*2, "32 字节 → 64 hex 字符")
	assert.NotEqual(t, a, b, "两次生成不应相同（随机）")
}

func TestConstantTimeTokenMatch(t *testing.T) {
	tok := newCallbackToken()
	assert.True(t, constantTimeTokenMatch(tok, tok), "相同 token 匹配")
	assert.False(t, constantTimeTokenMatch(tok, tok+"x"), "长度不同不匹配")
	assert.False(t, constantTimeTokenMatch(tok, newCallbackToken()), "不同 token 不匹配")
	assert.False(t, constantTimeTokenMatch("", "anything"), "空存储 token 一律不匹配")
	assert.False(t, constantTimeTokenMatch(tok, ""), "空请求 token 不匹配")
}

// ---------- 集成：HandleCallback（真 PG） ----------

// setCallbackToken 测试用：直接写 task.callback_token（摆精确前置态）。
func (s *taskSuite) setCallbackToken(t *testing.T, taskID, token string) {
	t.Helper()
	_, err := s.pool.Exec(context.Background(),
		`UPDATE task SET callback_token = $1 WHERE id = $2`, token, taskID)
	require.NoError(t, err)
}

// submitUpstreamSubmitted 提交一个任务并摆到 UPSTREAM_SUBMITTED + 设回调 token，返回 taskID。
func (s *taskSuite) submitUpstreamSubmitted(t *testing.T, token string) string {
	t.Helper()
	s.seedAccount(t, 100_000)
	taskID, err := s.svc.Submit(context.Background(), s.submitParams(1000))
	require.NoError(t, err)
	s.directSetStatus(t, taskID, db.TaskStatusUPSTREAMSUBMITTED, "cgt-cb-1")
	s.setCallbackToken(t, taskID, token)
	return taskID
}

func TestHandleCallback_ValidTokenTriggersAdvance(t *testing.T) {
	s := setupTaskSuite(t)
	ctx := context.Background()
	const tok = "valid-callback-token-segment"
	taskID := s.submitUpstreamSubmitted(t, tok)

	// mock pollFn 默认返回 Succeeded + usage → pollAndAdvance 推 COMPLETED + 入队 settle。
	outcome, err := s.svc.HandleCallback(ctx, taskID, tok)
	require.NoError(t, err)
	assert.Equal(t, CallbackAccepted, outcome)

	assert.Equal(t, db.TaskStatusCOMPLETED, s.getTask(t, taskID).Status, "回调触发 Poll 反查推进到 COMPLETED")
	assert.Equal(t, 1, s.enq.settleCount(), "CAS 赢家入队 settle")
	assert.Equal(t, int32(0), s.inflight(t), "进上游终态释放并发 claim")
}

func TestHandleCallback_WrongToken401NoStateChange(t *testing.T) {
	s := setupTaskSuite(t)
	ctx := context.Background()
	taskID := s.submitUpstreamSubmitted(t, "the-real-token")

	outcome, err := s.svc.HandleCallback(ctx, taskID, "forged-token")
	require.NoError(t, err)
	assert.Equal(t, CallbackUnauthorized, outcome)

	assert.Equal(t, db.TaskStatusUPSTREAMSUBMITTED, s.getTask(t, taskID).Status, "错 token 不改状态")
	assert.Equal(t, 0, s.enq.settleCount(), "错 token 不入队 settle")
}

func TestHandleCallback_UnknownTaskIgnored(t *testing.T) {
	s := setupTaskSuite(t)
	outcome, err := s.svc.HandleCallback(context.Background(), "vtask_does_not_exist", "whatever")
	require.NoError(t, err)
	assert.Equal(t, CallbackIgnoredUnknownTask, outcome, "未知 task → 200 忽略（不泄露存在性）")
}

// TestHandleCallback_TerminalStateDebounced 已终态任务（callback_token 已置空）的迟到回调 → 200 忽略，
// 不再触发 Poll（防泄露 token 强制重复 Poll 放大攻击）。
func TestHandleCallback_TerminalStateDebounced(t *testing.T) {
	s := setupTaskSuite(t)
	ctx := context.Background()
	const tok = "tok-before-terminal"
	taskID := s.submitUpstreamSubmitted(t, tok)
	// 摆到已终态（COMPLETED）；即便仍带 token，状态去抖优先 → 忽略。
	s.directSetStatus(t, taskID, db.TaskStatusCOMPLETED, "cgt-cb-1")

	outcome, err := s.svc.HandleCallback(ctx, taskID, tok)
	require.NoError(t, err)
	assert.Equal(t, CallbackIgnoredState, outcome)
	assert.Equal(t, 0, s.enq.settleCount(), "已终态不再触发 Poll / settle")
}

// TestHandleCallback_SubmittingStateIgnored 仍在 UPSTREAM_SUBMITTING（无 upstream_task_id）的回调 →
// 忽略（无法 Poll；正常不应在此态收到回调）。
func TestHandleCallback_SubmittingStateIgnored(t *testing.T) {
	s := setupTaskSuite(t)
	ctx := context.Background()
	s.seedAccount(t, 100_000)
	taskID, err := s.svc.Submit(ctx, s.submitParams(1000))
	require.NoError(t, err)
	s.directSetStatus(t, taskID, db.TaskStatusUPSTREAMSUBMITTING, "")
	s.setCallbackToken(t, taskID, "tok")

	outcome, err := s.svc.HandleCallback(ctx, taskID, "tok")
	require.NoError(t, err)
	assert.Equal(t, CallbackIgnoredState, outcome)
}

// TestHandleCallback_NonInflightStatesDebounced 任一「非 UPSTREAM_SUBMITTED」状态的回调（即便带
// 正确 token）一律去抖忽略，不触发 Poll（覆盖 COMPLETED 之外的失败/结算终态分支）。
func TestHandleCallback_NonInflightStatesDebounced(t *testing.T) {
	const tok = "debounce-token"
	for _, st := range []db.TaskStatus{
		db.TaskStatusFAILED, db.TaskStatusCANCELLED, db.TaskStatusEXPIRED, db.TaskStatusSETTLED,
	} {
		t.Run(string(st), func(t *testing.T) {
			s := setupTaskSuite(t)
			ctx := context.Background()
			taskID := s.submitUpstreamSubmitted(t, tok)
			s.directSetStatus(t, taskID, st, "cgt-cb-1")

			outcome, err := s.svc.HandleCallback(ctx, taskID, tok)
			require.NoError(t, err)
			assert.Equal(t, CallbackIgnoredState, outcome)
			assert.Equal(t, 0, s.enq.settleCount(), "非在途态不触发 settle")
		})
	}
}

// TestHandleCallback_ReplaySameToken_Idempotent 重放同一合法回调：首次推进 COMPLETED + 入队 settle；
// 重放命中已终态（token 已 NULL）→ 去抖忽略，绝不产生第二次 settle（防重放 / 防双结算）。
func TestHandleCallback_ReplaySameToken_Idempotent(t *testing.T) {
	s := setupTaskSuite(t)
	ctx := context.Background()
	const tok = "replay-token"
	taskID := s.submitUpstreamSubmitted(t, tok)

	o1, err := s.svc.HandleCallback(ctx, taskID, tok)
	require.NoError(t, err)
	require.Equal(t, CallbackAccepted, o1)
	require.Equal(t, 1, s.enq.settleCount())

	o2, err := s.svc.HandleCallback(ctx, taskID, tok)
	require.NoError(t, err)
	assert.Equal(t, CallbackIgnoredState, o2, "重放命中已终态 → 去抖忽略")
	assert.Equal(t, 1, s.enq.settleCount(), "重放不产生第二次 settle")
}

// TestHandleCallback_TokenNotReusableAcrossTasks 一个任务的 token 打到另一个任务 → Unauthorized
// （token 与具体 task 行绑定，非账户级可复用；防捕获回调 URL 横向越权）。
func TestHandleCallback_TokenNotReusableAcrossTasks(t *testing.T) {
	s := setupTaskSuite(t)
	ctx := context.Background()
	_ = s.submitUpstreamSubmitted(t, "token-A") // taskA（顺带 seedAccount）

	taskB, err := s.svc.Submit(ctx, s.submitParams(1000))
	require.NoError(t, err)
	s.directSetStatus(t, taskB, db.TaskStatusUPSTREAMSUBMITTED, "cgt-B")
	s.setCallbackToken(t, taskB, "token-B")

	// 用 A 的 token 打 B → 与 B 存储 token 不匹配 → Unauthorized，B 状态不变。
	outcome, err := s.svc.HandleCallback(ctx, taskB, "token-A")
	require.NoError(t, err)
	assert.Equal(t, CallbackUnauthorized, outcome)
	assert.Equal(t, db.TaskStatusUPSTREAMSUBMITTED, s.getTask(t, taskB).Status)
	assert.Equal(t, 0, s.enq.settleCount())
}

// TestHandleCallback_ConcurrentSameToken_OnlyOneAdvances N 并发回调同一任务：仅一个 CAS 赢家推进
// COMPLETED + 入队 settle，claim 仅释放一次（无 double-settle / inflight underflow）。直证
// markUpstreamTerminal 的条件 UPDATE 由 PG 行锁串行化（驳 ce-review 的并发 double-release 担忧）。
func TestHandleCallback_ConcurrentSameToken_OnlyOneAdvances(t *testing.T) {
	s := setupTaskSuite(t)
	ctx := context.Background()
	const tok = "concurrent-token"
	taskID := s.submitUpstreamSubmitted(t, tok)

	const n = 8
	var wg sync.WaitGroup
	var accepted int32
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if o, err := s.svc.HandleCallback(ctx, taskID, tok); err == nil && o == CallbackAccepted {
				atomic.AddInt32(&accepted, 1)
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, db.TaskStatusCOMPLETED, s.getTask(t, taskID).Status)
	assert.Equal(t, 1, s.enq.settleCount(), "并发回调仅入队一次 settle（CAS 赢家唯一）")
	assert.Equal(t, int32(0), s.inflight(t), "claim 仅释放一次（PG 行锁串行化 CAS，无 double-release）")
	assert.GreaterOrEqual(t, accepted, int32(1), "至少一个回调触发推进")
	assertInvariant(t, s.balance(t))
}

package task

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sunxin-git/api-gateway/internal/channel"
	"github.com/sunxin-git/api-gateway/internal/db"
)

// TestGetForAccount_CrossTenant_NotFound：跨租户隔离权威落点（SQL 强制归属）。
//
// A 提交一个 task；B（另一账户）用 GetForAccount 查 A 的 task_id → ErrTaskNotFound（0 行不可枚举），
// 即便 task 真实存在。这是 Unit 10「测试先行覆盖跨租户越权」的核心断言（落在读层，非仅 handler）。
func TestGetForAccount_CrossTenant_NotFound(t *testing.T) {
	s := setupTaskSuite(t)
	s.seedAccount(t, 100_000)
	ctx := context.Background()

	taskID, err := s.svc.Submit(ctx, s.submitParams(1000))
	require.NoError(t, err)

	// A 查自己的 task → 命中。
	view, err := s.svc.GetForAccount(ctx, s.accountID, taskID)
	require.NoError(t, err)
	assert.Equal(t, taskID, view.ID)
	assert.Equal(t, db.TaskStatusSUBMITTED, view.Status)
	assert.Equal(t, "gw-video", view.Model)
	assert.Equal(t, "text_to_video", view.TaskType, "task_type 从快照重建")

	// B 用自己账户查 A 的真实 task_id → 404（不泄露存在性）。
	_, err = s.svc.GetForAccount(ctx, "other-account-"+newTaskID(), taskID)
	assert.ErrorIs(t, err, ErrTaskNotFound, "跨租户归属不符必须返 ErrTaskNotFound（404）")
}

// TestGetForAccount_Unknown_NotFound：未知 task_id → ErrTaskNotFound。
func TestGetForAccount_Unknown_NotFound(t *testing.T) {
	s := setupTaskSuite(t)
	s.seedAccount(t, 100_000)

	_, err := s.svc.GetForAccount(context.Background(), s.accountID, "vtask_nonexistent")
	assert.ErrorIs(t, err, ErrTaskNotFound)
}

// TestGetBalance_ReflectsReserve：查余额反映在途 reserve（available 减、reserved 增）。
func TestGetBalance_ReflectsReserve(t *testing.T) {
	s := setupTaskSuite(t)
	s.seedAccount(t, 100_000)
	ctx := context.Background()

	_, err := s.svc.Submit(ctx, s.submitParams(1000))
	require.NoError(t, err)

	b, err := s.svc.GetBalance(ctx, s.accountID)
	require.NoError(t, err)
	assert.Equal(t, int64(99_000), b.Available)
	assert.Equal(t, int64(1000), b.Reserved)
}

// TestCheckEntitlement_GrantRevoke：grant 后 true、revoke 后 false。
func TestCheckEntitlement_GrantRevoke(t *testing.T) {
	s := setupTaskSuite(t)
	s.seedAccount(t, 0)
	ctx := context.Background()

	ok, err := s.svc.CheckEntitlement(ctx, s.accountID, "gw-video")
	require.NoError(t, err)
	assert.False(t, ok, "未 grant → 未授权")

	_, err = s.q.GrantEntitlement(ctx, db.GrantEntitlementParams{
		BusinessAccountID: s.accountID,
		GatewayModel:      "gw-video",
	})
	require.NoError(t, err)

	ok, err = s.svc.CheckEntitlement(ctx, s.accountID, "gw-video")
	require.NoError(t, err)
	assert.True(t, ok, "grant 后 → 已授权")

	// 清理 entitlement（cleanup 未覆盖此表）。
	t.Cleanup(func() {
		_, _ = s.pool.Exec(context.Background(),
			"DELETE FROM business_account_model_entitlement WHERE business_account_id = $1", s.accountID)
	})
}

// TestSubmit_ResolvesChannelByName：注入 ChannelResolver 后，未显式传 ChannelID 时按 catalog 绑定名解析。
func TestSubmit_ResolvesChannelByName(t *testing.T) {
	s := setupTaskSuite(t)
	s.seedAccount(t, 100_000)
	ctx := context.Background()

	// 注入 resolver：把 catalog 的 ChannelName 解析到测试 channel 行 id。
	s.svc.channels = fakeChannelResolver{id: s.channelID}

	p := s.submitParams(1000)
	p.ChannelID = nil // 不显式传，强制走 resolver 路径
	taskID, err := s.svc.Submit(ctx, p)
	require.NoError(t, err)

	tk := s.getTask(t, taskID)
	require.True(t, tk.ChannelID.Valid, "resolver 应已解析并落 channel_id")
	assert.Equal(t, s.channelID, tk.ChannelID.Int64)
}

type fakeChannelResolver struct{ id int64 }

func (f fakeChannelResolver) ResolveActiveChannelID(_ context.Context, _ string) (int64, error) {
	return f.id, nil
}

type erroringChannelResolver struct{ err error }

func (f erroringChannelResolver) ResolveActiveChannelID(_ context.Context, _ string) (int64, error) {
	return 0, f.err
}

// TestSubmit_ChannelResolveError_NoOrphanReserve：channel 解析失败在 reserve **前**短路，无 orphan reserve。
func TestSubmit_ChannelResolveError_NoOrphanReserve(t *testing.T) {
	s := setupTaskSuite(t)
	s.seedAccount(t, 100_000)
	ctx := context.Background()

	s.svc.channels = erroringChannelResolver{err: channel.ErrChannelNotFound}

	p := s.submitParams(1000)
	p.ChannelID = nil // 强制走 resolver 路径
	_, err := s.svc.Submit(ctx, p)
	require.Error(t, err)
	assert.ErrorIs(t, err, channel.ErrChannelNotFound, "应保留 channel sentinel 供 handler 映射 503")

	b := s.balance(t)
	assert.Equal(t, int64(0), b.Reserved, "解析失败在 reserve 前短路：无预占")
	assert.Equal(t, int64(100_000), b.Available, "余额未被动用（无 orphan reserve）")
	assert.Equal(t, int32(0), s.inflight(t), "未占并发位")
	assert.Empty(t, s.enq.submits, "未入队")
	assertInvariant(t, b)
}

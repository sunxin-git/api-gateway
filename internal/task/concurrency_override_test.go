package task

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sunxin-git/api-gateway/internal/relay/video"
)

// newServiceWithLimits 复用 suite 依赖构造一个注入了 limits 的 Service（测 Unit 8 覆写接线）。
func (s *taskSuite) newServiceWithLimits(t *testing.T, limits *video.ConcurrencyLimits) *Service {
	t.Helper()
	svc, err := NewService(Config{
		Pool:          s.pool,
		Ledger:        s.ledgerSvc,
		Adapter:       s.adapter,
		Catalog:       s.catalog,
		Creds:         fakeCreds{apiKey: "test-key"},
		Enqueuer:      s.enq,
		Logger:        silentLog(),
		Limits:        limits,
		SettleTimeout: 2 * time.Second,
		PollTimeout:   2 * time.Second,
		WorkerID:      "test-worker",
	})
	require.NoError(t, err)
	return svc
}

// TestSubmit_LimitsOverride_CapEnforced 验证 Unit 8 limits 接线：per-(account,model) 覆写上限实际
// 作用于 DB 原子 claim（覆写 cap=1 → 第 2 次提交 429），区别于静态默认 cap 路径（默认仍 5）。
func TestSubmit_LimitsOverride_CapEnforced(t *testing.T) {
	s := setupTaskSuite(t)
	s.seedAccount(t, 100_000)
	ctx := context.Background()

	limits, err := video.NewConcurrencyLimits(5, map[video.ConcurrencyKey]int32{
		{AccountID: s.accountID, Model: "gw-video"}: 1, // 覆写本账户×模型为 1
	})
	require.NoError(t, err)
	svc := s.newServiceWithLimits(t, limits)

	_, err = svc.Submit(ctx, s.submitParams(1000))
	require.NoError(t, err, "第 1 次占满覆写 cap=1")
	require.Equal(t, int32(1), s.inflight(t))

	_, err = svc.Submit(ctx, s.submitParams(1000))
	assert.ErrorIs(t, err, ErrConcurrencyLimit, "第 2 次超覆写 cap=1 → 429（证明用的是覆写值非默认 5）")
	assert.Equal(t, int32(1), s.inflight(t), "仍仅 1 个 claim；第 2 次 reserve 已 Release")

	// 资金不变量：第 2 次失败的 reserve 已释放，账本自洽。
	assertInvariant(t, s.balance(t))
}

// TestSubmit_LimitsZeroOverride_ModelDisabled 覆写 cap=0 → 该 (account, model) 被禁用（首次提交即 429）。
func TestSubmit_LimitsZeroOverride_ModelDisabled(t *testing.T) {
	s := setupTaskSuite(t)
	s.seedAccount(t, 100_000)
	ctx := context.Background()

	limits, err := video.NewConcurrencyLimits(5, map[video.ConcurrencyKey]int32{
		{AccountID: s.accountID, Model: "gw-video"}: 0,
	})
	require.NoError(t, err)
	svc := s.newServiceWithLimits(t, limits)

	_, err = svc.Submit(ctx, s.submitParams(1000))
	assert.ErrorIs(t, err, ErrConcurrencyLimit, "cap=0 → 首次提交即占不到（claim 的 cap=0 守卫）")
	assert.Equal(t, int32(0), s.inflight(t), "无 claim")
	assertInvariant(t, s.balance(t))
}

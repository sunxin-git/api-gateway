//go:build unix

package ledger

import (
	"context"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

// TestRegisterReconcilerPauseSignal_Unix 验证 SIGUSR1 在 Unix 平台上能正确切换 reconciler 暂停态。
//
// 流程：
//  1. 起 ReconcilerController.Start()（内部 spawn Run goroutine + 注册 SIGUSR1）
//  2. 第一次 SIGUSR1 → controller.IsPaused() == true；metric gateway_reconciler_paused == 1
//  3. 第二次 SIGUSR1 → IsPaused() == false；metric == 0；Run goroutine 重新就绪
//  4. Stop → metric 归 0，goroutine 退出
//
// 用 50ms interval + 1ns initialDelay 让 reconciler 频繁打转，加速观察。
//
// 平台限制：仅 Unix 编译进来（reconciler_pause_unix.go 同 build tag）。
func TestRegisterReconcilerPauseSignal_Unix(t *testing.T) {
	h := newReconcilerHarness(t, func(c *ReconcilerConfig) {
		c.Interval = 50 * time.Millisecond
		c.InitialDelay = 1 * time.Nanosecond // 会被 NewReconciler 内部回落为默认 30s — 故下面手动确认行为而非依赖跑次数
	})

	parentCtx, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()
	ctrl := NewReconcilerController(parentCtx, h.reconciler, newSilentLogger(), h.metrics)

	// 初始 metric == 0。
	require.InDelta(t, 0, testutil.ToFloat64(h.metrics.ReconcilerPaused), 0)

	ctrl.Start()
	defer ctrl.Stop()

	// 初始状态：未暂停。
	require.False(t, ctrl.IsPaused(), "初始应运行中")

	// 给信号 handler goroutine 一点时间起来（signal.Notify 已经同步注册，但 listener goroutine 仍需调度）。
	time.Sleep(20 * time.Millisecond)

	// 发第一次 SIGUSR1 → 暂停。
	require.NoError(t, syscall.Kill(syscall.Getpid(), syscall.SIGUSR1))
	requireEventually(t, 2*time.Second, "第一次 SIGUSR1 应进入暂停", func() bool {
		return ctrl.IsPaused()
	})
	require.InDelta(t, 1, testutil.ToFloat64(h.metrics.ReconcilerPaused), 0,
		"暂停后 gateway_reconciler_paused 应等于 1")

	// 发第二次 SIGUSR1 → 恢复。
	require.NoError(t, syscall.Kill(syscall.Getpid(), syscall.SIGUSR1))
	requireEventually(t, 2*time.Second, "第二次 SIGUSR1 应恢复运行", func() bool {
		return !ctrl.IsPaused()
	})
	require.InDelta(t, 0, testutil.ToFloat64(h.metrics.ReconcilerPaused), 0,
		"恢复后 gateway_reconciler_paused 应归 0")

	// Stop 验证 metric 归 0、不再卡。
	ctrl.Stop()
	require.InDelta(t, 0, testutil.ToFloat64(h.metrics.ReconcilerPaused), 0)
}

// TestReconcilerController_StartStop 不依赖信号；仅验证 controller 自身生命周期。
//
// 跨平台跑（Unix/Windows 都包含）—— 但本文件 build tag 是 unix，
// Windows 上专门起测试有点过度（功能等价的覆盖在 Start/Stop 的状态机里），故只跑 unix。
func TestReconcilerController_StartStop(t *testing.T) {
	h := newReconcilerHarness(t)
	parentCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctrl := NewReconcilerController(parentCtx, h.reconciler, newSilentLogger(), h.metrics)

	ctrl.Start()
	require.False(t, ctrl.IsPaused())

	// 二次 Start 应被忽略，不 panic、不 spawn 第二个 goroutine。
	ctrl.Start()
	require.False(t, ctrl.IsPaused())

	// 手动 Pause / Resume（不走信号）。
	ctrl.Pause()
	require.True(t, ctrl.IsPaused())
	require.InDelta(t, 1, testutil.ToFloat64(h.metrics.ReconcilerPaused), 0)

	ctrl.Pause() // 幂等
	require.True(t, ctrl.IsPaused())

	ctrl.Resume()
	require.False(t, ctrl.IsPaused())
	require.InDelta(t, 0, testutil.ToFloat64(h.metrics.ReconcilerPaused), 0)

	ctrl.Resume() // 幂等
	require.False(t, ctrl.IsPaused())

	ctrl.Stop()
	// Stop 后再 Start 应被忽略（不 panic）。
	ctrl.Start()
}

// requireEventually 在 timeout 内反复轮询 cond；条件不成立则 t.Fatalf。
//
// 不引 require.Eventually（避免 testify polling 写法挑剔）；本地最简实现。
func requireEventually(t *testing.T, timeout time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("requireEventually timeout: %s", msg)
}

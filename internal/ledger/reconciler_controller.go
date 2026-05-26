// Package ledger 的 ReconcilerController 部分（计划 Unit 10）。
//
// 职责：把 Reconciler 的 goroutine 生命周期 + SIGUSR1 暂停/恢复语义封装为一个
// 显式控制器，避免 main.go 散落「go reconciler.Run / cancel / re-start」碎片。
//
// 设计要点：
//   - Reconciler.Run(ctx) 由 controller 管理 ctx，外部不直接传 ctx；
//   - Pause / Resume 各自完全独立的 ctx 生命周期（cancel 后 ctx 不能重用，必须新建）；
//   - 状态切换 100% 显式 log WARN + bump gateway_reconciler_paused metric；
//   - Stop 是终态：cancel + wait goroutine 退出，cleanup signal handler；
//   - 平台相关的信号注册（SIGUSR1）通过 build tag 拆分到 reconciler_pause_{unix,windows}.go；
//     controller 本身平台无关。
//
// 不在本文件：Reconciler 主循环逻辑（reconciler.go）。
package ledger

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/sunxin-git/api-gateway/internal/obs"
)

// ReconcilerController 管理 Reconciler 的 goroutine 生命周期与暂停/恢复语义。
//
// 典型使用（main.go）：
//
//	ctrl := ledger.NewReconcilerController(ctx, reconciler, log, metrics)
//	ctrl.Start()         // 起 goroutine + 注册 SIGUSR1（仅 Unix）
//	// ... 进程跑 ...
//	ctrl.Stop()          // graceful shutdown
//
// 并发安全：所有公开方法用 mu 互斥；不允许在多 goroutine 同时调 Start。
type ReconcilerController struct {
	reconciler *Reconciler
	parentCtx  context.Context
	log        *slog.Logger
	metrics    *obs.Metrics

	mu sync.Mutex
	// runCancel 当前 Run goroutine 的 ctx cancel 函数；nil 表示未启动 / 已暂停 / 已停止。
	runCancel context.CancelFunc
	// runDone 当前 Run goroutine 退出信号（每次 spawn 新建）。
	runDone chan struct{}
	// paused true=已暂停（runCancel == nil），false=运行中。
	paused bool
	// stopped 进入终态；Stop 后所有方法成为 noop。
	stopped bool
	// signalCleanup 平台层 cleanup 函数（signal.Stop 等）；nil 表示未注册。
	signalCleanup func()
}

// NewReconcilerController 构造控制器。
//
// 入参全部必填；任一为 nil 直接 panic（显式优于隐式）。
//
//   - parentCtx：父 ctx，通常是 main 的 ctx；Stop 时不依赖此 ctx，但 ctx.Done() 会让
//     最新一次 Run goroutine 自然退出。
//   - reconciler：实际跑的 Reconciler 实例。
//   - log：slog logger。
//   - metrics：obs.Metrics，用于 bump ReconcilerPaused gauge。
func NewReconcilerController(
	parentCtx context.Context,
	reconciler *Reconciler,
	log *slog.Logger,
	metrics *obs.Metrics,
) *ReconcilerController {
	if parentCtx == nil {
		panic("ledger.NewReconcilerController: parentCtx 不能为 nil")
	}
	if reconciler == nil {
		panic("ledger.NewReconcilerController: reconciler 不能为 nil")
	}
	if log == nil {
		panic("ledger.NewReconcilerController: log 不能为 nil")
	}
	if metrics == nil {
		panic("ledger.NewReconcilerController: metrics 不能为 nil")
	}
	return &ReconcilerController{
		reconciler: reconciler,
		parentCtx:  parentCtx,
		log:        log,
		metrics:    metrics,
	}
}

// Start 启动 reconciler goroutine 并注册平台层暂停信号（仅 Unix 注册 SIGUSR1）。
//
// 重复 Start 会被忽略并 log WARN（避免误启第二个 Run goroutine 抢 advisory lock 自己）。
// 已 Stop 的 controller 不能再 Start。
func (c *ReconcilerController) Start() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.stopped {
		c.log.Warn("reconciler controller 已终态，忽略 Start 调用")
		return
	}
	if c.runCancel != nil {
		c.log.Warn("reconciler controller 已在运行，忽略重复 Start")
		return
	}

	c.spawnRunLocked()

	// 注册平台层信号 handler（仅 Unix 注册 SIGUSR1；Windows 是 no-op）。
	cleanup, err := registerReconcilerPauseSignal(c)
	if err != nil {
		c.log.Error("reconciler controller 注册暂停信号失败（继续运行，不可暂停）",
			slog.String("err", err.Error()))
	} else {
		c.signalCleanup = cleanup
	}
	c.metrics.ReconcilerPaused.Set(0)
}

// Pause 取消当前 Run goroutine 的 ctx；幂等（已暂停再调一次 noop）。
//
// 由平台层信号 handler（SIGUSR1）或测试代码调用。
//
// 注意：Pause 不等待 Run goroutine 真正退出（reconciler 内部可能正在跑一轮，
// 需要等 ctx.Done() 被检查到才退）。如需「确保退出」语义请用 Stop。
func (c *ReconcilerController) Pause() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.stopped {
		c.log.Warn("reconciler controller 已终态，忽略 Pause")
		return
	}
	if c.paused {
		c.log.Info("reconciler 已暂停，忽略重复 Pause（幂等）")
		return
	}
	c.pauseLocked("manual")
}

// Resume 重启 reconciler goroutine（用新 ctx + 新 Run）；幂等（运行中再调一次 noop）。
//
// 注意：context.CancelFunc 一旦 cancel 不能复用；Resume 必须新建 ctx。
func (c *ReconcilerController) Resume() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.stopped {
		c.log.Warn("reconciler controller 已终态，忽略 Resume")
		return
	}
	if !c.paused {
		c.log.Info("reconciler 未处于暂停态，忽略重复 Resume（幂等）")
		return
	}

	c.spawnRunLocked()
	c.paused = false
	c.metrics.ReconcilerPaused.Set(0)
	c.log.Warn("reconciler 已恢复运行（SIGUSR1 / 手动 Resume）",
		slog.String("host", hostnameSafe()),
		slog.Int("pid", os.Getpid()),
		slog.Time("at", time.Now().UTC()))
}

// Toggle 在 Pause / Resume 之间切换；平台层 SIGUSR1 handler 使用。
//
// 第一次 SIGUSR1 → Pause；第二次 SIGUSR1 → Resume；以此类推。
func (c *ReconcilerController) Toggle() {
	c.mu.Lock()
	pausedNow := c.paused
	c.mu.Unlock()
	if pausedNow {
		c.Resume()
	} else {
		c.Pause()
	}
}

// Stop 终态：cancel 当前 Run、等 goroutine 退出、cleanup signal handler。
//
// 调用后 controller 不可再 Start。
func (c *ReconcilerController) Stop() {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return
	}
	c.stopped = true

	// 取消 signal handler（避免 Stop 后还能收到 SIGUSR1 死路 spawn 新 goroutine）。
	if c.signalCleanup != nil {
		c.signalCleanup()
		c.signalCleanup = nil
	}

	// 取消当前 Run goroutine（如有）。
	cancel := c.runCancel
	done := c.runDone
	c.runCancel = nil
	c.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
			c.log.Info("reconciler controller Stop 已等到 Run 退出")
		case <-time.After(35 * time.Second):
			// 35s = 30s graceful shutdown + 5s 余量；超时是 bug 信号，不正常情况下走不到。
			c.log.Error("reconciler controller Stop 等 Run 退出超时（>35s），可能 goroutine 卡死")
		}
	}
	c.metrics.ReconcilerPaused.Set(0)
}

// IsPaused 仅供测试 / 健康检查使用；并发安全。
func (c *ReconcilerController) IsPaused() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.paused
}

// spawnRunLocked 在持锁状态下启动一个新的 Run goroutine；调用方负责保证 paused 状态正确。
//
// 用 parentCtx 派生新 ctx；cancel 函数存到 c.runCancel；goroutine 退出时关闭 c.runDone。
func (c *ReconcilerController) spawnRunLocked() {
	runCtx, cancel := context.WithCancel(c.parentCtx)
	done := make(chan struct{})
	c.runCancel = cancel
	c.runDone = done

	go func() {
		defer close(done)
		// Run 内部已有 recover；本 goroutine 没必要再加。
		if err := c.reconciler.Run(runCtx); err != nil {
			c.log.Error("reconciler.Run 返回错误（goroutine 退出）",
				slog.String("err", err.Error()))
		}
	}()
}

// pauseLocked 在持锁状态下执行暂停语义；调用方保证不处于 stopped / 已 paused 状态。
//
// reason 仅用于 log，无功能影响。
func (c *ReconcilerController) pauseLocked(reason string) {
	cancel := c.runCancel
	c.runCancel = nil
	c.paused = true
	c.metrics.ReconcilerPaused.Set(1)

	if cancel != nil {
		cancel()
	}

	c.log.Warn("reconciler 已暂停（SIGUSR1 / 手动 Pause）",
		slog.String("reason", reason),
		slog.String("host", hostnameSafe()),
		slog.Int("pid", os.Getpid()),
		slog.Time("at", time.Now().UTC()))
}

// hostnameSafe 返回主机名；失败返 "unknown"（不影响业务）。
func hostnameSafe() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}

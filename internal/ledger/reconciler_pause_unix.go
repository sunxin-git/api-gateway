//go:build unix

// Package ledger 的 reconciler 暂停信号 — Unix 平台实现（计划 Unit 10）。
//
// 监听 SIGUSR1：每次收到信号触发 ReconcilerController.Toggle()，
// 在 Pause / Resume 之间切换 reconciler goroutine 的生命周期。
//
// 用途（详见 docs/dev-setup.md §5.2 / §5.3）：
//   - 生产 migration 部署：`kill -SIGUSR1 <pid>` 暂停 reconciler → migrate up → 再 SIGUSR1 恢复
//   - Rebuild stuck 调试：手动暂停 reconciler 防止误干预
//
// 平台拆分：Windows 走 reconciler_pause_windows.go 的 no-op stub。
package ledger

import (
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

// registerReconcilerPauseSignal 注册 SIGUSR1 → Toggle 处理。
//
// 行为：
//   - 启动后台 goroutine 监听 sigCh；
//   - 每次收到 SIGUSR1 调用 ctrl.Toggle()（首次 Pause / 第二次 Resume / ...）；
//   - parentCtx.Done() 时 goroutine 退出，并 signal.Stop（避免 leak）；
//   - 返回 cleanup 函数：调用方在 Stop 时调用 cleanup → signal.Stop + 不再 Toggle。
//
// 返回 error 永远为 nil；接口形式保留为日后扩展。
func registerReconcilerPauseSignal(ctrl *ReconcilerController) (cleanup func(), err error) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGUSR1)

	// 用一个独立 done 让 cleanup 既能 stop signal 又能让 goroutine 退出。
	done := make(chan struct{})

	ctrl.log.Info("reconciler controller 已注册 SIGUSR1 暂停切换 handler",
		slog.Int("pid", os.Getpid()))

	go func() {
		defer signal.Stop(sigCh)
		for {
			select {
			case <-done:
				return
			case <-ctrl.parentCtx.Done():
				return
			case _, ok := <-sigCh:
				if !ok {
					return
				}
				ctrl.Toggle()
			}
		}
	}()

	return func() {
		// close(done) 让 goroutine 退出；signal.Stop 防止后续信号继续 deliver。
		// 用 select 避免重复 close panic（理论上 controller 用 mu 保证只调一次，但兜底）。
		select {
		case <-done:
			// 已 close
		default:
			close(done)
		}
		signal.Stop(sigCh)
	}, nil
}

//go:build windows

// Package ledger 的 reconciler 暂停信号 — Windows 平台实现（计划 Unit 10）。
//
// Windows 不支持 SIGUSR1（syscall.SIGUSR1 在 Windows 下未定义），因此本平台实现是 no-op：
// 不注册任何信号 handler，cleanup 也是 no-op。
//
// Windows 部署的暂停语义请改用进程级控制：
//   - systemctl-style：用 NSSM / sc.exe 包装为服务，stop / start 整个 gateway 进程
//   - Docker：`docker stop` / `docker start`
//   - 或暂时改 LEDGER_DRIFT_ACTION=log 让 reconciler 仍跑但不冻账户
//
// Unix 实现见 reconciler_pause_unix.go。
package ledger

import (
	"log/slog"
	"os"
)

// registerReconcilerPauseSignal Windows 平台 no-op stub。
//
// 不报错（不能让 main.go 启动失败），但 log INFO 一次明确说明语义，
// 避免运维人员误以为 SIGUSR1 在 Windows 上生效。
func registerReconcilerPauseSignal(ctrl *ReconcilerController) (cleanup func(), err error) {
	ctrl.log.Info(
		"Windows 平台不支持 SIGUSR1 暂停 reconciler；如需 migration 部署请改用 systemctl/sc/docker stop 进程级控制",
		slog.Int("pid", os.Getpid()),
	)
	return func() {}, nil
}

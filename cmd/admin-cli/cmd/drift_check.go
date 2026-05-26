// admin-cli `drift-check` 子命令（计划 Unit 9）。
//
// 语义收口：本子命令仅做 **ledger drift** 一次性扫描（ledger SUM vs balance 投影），
// 不再做 schema drift（sqlc / migrations / DB schema 的一致性检查由 `make sqlc-diff`
// + `migrate version` 单独覆盖）。
//
// 工作机制：直接调 `Reconciler.RunOnce(ctx)` —— 与后台 ticker 同一实现路径，
// 跳过 advisory lock 抢占（lock 抢占只在 `Run` 主循环里做；CLI 单次调用一定能跑）。
// 返回的 RunResult 序列化为 JSON 输出 stdout。
//
// 退出码：本子命令总是返 0（除非 RunOnce 失败 / PG 不可达），无论是否发现 drift —
// 「跑完一轮」即 success；drift 数量 > 0 由调用方读 JSON 决定后续动作。
package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
)

// driftCheckResult 是 drift-check 子命令的 stdout JSON 结构。
//
// 不直接序列化 ledger.RunResult —— 那里有 time.Duration，JSON 化变成纳秒整数对人不友好。
// 此处把 duration 拆出来用毫秒整数表达；其余字段一一对应。
type driftCheckResult struct {
	Checked        int   `json:"checked"`
	Drifted        int   `json:"drifted"`
	FalsePositives int   `json:"false_positives"`
	StuckRebuilds  int   `json:"stuck_rebuilds"`
	DurationMS     int64 `json:"duration_ms"`
}

func newDriftCheckCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "drift-check",
		Short: "对所有未冻结账户跑一轮 ledger drift 检测",
		Long: `对所有未冻结账户做一次 ledger SUM vs balance 投影一致性检测。

实现：直接调 reconciler.RunOnce —— 与后台 ticker 共享同一逻辑：
  1. REPEATABLE READ + READ ONLY 单只读事务读 SUM + balance（一致快照）
  2. 首次发现不一致时 sleep 1s 二次确认（过滤瞬时 race）
  3. 二次确认仍不一致 → 按 LEDGER_DRIFT_ACTION 处理（log 仅日志；freeze 调 service.Freeze）

输出（stdout）：
  {"checked": N, "drifted": M, "false_positives": K, "stuck_rebuilds": L, "duration_ms": D}

退出码：跑完一轮即 0，不论是否发现 drift；只有 PG 不可达 / RunOnce 失败时才返非零。`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			svc, cleanup, err := OpenServices(ctx)
			if err != nil {
				return err
			}
			defer cleanup()

			result, err := svc.Reconciler.RunOnce(ctx)
			if err != nil {
				return fmt.Errorf("drift-check RunOnce 失败: %w", err)
			}

			out := driftCheckResult{
				Checked:        result.Checked,
				Drifted:        result.Drifted,
				FalsePositives: result.FalsePositives,
				StuckRebuilds:  result.StuckRebuilds,
				DurationMS:     result.Duration.Milliseconds(),
			}
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			if err := enc.Encode(out); err != nil {
				return fmt.Errorf("序列化 drift-check JSON 失败: %w", err)
			}
			return nil
		},
	}
}

package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newDriftCheckCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "drift-check",
		Short: "校验数据库 schema 与代码模型的漂移（Phase 1 占位）",
		Long: `比较数据库 schema、sqlc 生成代码与 migrations 之间的一致性。

Phase 1 阶段仅暴露命令骨架；真实实现见 Phase 2 工作流 E。`,
		RunE: func(_ *cobra.Command, _ []string) error {
			return fmt.Errorf("drift-check: 尚未实现 — 见 Phase 2 工作流 E")
		},
	}
}

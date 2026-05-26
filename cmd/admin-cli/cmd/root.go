// Package cmd 提供 admin-cli 的 Cobra 子命令定义。
//
// Phase 1 阶段，所有命令均为「占位」骨架：列在 --help 中可见，但 RunE 返回
// 「尚未实现 — 见 Phase X 工作流 Y」错误。真实实现见 Phase 2 工作流。
package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// mustMarkFlagRequired 在 cobra flag 被标记为必填时返回错误即视为编程错误（flag 不存在）。
// 配合 CLAUDE.md §四 #6「显式优于隐式」原则：不允许 `_ = c.MarkFlagRequired(...)` 默默吞错。
func mustMarkFlagRequired(c *cobra.Command, name string) {
	if err := c.MarkFlagRequired(name); err != nil {
		panic(fmt.Errorf("admin-cli: MarkFlagRequired(%q) 失败（flag 未定义？）: %w", name, err))
	}
}

// NewRootCmd 构造 admin-cli 根命令。每次构造返回新实例，便于测试隔离。
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "admin-cli",
		Short: "api-gateway 管理 CLI（运维 / 凭据 / 业务账户）",
		Long: `admin-cli 是 api-gateway 的离线管理工具，覆盖数据库迁移、Admin Token 管理、
业务账户创建与充值、漂移检查等运维场景。

Phase 1 阶段本工具仅暴露子命令骨架，所有子命令均会返回「尚未实现」错误，
真实功能将在 Phase 2 工作流 D/E 中实现。`,
		SilenceUsage:  true,
		SilenceErrors: false,
	}

	root.AddCommand(
		newMigrateCmd(),
		newTokenCmd(),
		newAccountCmd(),
		newDriftCheckCmd(),
	)

	return root
}

// Execute 运行 root 命令，供 main 调用。
func Execute() error {
	return NewRootCmd().Execute()
}

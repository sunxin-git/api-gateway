package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// 通用提示：本子命令在 Phase 1 阶段不提供实现。
const migrateUnimplementedHint = "尚未实现 — 请使用 golang-migrate CLI 或 Makefile target；本子命令将在 Phase 2 工作流 D-min 接入"

func newMigrateCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "migrate",
		Short: "数据库迁移（Phase 1 占位）",
		Long: `管理数据库 schema 迁移。

Phase 1 阶段本命令为占位实现；请直接使用 golang-migrate CLI 或 Makefile
target（如 make migrate-up / make migrate-down）执行迁移。

Phase 2 工作流 D-min 将正式接入本命令并支持 up / down / version 子命令。`,
	}
	c.AddCommand(newMigrateUpCmd(), newMigrateDownCmd(), newMigrateVersionCmd())
	return c
}

func newMigrateUpCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "up",
		Short: "应用所有未执行的迁移（Phase 1 占位）",
		RunE: func(_ *cobra.Command, _ []string) error {
			return fmt.Errorf("migrate up: %s", migrateUnimplementedHint)
		},
	}
}

func newMigrateDownCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "down [N]",
		Short: "回滚 N 个迁移（Phase 1 占位）",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, _ []string) error {
			return fmt.Errorf("migrate down: %s", migrateUnimplementedHint)
		},
	}
	return c
}

func newMigrateVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "查询当前迁移版本（Phase 1 占位）",
		RunE: func(_ *cobra.Command, _ []string) error {
			return fmt.Errorf("migrate version: %s", migrateUnimplementedHint)
		},
	}
}

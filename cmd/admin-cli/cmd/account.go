package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newAccountCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "account",
		Short: "业务账户管理（Phase 1 占位）",
		Long: `管理业务账户（business_account），包括创建与充值。

Phase 1 阶段所有子命令仅为骨架，真实实现见 Phase 2 工作流 D-min。`,
	}
	c.AddCommand(newAccountCreateCmd(), newAccountRechargeCmd())
	return c
}

func newAccountCreateCmd() *cobra.Command {
	var (
		businessID        string
		isolationRequired bool
	)
	c := &cobra.Command{
		Use:   "create",
		Short: "创建业务账户（Phase 1 占位）",
		RunE: func(_ *cobra.Command, _ []string) error {
			_ = businessID
			_ = isolationRequired
			return fmt.Errorf("account create: 尚未实现 — 见 Phase 2 工作流 D-min")
		},
	}
	c.Flags().StringVar(&businessID, "id", "", "业务账户 ID（必填）")
	c.Flags().BoolVar(&isolationRequired, "isolation-required", false,
		"是否要求 provider 隔离（fail-closed 默认）")
	mustMarkFlagRequired(c, "id")
	return c
}

func newAccountRechargeCmd() *cobra.Command {
	var (
		businessID string
		amount     int64
	)
	c := &cobra.Command{
		Use:   "recharge",
		Short: "为业务账户充值（Phase 1 占位）",
		RunE: func(_ *cobra.Command, _ []string) error {
			_ = businessID
			_ = amount
			return fmt.Errorf("account recharge: 尚未实现 — 见 Phase 2 工作流 D-min")
		},
	}
	c.Flags().StringVar(&businessID, "id", "", "业务账户 ID（必填）")
	c.Flags().Int64Var(&amount, "amount", 0, "充值金额（最小货币单位，必填）")
	mustMarkFlagRequired(c, "id")
	mustMarkFlagRequired(c, "amount")
	return c
}

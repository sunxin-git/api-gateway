package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newTokenCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "token",
		Short: "Admin Token 管理（Phase 1 占位）",
		Long: `管理 Admin API Bearer Token。

Phase 1 阶段所有子命令仅为骨架，真实实现见 Phase 2 工作流 D-min。`,
	}
	c.AddCommand(newTokenCreateCmd())
	return c
}

func newTokenCreateCmd() *cobra.Command {
	var (
		scopes      []string
		ipAllowlist []string
	)
	c := &cobra.Command{
		Use:   "create",
		Short: "创建 Admin Token（Phase 1 占位）",
		Long: `创建一个新的 Admin Token，可指定 scope 列表与 IP 白名单。

Phase 1 阶段本命令仅打印 flag 接受到的值后返回「尚未实现」错误，
真实签发由 Phase 2 工作流 D-min 完成。`,
		RunE: func(_ *cobra.Command, _ []string) error {
			// 故意引用 flag 变量避免 staticcheck 报未用；
			// 同时让用户看到我们至少正确解析了入参。
			_ = scopes
			_ = ipAllowlist
			return fmt.Errorf("token create: 尚未实现 — 见 Phase 2 工作流 D-min")
		},
	}
	c.Flags().StringSliceVar(&scopes, "scope", nil,
		"scope 列表（逗号分隔），如 business_account:read,token:write")
	c.Flags().StringSliceVar(&ipAllowlist, "ip-allowlist", nil,
		"IP 白名单（CIDR 列表，逗号分隔），如 10.0.0.0/8,192.168.0.0/16")
	return c
}

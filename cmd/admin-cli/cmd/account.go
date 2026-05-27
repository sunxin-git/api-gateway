// admin-cli `account` 子命令族（计划 Unit 9）。
//
// 真实接 LedgerService —— 不再返回 Phase 1 占位错误。
// 严格保持子命令清单：仅 create / recharge；**不**实装 freeze / unfreeze / rebuild
// （计划 Unit 9 §不实现）。
//
// 安全约束（计划 R17）：
//   - 不接受 --created-by flag；Actor 统一通过 cliActor() 写死为 cli:bootstrap
//   - 失败 → stderr 中文 + 非零退出码；JSON 输出走 stdout（便于 `| jq`）
package cmd

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/sunxin-git/api-gateway/internal/ledger"
)

func newAccountCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "account",
		Short: "业务账户管理（创建 / 充值）",
		Long: `管理业务账户（business_account）的创建与充值。

子命令：
  create    创建新业务账户（同事务建 balance 零行 + 发 account.created 事件）
  recharge  为账户充值（幂等键 + canonical body sha256；同事务发 account.recharged）

P0 安全约束：本工具写入的 ledger entry 一律记 actor_type=cli, actor_id=bootstrap，
不接受 --created-by flag（避免审计伪造）。HTTP 路径的细粒度审计由 Phase 2 工作流 D-min 落地。`,
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
		Short: "创建业务账户",
		Long: `创建业务账户并初始化对应的 balance 零行；同事务向 webhook_event_outbox
发布 account.created 事件。

成功输出（stdout）：账户 JSON。
失败输出（stderr）：中文错误信息 + 非零退出码（如账户已存在、PG 不可达等）。`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if businessID == "" {
				return errors.New("--id 不能为空")
			}
			ctx := cmd.Context()
			svc, cleanup, err := OpenServices(ctx)
			if err != nil {
				return err
			}
			defer cleanup()

			account, err := svc.Service.CreateAccount(ctx, cliActor(), ledger.CreateAccountParams{
				ID:                businessID,
				IsolationRequired: isolationRequired,
			})
			if err != nil {
				return fmt.Errorf("创建账户失败: %w", err)
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			if err := enc.Encode(account); err != nil {
				return fmt.Errorf("序列化账户 JSON 失败: %w", err)
			}
			return nil
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
		businessID     string
		amount         int64
		idempotencyKey string
	)
	c := &cobra.Command{
		Use:   "recharge",
		Short: "为业务账户充值",
		Long: `为指定账户充值；幂等键 + canonical body sha256 双重防重放。

幂等语义：
  - 同 idempotency-key + 同 (id, amount) → 返回原 ledger entry（退出码 0）
  - 同 idempotency-key + 不同 (id 或 amount) → 拒绝并返回 ErrIdempotencyConflict（退出码非零）

成功输出（stdout）：ledger entry JSON。
失败输出（stderr）：中文错误信息 + 非零退出码。`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if businessID == "" {
				return errors.New("--id 不能为空")
			}
			if amount <= 0 {
				return fmt.Errorf("--amount 必须 > 0（当前 %d）", amount)
			}
			if idempotencyKey == "" {
				return errors.New("--idempotency-key 不能为空")
			}
			ctx := cmd.Context()
			svc, cleanup, err := OpenServices(ctx)
			if err != nil {
				return err
			}
			defer cleanup()

			body := &ledger.RechargeBody{
				AccountID:   businessID,
				Amount:      amount,
				ExternalRef: idempotencyKey,
			}
			entry, _, err := svc.Service.Recharge(ctx, cliActor(), ledger.RechargeParams{
				AccountID:      businessID,
				Amount:         amount,
				IdempotencyKey: idempotencyKey,
				CanonicalBody:  body,
			})
			if err != nil {
				return fmt.Errorf("充值失败: %w", err)
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			if err := enc.Encode(entry); err != nil {
				return fmt.Errorf("序列化 ledger entry JSON 失败: %w", err)
			}
			return nil
		},
	}
	c.Flags().StringVar(&businessID, "id", "", "业务账户 ID（必填）")
	c.Flags().Int64Var(&amount, "amount", 0, "充值金额（最小货币单位，必须 > 0）")
	c.Flags().StringVar(&idempotencyKey, "idempotency-key", "",
		"幂等键；同 key + 同 (id, amount) 视为同一笔充值（必填）")
	mustMarkFlagRequired(c, "id")
	mustMarkFlagRequired(c, "amount")
	mustMarkFlagRequired(c, "idempotency-key")
	return c
}

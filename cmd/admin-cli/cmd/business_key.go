// admin-cli `business-key` 子命令族（计划 F-min Unit 6）。
//
// 业务系统接入网关的对外 API Key 管理；与 admin token 命令同形态但简化：
//   - 无 scope（业务 key 全权限，与 OpenAI / DeepSeek 风格一致，plan §D4）
//   - 无 IP allowlist（业务多 region 接入，MVP 简化）
//   - 仅 --rpm 一个阀门（plan §D7：业务侧只有 RPM，无 circuit）
//
// 安全约束（plan §R9 + D14）：
//   - created_by 硬编码 "cli:bootstrap"，不接受 flag 注入
//   - plaintext 仅创建时返回一次；与 admin token 共享 HMAC pepper（F-min D4）→
//     CLI 创出的 key 可被 relay HTTP 鉴权命中
//   - revoke 三态语义：不存在 / 首次 / 已 revoked，分别给运维不同反馈
package cmd

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sunxin-git/api-gateway/internal/businesskey"
)

// =============================================================================
// 顶层 `business-key` 命令
// =============================================================================

func newBusinessKeyCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "business-key",
		Short: "业务 API Key 管理（create / list / revoke）",
		Long: `管理业务系统接入网关的对外 API Key（business_account_api_key）。

子命令：
  create   为指定业务账户生成新 key（plaintext 仅创建时返回一次）
  list     列出未吊销的 key（不返回 hash；可按 --business-account-id 过滤）
  revoke   吊销指定 id 的 key；已吊销时仍幂等返回 0 但带醒目警告

安全约束（R9 + D4）：
  - created_by 写死 "cli:bootstrap"，不接受 --created-by
  - 与 admin token 共享 HMAC pepper：CLI 创出的 key 可被 relay /v1/* 鉴权命中
  - 无 scope / IP allowlist（MVP 简化；业务 key 全权限）`,
	}
	c.AddCommand(
		newBusinessKeyCreateCmd(),
		newBusinessKeyListCmd(),
		newBusinessKeyRevokeCmd(),
	)
	return c
}

// =============================================================================
// `business-key create`
// =============================================================================

type businessKeyCreateFlags struct {
	description       string
	businessAccountID string
	rpm               int32
	out               string
}

func newBusinessKeyCreateCmd() *cobra.Command {
	f := &businessKeyCreateFlags{}
	c := &cobra.Command{
		Use:   "create",
		Short: "创建新业务 API Key；plaintext 仅一次性返回",
		Long: `为指定业务账户创建新 API Key。

输出模式（互斥二选一）：
  默认           plaintext JSON 输出到 stdout + stderr 反模式警告
  --out <file>   plaintext 写入指定文件（0600 + O_EXCL 防覆盖）；stdout 只输出 metadata

必填：--description / --business-account-id（business_account 必须已存在，否则 FK 失败）
可选：--rpm（RPM 限速；不设 = 不限速）`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runBusinessKeyCreate(cmd, f)
		},
	}
	c.Flags().StringVar(&f.description, "description", "", "key 标签（必填，如 'creator-platform-prod-key-1'）")
	c.Flags().StringVar(&f.businessAccountID, "business-account-id", "", "归属业务账户 ID（必填；账户须已存在）")
	c.Flags().Int32Var(&f.rpm, "rpm", 0, "RPM 限速（请求/分钟；0 = 不限速）")
	c.Flags().StringVar(&f.out, "out", "", "把 plaintext 写入指定文件（0600 + O_EXCL）；与默认 stdout 互斥")
	mustMarkFlagRequired(c, "description")
	mustMarkFlagRequired(c, "business-account-id")
	return c
}

func runBusinessKeyCreate(cmd *cobra.Command, f *businessKeyCreateFlags) error {
	params := businesskey.CreateParams{
		Description:       strings.TrimSpace(f.description),
		BusinessAccountID: strings.TrimSpace(f.businessAccountID),
		RequestsPerMinute: int32PtrIfPositive(f.rpm),
		CreatedBy:         "cli:bootstrap",
	}

	ctx := cmd.Context()
	svc, cleanup, err := OpenServices(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	key, plaintext, err := svc.BusinessKey.Create(ctx, params)
	if err != nil {
		// FK 违反（业务账户不存在）友好提示
		if isForeignKeyViolation(err) {
			return fmt.Errorf("创建 key 失败：业务账户 %q 不存在（请先用 account create 创建）", f.businessAccountID)
		}
		return fmt.Errorf("创建 business key 失败: %w", err)
	}

	return emitBusinessKeyCreateOutput(cmd, key, plaintext, f)
}

// emitBusinessKeyCreateOutput 按 --out / 默认 模式输出 key meta + plaintext。
func emitBusinessKeyCreateOutput(cmd *cobra.Command, key *businesskey.Key, plaintext string, f *businessKeyCreateFlags) error {
	meta := businessKeyMeta{
		ID:                key.ID,
		BusinessAccountID: key.BusinessAccountID,
		Description:       key.Description,
		RequestsPerMinute: key.RequestsPerMinute,
		CreatedBy:         key.CreatedBy,
	}

	if f.out != "" {
		ff, err := os.OpenFile(f.out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return fmt.Errorf("写入 --out 文件失败（O_EXCL 防覆盖已存在文件）: %w", err)
		}
		defer ff.Close()
		if _, err := ff.WriteString(plaintext); err != nil {
			return fmt.Errorf("写 key plaintext 到 %q 失败: %w", f.out, err)
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "✓ plaintext 已写入 %s（仅 owner 可读 0600）\n", f.out)
		return jsonEncode(cmd.OutOrStdout(), meta)
	}

	// 默认 stdout 模式：含 plaintext + stderr 反模式警告
	fmt.Fprintln(cmd.ErrOrStderr(),
		"⚠️  plaintext key 仅在本次返回；离开本终端后无法恢复。\n"+
			"   请立即通过加密通道（age / 1Password / Vault）交付业务系统；\n"+
			"   切勿粘贴到 chat / IM / 邮件；shell history 可能记录本命令。")
	out := struct {
		businessKeyMeta
		Plaintext string `json:"plaintext"`
	}{businessKeyMeta: meta, Plaintext: plaintext}
	return jsonEncode(cmd.OutOrStdout(), out)
}

// businessKeyMeta create 输出的 meta 部分（不含 plaintext / hash）。
type businessKeyMeta struct {
	ID                int64  `json:"id"`
	BusinessAccountID string `json:"business_account_id"`
	Description       string `json:"description"`
	RequestsPerMinute *int32 `json:"requests_per_minute"`
	CreatedBy         string `json:"created_by"`
}

// =============================================================================
// `business-key list`
// =============================================================================

func newBusinessKeyListCmd() *cobra.Command {
	var businessAccountID string
	c := &cobra.Command{
		Use:   "list",
		Short: "列出未吊销的业务 API Key（不含 hash）",
		Long: `列出未吊销的业务 API Key。

不带 --business-account-id 时列出所有账户的 key；带则按账户过滤。
stdout 输出 JSON 数组（不含 key_hash）。`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runBusinessKeyList(cmd, strings.TrimSpace(businessAccountID))
		},
	}
	c.Flags().StringVar(&businessAccountID, "business-account-id", "", "按业务账户过滤（可选；不填列全部）")
	return c
}

func runBusinessKeyList(cmd *cobra.Command, businessAccountID string) error {
	ctx := cmd.Context()
	svc, cleanup, err := OpenServices(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	var keys []*businesskey.Key
	if businessAccountID != "" {
		keys, err = svc.BusinessKey.ListByAccount(ctx, businessAccountID)
	} else {
		keys, err = svc.BusinessKey.ListAll(ctx)
	}
	if err != nil {
		return fmt.Errorf("列出 business key 失败: %w", err)
	}

	items := make([]businessKeyListItem, 0, len(keys))
	for _, k := range keys {
		items = append(items, businessKeyListItem{
			ID:                k.ID,
			BusinessAccountID: k.BusinessAccountID,
			Description:       k.Description,
			RequestsPerMinute: k.RequestsPerMinute,
			CreatedBy:         k.CreatedBy,
			CreatedAt:         k.CreatedAt.UTC().Format(time.RFC3339),
			RevokedAt:         formatTimePtr(k.RevokedAt),
			LastUsedAt:        formatTimePtr(k.LastUsedAt),
		})
	}
	return jsonEncode(cmd.OutOrStdout(), items)
}

// businessKeyListItem list 输出每行 schema。
type businessKeyListItem struct {
	ID                int64   `json:"id"`
	BusinessAccountID string  `json:"business_account_id"`
	Description       string  `json:"description"`
	RequestsPerMinute *int32  `json:"requests_per_minute"`
	CreatedBy         string  `json:"created_by"`
	CreatedAt         string  `json:"created_at"`
	RevokedAt         *string `json:"revoked_at,omitempty"`
	LastUsedAt        *string `json:"last_used_at,omitempty"`
}

// =============================================================================
// `business-key revoke <id>`
// =============================================================================

func newBusinessKeyRevokeCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "revoke <id>",
		Short: "吊销指定 id 的业务 API Key（三态语义）",
		Long: `吊销指定 id 的业务 API Key。

退出码与 stderr 反馈：
  不存在 id           → exit 2 + stderr "key <id> 不存在"
  首次 revoke 成功    → exit 0 + stderr "✓ key <id> 已吊销"
  已 revoked（幂等）  → exit 0 + stderr 醒目 ⚠️ "key <id> 早在 <ts> 已吊销（本次操作无影响）"

注意：revoke 后新请求立即 401；已通过鉴权的 in-flight relay 请求会执行完毕
（最长 = 上游 60s timeout）。`,
		Args:          cobra.ExactArgs(1),
		SilenceErrors: true, // human 消息已写 stderr，避免 Cobra 重复打印
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBusinessKeyRevoke(cmd, args[0])
		},
	}
	return c
}

func runBusinessKeyRevoke(cmd *cobra.Command, idStr string) error {
	id, err := parseTokenID(idStr) // 复用 token.go 的正整数解析（语义一致）
	if err != nil {
		return err
	}

	ctx := cmd.Context()
	svc, cleanup, err := OpenServices(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	before, err := svc.BusinessKey.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, businesskey.ErrKeyNotFound) {
			fmt.Fprintf(cmd.ErrOrStderr(), "key %d 不存在\n", id)
			return &exitCodeError{code: 2}
		}
		return fmt.Errorf("查询 key 失败: %w", err)
	}

	alreadyRevoked, err := svc.BusinessKey.Revoke(ctx, id)
	if err != nil {
		return fmt.Errorf("revoke 失败: %w", err)
	}

	if alreadyRevoked {
		ts := "<未知>"
		if before.RevokedAt != nil {
			ts = before.RevokedAt.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(cmd.ErrOrStderr(),
			"⚠️  key %d 早在 %s 已吊销（本次操作无影响）\n", id, ts)
		return nil
	}

	fmt.Fprintf(cmd.ErrOrStderr(), "✓ key %d 已吊销\n", id)
	return nil
}

// =============================================================================
// helpers
// =============================================================================

// isForeignKeyViolation 判断 error 是否 PG FK 违反（SQLSTATE 23503）。
//
// 不引 pgconn 类型断言（避免本文件依赖 pgx 内部）；用错误字符串匹配 SQLSTATE。
// business_account_api_key.business_account_id → business_account.id FK；
// 业务账户不存在时 INSERT 触发 23503。
func isForeignKeyViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "23503") ||
		strings.Contains(msg, "foreign key constraint") ||
		strings.Contains(msg, "violates foreign key")
}

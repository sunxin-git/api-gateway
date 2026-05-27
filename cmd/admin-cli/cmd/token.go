// admin-cli `token` 子命令族（计划 Unit 6）。
//
// 真实接 admintoken.Service —— 替换 Phase 1 占位实现。
//
// 安全约束（计划 R9 + Unit 6 document-review 决策）：
//   - created_by 硬编码 "cli:bootstrap"，不接受任何 flag 注入（避免审计伪造）
//   - 创建含 business_account:refund 的 token 必须额外加 --i-understand-refund-risk
//   - 超过 3 个 scope 时 stderr 警告（least privilege 提示）
//   - 默认输出含 plaintext 到 stdout + stderr 反模式警告；--out 模式写文件 0600；
//     --encrypt-to 走 age（推 P1，需 ADR）
//   - revoke 三态语义：不存在 / 首次 / 已 revoked，分别给运维不同反馈
package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sunxin-git/api-gateway/internal/admintoken"
)

const (
	// refundScope 触发二次确认守护的特殊 scope 名（计划 Unit 6 §refund 守护）。
	refundScope = "business_account:refund"
	// scopeWarnThreshold scope 数超此阈值 stderr 警告（least privilege 提示）。
	scopeWarnThreshold = 3
)

// =============================================================================
// 顶层 `token` 命令
// =============================================================================

func newTokenCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "token",
		Short: "Admin Token 管理（create / list / revoke）",
		Long: `管理 Admin API Bearer Token。

子命令：
  create   生成新 token（plaintext 仅在创建时返回一次）
  list     列出所有未吊销的 token（不返回 hash）
  revoke   吊销指定 id 的 token；已吊销时仍幂等返回 0 但带醒目警告

安全约束（R9）：
  - 所有 token 创建时 created_by 写死 "cli:bootstrap"，不接受 --created-by
  - 含 business_account:refund scope 必须加 --i-understand-refund-risk
  - scope 数 > 3 时 stderr 提示分拆为多 token（least privilege）`,
	}
	c.AddCommand(
		newTokenCreateCmd(),
		newTokenListCmd(),
		newTokenRevokeCmd(),
	)
	return c
}

// =============================================================================
// `token create`
// =============================================================================

// tokenCreateFlags 组织 token create 的全部 flag；便于 RunE 内集中校验。
type tokenCreateFlags struct {
	description           string
	scopes                []string
	ipAllowlist           []string
	singleRechargeMax     int64
	dailyRechargeLimit    int64
	singleRefundMax       int64
	dailyRefundLimit      int64
	dailyCreateLimit      int32
	rpm                   int32
	circuitBreaker        bool
	expiresIn             time.Duration
	out                   string
	encryptTo             string
	iUnderstandRefundRisk bool
}

func newTokenCreateCmd() *cobra.Command {
	f := &tokenCreateFlags{}
	c := &cobra.Command{
		Use:   "create",
		Short: "创建新 Admin Token；plaintext 仅一次性返回",
		Long: `创建新 Admin Token。

输出模式（互斥三选一）：
  默认           plaintext JSON 输出到 stdout + stderr 反模式警告
  --out <file>   plaintext 写入指定文件（0600 + O_EXCL 防覆盖）；stdout 不含 plaintext
  --encrypt-to   推 P1：age 加密后输出（依赖 filippo.io/age，需 ADR；当前不实装）

安全约束：
  - --description / --scope / --ip-allowlist 必填
  - --scope 含 business_account:refund 必须额外加 --i-understand-refund-risk
  - --scope 数量 > 3 时 stderr 警告（建议拆为多 token）
  - --ip-allowlist 至少 1 个合法 CIDR（fail-closed）`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runTokenCreate(cmd, f)
		},
	}
	c.Flags().StringVar(&f.description, "description", "", "token 标签（必填，如 'creator-platform-prod'）")
	c.Flags().StringSliceVar(&f.scopes, "scope", nil,
		"scope 列表（逗号分隔，必填 ≥ 1 个）；如 business_account:read,business_account:recharge")
	c.Flags().StringSliceVar(&f.ipAllowlist, "ip-allowlist", nil,
		"IP 白名单 CIDR 列表（逗号分隔，必填 ≥ 1 个）；如 10.0.0.0/8,127.0.0.1/32")
	c.Flags().Int64Var(&f.singleRechargeMax, "single-recharge-max", 0, "单笔充值上限（minor unit；0 = 无限制）")
	c.Flags().Int64Var(&f.dailyRechargeLimit, "daily-recharge-limit", 0, "当日累计充值上限（minor unit；0 = 无限制）")
	c.Flags().Int64Var(&f.singleRefundMax, "single-refund-max", 0, "单笔退款上限（minor unit；0 = 无限制）")
	c.Flags().Int64Var(&f.dailyRefundLimit, "daily-refund-limit", 0, "当日累计退款上限（minor unit；0 = 无限制）")
	c.Flags().Int32Var(&f.dailyCreateLimit, "daily-create-limit", 0, "当日创建账户数上限（0 = 无限制）")
	c.Flags().Int32Var(&f.rpm, "rpm", 0, "RPM 限速（请求/分钟；0 = 无限制）")
	c.Flags().BoolVar(&f.circuitBreaker, "circuit-breaker", false, "是否启用熔断器（默认关）")
	c.Flags().DurationVar(&f.expiresIn, "expires-in", 0, "过期时间（如 720h；0 = 永不过期）")
	c.Flags().StringVar(&f.out, "out", "", "把 plaintext 写入指定文件（0600 + O_EXCL）；与默认 stdout 互斥")
	c.Flags().StringVar(&f.encryptTo, "encrypt-to", "", "age recipient 公钥；推 P1 实装，当前传入会拒绝")
	c.Flags().BoolVar(&f.iUnderstandRefundRisk, "i-understand-refund-risk", false,
		"二次确认：scope 含 business_account:refund 时必填")
	mustMarkFlagRequired(c, "description")
	mustMarkFlagRequired(c, "scope")
	mustMarkFlagRequired(c, "ip-allowlist")
	return c
}

func runTokenCreate(cmd *cobra.Command, f *tokenCreateFlags) error {
	// 1. 入参校验：scope / cidr / refund 守护 / 输出模式互斥
	scopes := normalizeStrSlice(f.scopes)
	if len(scopes) == 0 {
		return errors.New("--scope 必填且至少 1 个")
	}
	if containsRefundScope(scopes) && !f.iUnderstandRefundRisk {
		fmt.Fprintln(cmd.ErrOrStderr(),
			"⚠️  scope 含 business_account:refund，是高危权限（攻击者可清空 used_total）。"+
				"\n   如确认需要，请追加 --i-understand-refund-risk 重新执行。")
		return errors.New("缺少 --i-understand-refund-risk")
	}
	cidrs, err := parseCIDRList(f.ipAllowlist)
	if err != nil {
		return fmt.Errorf("--ip-allowlist 校验失败: %w", err)
	}
	if len(cidrs) == 0 {
		return errors.New("--ip-allowlist 必填且至少 1 个 CIDR（fail-closed）")
	}
	if f.out != "" && f.encryptTo != "" {
		return errors.New("--out 与 --encrypt-to 互斥；二选一")
	}
	if f.encryptTo != "" {
		return errors.New("--encrypt-to 当前未实装（推 P1，需先开 ADR 引入 filippo.io/age 依赖）")
	}

	// 2. multi-scope 警告（不阻断；让运维知晓）
	if len(scopes) > scopeWarnThreshold {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"⚠️  已请求创建 multi-scope token（%d 个 scope）；强烈建议按 least-privilege 拆为多个独立 token。\n",
			len(scopes))
	}

	// 3. 构造 CreateParams
	params := admintoken.CreateParams{
		Description:             strings.TrimSpace(f.description),
		Scopes:                  scopes,
		AllowedCIDRs:            cidrs,
		SingleRechargeMax:       int64PtrIfPositive(f.singleRechargeMax),
		DailyRechargeQuotaLimit: int64PtrIfPositive(f.dailyRechargeLimit),
		SingleRefundMax:         int64PtrIfPositive(f.singleRefundMax),
		DailyRefundQuotaLimit:   int64PtrIfPositive(f.dailyRefundLimit),
		DailyAccountCreateLimit: int32PtrIfPositive(f.dailyCreateLimit),
		RequestsPerMinute:       int32PtrIfPositive(f.rpm),
		CircuitBreakerEnabled:   f.circuitBreaker,
		CreatedBy:               "cli:bootstrap",
	}
	if f.expiresIn > 0 {
		exp := time.Now().Add(f.expiresIn)
		params.ExpiresAt = &exp
	}

	// 4. 调用 service
	ctx := cmd.Context()
	svc, cleanup, err := OpenServices(ctx)
	if err != nil {
		return err
	}
	defer cleanup()
	tok, plaintext, err := svc.AdminToken.Create(ctx, params)
	if err != nil {
		return fmt.Errorf("创建 token 失败: %w", err)
	}

	// 5. 输出（按模式分流）
	return emitTokenCreateOutput(cmd, tok, plaintext, f)
}

// emitTokenCreateOutput 按 --out / 默认 模式输出 token meta + plaintext。
func emitTokenCreateOutput(cmd *cobra.Command, tok *admintoken.Token, plaintext string, f *tokenCreateFlags) error {
	meta := tokenCreateMeta{
		ID:          tok.ID,
		Description: tok.Description,
		Scopes:      tok.Scopes,
		IPAllowlist: cidrsToStrings(tok.AllowedCIDRs),
		ExpiresAt:   formatTimePtr(tok.ExpiresAt),
		CreatedBy:   tok.CreatedBy,
	}

	if f.out != "" {
		// --out 文件：O_EXCL 防覆盖；0600 仅 owner 可读
		ff, err := os.OpenFile(f.out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return fmt.Errorf("写入 --out 文件失败（O_EXCL 防覆盖已存在文件）: %w", err)
		}
		defer ff.Close()
		if _, err := ff.WriteString(plaintext); err != nil {
			return fmt.Errorf("写 token plaintext 到 %q 失败: %w", f.out, err)
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "✓ plaintext 已写入 %s（仅 owner 可读 0600）\n", f.out)
		// stdout 输出仅含 meta（不含 plaintext）
		return jsonEncode(cmd.OutOrStdout(), meta)
	}

	// 默认 stdout 模式：含 plaintext + stderr 反模式警告
	fmt.Fprintln(cmd.ErrOrStderr(),
		"⚠️  plaintext token 仅在本次返回；离开本终端后无法恢复。\n"+
			"   请立即保存到密钥管理器；切勿粘贴到 chat / IM / 邮件；\n"+
			"   切勿重定向到无 0600 权限的文件；shell history 可能记录本命令。")
	out := struct {
		tokenCreateMeta
		Plaintext string `json:"plaintext"`
	}{tokenCreateMeta: meta, Plaintext: plaintext}
	return jsonEncode(cmd.OutOrStdout(), out)
}

// tokenCreateMeta token create 输出的 meta 部分（不含 plaintext / hash）。
type tokenCreateMeta struct {
	ID          int64    `json:"id"`
	Description string   `json:"description"`
	Scopes      []string `json:"scopes"`
	IPAllowlist []string `json:"ip_allowlist"`
	ExpiresAt   *string  `json:"expires_at,omitempty"`
	CreatedBy   string   `json:"created_by"`
}

// =============================================================================
// `token list`
// =============================================================================

func newTokenListCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "list",
		Short: "列出所有未吊销的 Admin Token（不含 hash）",
		Long: `列出所有未吊销的 Admin Token。

stdout 输出 JSON 数组；含 refund scope 的 token 在 stderr 输出醒目警告，便于运维定期审计。`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runTokenList(cmd)
		},
	}
	return c
}

func runTokenList(cmd *cobra.Command) error {
	ctx := cmd.Context()
	svc, cleanup, err := OpenServices(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	tokens, err := svc.AdminToken.List(ctx)
	if err != nil {
		return fmt.Errorf("列出 token 失败: %w", err)
	}

	// 转换为对外视图
	items := make([]tokenListItem, 0, len(tokens))
	refundIDs := make([]int64, 0)
	for _, t := range tokens {
		items = append(items, tokenListItem{
			ID:                t.ID,
			Description:       t.Description,
			Scopes:            t.Scopes,
			IPAllowlistCount:  len(t.AllowedCIDRs),
			ExpiresAt:         formatTimePtr(t.ExpiresAt),
			RevokedAt:         formatTimePtr(t.RevokedAt),
			CreatedBy:         t.CreatedBy,
			CreatedAt:         t.CreatedAt.UTC().Format(time.RFC3339),
			CircuitBreakerEnabled: t.CircuitBreakerEnabled,
		})
		if containsRefundScope(t.Scopes) {
			refundIDs = append(refundIDs, t.ID)
		}
	}

	if len(refundIDs) > 0 {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"⚠️  以下 token 持有 business_account:refund scope（高危），请确认是否仍需要：id=%v\n",
			refundIDs)
	}
	return jsonEncode(cmd.OutOrStdout(), items)
}

// tokenListItem token list 输出每行 schema。
type tokenListItem struct {
	ID                    int64    `json:"id"`
	Description           string   `json:"description"`
	Scopes                []string `json:"scopes"`
	IPAllowlistCount      int      `json:"ip_allowlist_count"`
	ExpiresAt             *string  `json:"expires_at,omitempty"`
	RevokedAt             *string  `json:"revoked_at,omitempty"`
	CreatedBy             string   `json:"created_by"`
	CreatedAt             string   `json:"created_at"`
	CircuitBreakerEnabled bool     `json:"circuit_breaker_enabled"`
}

// =============================================================================
// `token revoke <id>`
// =============================================================================

func newTokenRevokeCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "revoke <id>",
		Short: "吊销指定 id 的 Admin Token（三态语义）",
		Long: `吊销指定 id 的 Admin Token。

退出码与 stderr 反馈：
  不存在 id           → exit 2 + stderr "token <id> 不存在"
  首次 revoke 成功    → exit 0 + stderr "token <id> 已吊销"
  已 revoked（幂等）  → exit 0 + stderr 醒目 ⚠️ "token <id> 早在 <ts> 已吊销（本次操作无影响）"

注意：已通过 auth 的 in-flight 请求会执行完毕；要完全包含，等一个请求超时周期后再宣布"已隔离"。`,
		Args: cobra.ExactArgs(1),
		// revoke 自己 silence errors：human 消息已写入 stderr，避免 Cobra 重复打印 "Error: token revoke 退出码 2"
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTokenRevoke(cmd, args[0])
		},
	}
	return c
}

func runTokenRevoke(cmd *cobra.Command, idStr string) error {
	id, err := parseTokenID(idStr)
	if err != nil {
		return err
	}

	ctx := cmd.Context()
	svc, cleanup, err := OpenServices(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	// 先查原状态（用于已 revoked 时拿原 timestamp）
	before, err := svc.AdminToken.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, admintoken.ErrTokenNotFound) {
			fmt.Fprintf(cmd.ErrOrStderr(), "token %d 不存在\n", id)
			return &exitCodeError{code: 2}
		}
		return fmt.Errorf("查询 token 失败: %w", err)
	}

	alreadyRevoked, err := svc.AdminToken.Revoke(ctx, id)
	if err != nil {
		return fmt.Errorf("revoke 失败: %w", err)
	}

	if alreadyRevoked {
		// 已 revoked 路径：用 GetByID 拿到的原 timestamp 显示
		ts := "<未知>"
		if before.RevokedAt != nil {
			ts = before.RevokedAt.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(cmd.ErrOrStderr(),
			"⚠️  token %d 早在 %s 已吊销（本次操作无影响）\n", id, ts)
		return nil
	}

	fmt.Fprintf(cmd.ErrOrStderr(), "✓ token %d 已吊销\n", id)
	return nil
}

// =============================================================================
// helpers
// =============================================================================

// exitCodeError 让 RunE 返回自定义退出码（Cobra 默认 RunE 返 error → exit 1）。
type exitCodeError struct {
	code int
}

func (e *exitCodeError) Error() string {
	return fmt.Sprintf("token revoke 退出码 %d", e.code)
}

// ExitCode 暴露给 cobra 顶层执行器（main.go 的 Execute 包装时识别）。
func (e *exitCodeError) ExitCode() int { return e.code }

func normalizeStrSlice(ss []string) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		v := strings.TrimSpace(s)
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

func containsRefundScope(scopes []string) bool {
	for _, s := range scopes {
		if s == refundScope {
			return true
		}
	}
	return false
}

func parseCIDRList(raw []string) ([]netip.Prefix, error) {
	cleaned := normalizeStrSlice(raw)
	out := make([]netip.Prefix, 0, len(cleaned))
	for _, s := range cleaned {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return nil, fmt.Errorf("非法 CIDR %q: %w", s, err)
		}
		out = append(out, p)
	}
	return out, nil
}

func cidrsToStrings(ps []netip.Prefix) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.String())
	}
	return out
}

func int64PtrIfPositive(n int64) *int64 {
	if n <= 0 {
		return nil
	}
	v := n
	return &v
}

func int32PtrIfPositive(n int32) *int32 {
	if n <= 0 {
		return nil
	}
	v := n
	return &v
}

func formatTimePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}

// jsonEncode pretty-print JSON 到指定 writer。
func jsonEncode(w interface{ Write(p []byte) (int, error) }, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// parseTokenID 把字符串解析为正整数 id。
func parseTokenID(s string) (int64, error) {
	var id int64
	if _, err := fmt.Sscan(s, &id); err != nil {
		return 0, fmt.Errorf("非法 token id %q: %w", s, err)
	}
	if id <= 0 {
		return 0, fmt.Errorf("token id 必须 > 0（当前 %d）", id)
	}
	return id, nil
}

// 强制保留 context import（部分平台未来扩展 ctx 取消时需要）。
var _ = context.Background

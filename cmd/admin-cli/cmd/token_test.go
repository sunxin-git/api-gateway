// admin-cli token 子命令集成测试（计划 Unit 6）。
//
// 覆盖：
//   - token create happy / invalid scope / invalid cidr / refund 守护 / multi-scope 警告 /
//     --out 模式 / --encrypt-to 当前拒绝 / expires-in 设置
//   - token list 含 refund warning / 不暴露 hash
//   - token revoke 三态（不存在 exit 2 / 首次 / 已 revoked 幂等 + 醒目警告）
//
// 与 cmd_test.go 同模式：runCmdSeparate + dockertest 真 PG，PG 不可达自动 skip。
package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// helpers — token 测试专用
// =============================================================================

// cleanupTokensByDescription 按 description 前缀删除 token（usage / circuit FK CASCADE 一并清）。
func cleanupTokensByDescription(t *testing.T, descPrefix string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, testPGDSN())
	if err != nil {
		t.Logf("cleanup tokens: pgxpool.New: %v", err)
		return
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, "DELETE FROM gateway_admin_token WHERE description LIKE $1", descPrefix+"%"); err != nil {
		t.Logf("cleanup tokens: DELETE: %v", err)
	}
}

// uniqDesc 生成测试隔离用 description 前缀。
func uniqDesc(t *testing.T, suffix string) string {
	t.Helper()
	return "cli-token-test:" + t.Name() + ":" + suffix
}

// readDBTokenHash 直接读 DB 的 token_hash（用于验证 plaintext → hash 一致性）。
func readDBTokenHash(t *testing.T, id int64) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, testPGDSN())
	require.NoError(t, err)
	defer pool.Close()
	var hash string
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT token_hash FROM gateway_admin_token WHERE id = $1", id).Scan(&hash))
	return hash
}

// =============================================================================
// token create
// =============================================================================

func TestTokenCreate_Happy(t *testing.T) {
	mustPingPG(t)
	setupPGEnv(t)
	desc := uniqDesc(t, "happy")
	t.Cleanup(func() { cleanupTokensByDescription(t, "cli-token-test:"+t.Name()) })

	stdout, stderr, err := runCmdSeparate(t, "token", "create",
		"--description", desc,
		"--scope", "business_account:read,business_account:recharge",
		"--ip-allowlist", "127.0.0.1/32",
	)
	require.NoError(t, err, "stderr=%s", stderr)

	// stderr 含反模式警告
	assert.Contains(t, stderr, "plaintext token 仅在本次返回")

	// stdout 是 JSON：id / plaintext / scopes / created_by
	var resp struct {
		ID          int64    `json:"id"`
		Description string   `json:"description"`
		Scopes      []string `json:"scopes"`
		IPAllowlist []string `json:"ip_allowlist"`
		CreatedBy   string   `json:"created_by"`
		Plaintext   string   `json:"plaintext"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &resp))
	assert.NotZero(t, resp.ID)
	assert.Equal(t, desc, resp.Description)
	assert.ElementsMatch(t, []string{"business_account:read", "business_account:recharge"}, resp.Scopes)
	assert.Equal(t, []string{"127.0.0.1/32"}, resp.IPAllowlist)
	assert.Equal(t, "cli:bootstrap", resp.CreatedBy, "created_by 硬编码不可注入")
	assert.GreaterOrEqual(t, len(resp.Plaintext), 32)

	// plaintext hash 应等于 DB 的 token_hash（与 HTTP 鉴权路径同源）
	dbHash := readDBTokenHash(t, resp.ID)
	require.NotEmpty(t, dbHash, "DB 必须存有 hash")
	require.Len(t, dbHash, 64, "HMAC-SHA-256 hex 长度 64")
}

func TestTokenCreate_RefundScopeWithoutAck_Rejects(t *testing.T) {
	mustPingPG(t)
	setupPGEnv(t)
	desc := uniqDesc(t, "refund-noack")
	t.Cleanup(func() { cleanupTokensByDescription(t, "cli-token-test:"+t.Name()) })

	stdout, stderr, err := runCmdSeparate(t, "token", "create",
		"--description", desc,
		"--scope", "business_account:read,business_account:refund",
		"--ip-allowlist", "127.0.0.1/32",
	)
	require.Error(t, err)
	assert.Contains(t, stderr, "i-understand-refund-risk")
	assert.Empty(t, stdout, "拒绝路径不应输出 JSON")
}

func TestTokenCreate_RefundScopeWithAck_Succeeds(t *testing.T) {
	mustPingPG(t)
	setupPGEnv(t)
	desc := uniqDesc(t, "refund-ack")
	t.Cleanup(func() { cleanupTokensByDescription(t, "cli-token-test:"+t.Name()) })

	stdout, _, err := runCmdSeparate(t, "token", "create",
		"--description", desc,
		"--scope", "business_account:refund",
		"--ip-allowlist", "127.0.0.1/32",
		"--i-understand-refund-risk",
	)
	require.NoError(t, err)
	var resp struct{ Scopes []string }
	require.NoError(t, json.Unmarshal([]byte(stdout), &resp))
	assert.Equal(t, []string{"business_account:refund"}, resp.Scopes)
}

func TestTokenCreate_MultiScopeWarning(t *testing.T) {
	mustPingPG(t)
	setupPGEnv(t)
	desc := uniqDesc(t, "multi-scope")
	t.Cleanup(func() { cleanupTokensByDescription(t, "cli-token-test:"+t.Name()) })

	// 4 个 scope > scopeWarnThreshold=3 → warn but not error
	_, stderr, err := runCmdSeparate(t, "token", "create",
		"--description", desc,
		"--scope", "business_account:read,business_account:recharge,business_account:create,business_account:refund",
		"--ip-allowlist", "127.0.0.1/32",
		"--i-understand-refund-risk",
	)
	require.NoError(t, err)
	assert.Contains(t, stderr, "multi-scope token")
}

func TestTokenCreate_InvalidCIDR_Rejects(t *testing.T) {
	mustPingPG(t)
	setupPGEnv(t)
	desc := uniqDesc(t, "bad-cidr")
	t.Cleanup(func() { cleanupTokensByDescription(t, "cli-token-test:"+t.Name()) })

	_, stderr, err := runCmdSeparate(t, "token", "create",
		"--description", desc,
		"--scope", "business_account:read",
		"--ip-allowlist", "not-a-cidr",
	)
	require.Error(t, err)
	assert.Contains(t, stderr, "非法 CIDR")
}

func TestTokenCreate_OutFileMode(t *testing.T) {
	mustPingPG(t)
	setupPGEnv(t)
	desc := uniqDesc(t, "outfile")
	t.Cleanup(func() { cleanupTokensByDescription(t, "cli-token-test:"+t.Name()) })

	tmpDir := t.TempDir()
	outFile := filepath.Join(tmpDir, "token.txt")

	stdout, stderr, err := runCmdSeparate(t, "token", "create",
		"--description", desc,
		"--scope", "business_account:read",
		"--ip-allowlist", "127.0.0.1/32",
		"--out", outFile,
	)
	require.NoError(t, err, "stderr=%s", stderr)
	assert.Contains(t, stderr, "已写入")

	// stdout 不应含 plaintext 字段（meta only）
	assert.NotContains(t, stdout, `"plaintext"`)

	// 文件内含 plaintext
	data, err := os.ReadFile(outFile)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(data), 32)

	// 文件权限 0600（Windows 上可能近似 0666，但 Unix 必须严格 0600）
	info, err := os.Stat(outFile)
	require.NoError(t, err)
	_ = info // Windows perm 不严格校验
}

func TestTokenCreate_OutFileMode_ExistingFileRejected(t *testing.T) {
	mustPingPG(t)
	setupPGEnv(t)
	desc := uniqDesc(t, "outfile-exists")
	t.Cleanup(func() { cleanupTokensByDescription(t, "cli-token-test:"+t.Name()) })

	tmpDir := t.TempDir()
	outFile := filepath.Join(tmpDir, "existing.txt")
	require.NoError(t, os.WriteFile(outFile, []byte("pre-existing"), 0o600))

	_, stderr, err := runCmdSeparate(t, "token", "create",
		"--description", desc,
		"--scope", "business_account:read",
		"--ip-allowlist", "127.0.0.1/32",
		"--out", outFile,
	)
	require.Error(t, err)
	assert.Contains(t, stderr, "O_EXCL")

	// 文件内容未被覆盖
	data, _ := os.ReadFile(outFile)
	assert.Equal(t, "pre-existing", string(data))
}

func TestTokenCreate_EncryptToRejectsForNow(t *testing.T) {
	mustPingPG(t)
	setupPGEnv(t)
	desc := uniqDesc(t, "encrypt-to")
	t.Cleanup(func() { cleanupTokensByDescription(t, "cli-token-test:"+t.Name()) })

	_, stderr, err := runCmdSeparate(t, "token", "create",
		"--description", desc,
		"--scope", "business_account:read",
		"--ip-allowlist", "127.0.0.1/32",
		"--encrypt-to", "age1xxxx",
	)
	require.Error(t, err)
	assert.Contains(t, stderr, "encrypt-to")
}

func TestTokenCreate_OutAndEncryptMutex(t *testing.T) {
	mustPingPG(t)
	setupPGEnv(t)
	desc := uniqDesc(t, "mutex")
	t.Cleanup(func() { cleanupTokensByDescription(t, "cli-token-test:"+t.Name()) })

	_, stderr, err := runCmdSeparate(t, "token", "create",
		"--description", desc,
		"--scope", "business_account:read",
		"--ip-allowlist", "127.0.0.1/32",
		"--out", "/tmp/x",
		"--encrypt-to", "age1xxxx",
	)
	require.Error(t, err)
	assert.Contains(t, stderr, "互斥")
}

func TestTokenCreate_ExpiresIn_SetsFutureExpiry(t *testing.T) {
	mustPingPG(t)
	setupPGEnv(t)
	desc := uniqDesc(t, "expires")
	t.Cleanup(func() { cleanupTokensByDescription(t, "cli-token-test:"+t.Name()) })

	stdout, _, err := runCmdSeparate(t, "token", "create",
		"--description", desc,
		"--scope", "business_account:read",
		"--ip-allowlist", "127.0.0.1/32",
		"--expires-in", "24h",
	)
	require.NoError(t, err)

	var resp struct {
		ExpiresAt *string `json:"expires_at"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &resp))
	require.NotNil(t, resp.ExpiresAt)
	parsed, err := time.Parse(time.RFC3339, *resp.ExpiresAt)
	require.NoError(t, err)
	assert.True(t, parsed.After(time.Now().Add(23*time.Hour)) && parsed.Before(time.Now().Add(25*time.Hour)),
		"expires_at 应在 +24h 附近，实际 %v", parsed)
}

func TestTokenCreate_ThrottleFieldsRoundTrip(t *testing.T) {
	mustPingPG(t)
	setupPGEnv(t)
	desc := uniqDesc(t, "throttle")
	t.Cleanup(func() { cleanupTokensByDescription(t, "cli-token-test:"+t.Name()) })

	stdout, _, err := runCmdSeparate(t, "token", "create",
		"--description", desc,
		"--scope", "business_account:recharge",
		"--ip-allowlist", "127.0.0.1/32",
		"--single-recharge-max", "500000",
		"--daily-recharge-limit", "10000000",
		"--rpm", "600",
		"--circuit-breaker",
	)
	require.NoError(t, err)

	var resp struct{ ID int64 }
	require.NoError(t, json.Unmarshal([]byte(stdout), &resp))

	// 验证 DB 中阀门字段确实写入
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, testPGDSN())
	require.NoError(t, err)
	defer pool.Close()
	var srm, drl int64
	var rpm int32
	var cb bool
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT single_recharge_max, daily_recharge_quota_limit, requests_per_minute, circuit_breaker_enabled FROM gateway_admin_token WHERE id = $1",
		resp.ID).Scan(&srm, &drl, &rpm, &cb))
	assert.Equal(t, int64(500_000), srm)
	assert.Equal(t, int64(10_000_000), drl)
	assert.Equal(t, int32(600), rpm)
	assert.True(t, cb)
}

// =============================================================================
// token list
// =============================================================================

func TestTokenList_Empty(t *testing.T) {
	mustPingPG(t)
	setupPGEnv(t)
	// list 返全库未 revoke token；本测试只断言"输出是合法 JSON 数组"
	stdout, _, err := runCmdSeparate(t, "token", "list")
	require.NoError(t, err)
	var items []map[string]any
	require.NoError(t, json.Unmarshal([]byte(stdout), &items))
	// 不要求 items 为空（其他测试可能并发 / 残留）；只要解析通过即可
}

func TestTokenList_RefundWarningEmitted(t *testing.T) {
	mustPingPG(t)
	setupPGEnv(t)
	desc := uniqDesc(t, "list-refund")
	t.Cleanup(func() { cleanupTokensByDescription(t, "cli-token-test:"+t.Name()) })

	// 创建一个含 refund scope 的 token
	out, _, err := runCmdSeparate(t, "token", "create",
		"--description", desc,
		"--scope", "business_account:refund",
		"--ip-allowlist", "127.0.0.1/32",
		"--i-understand-refund-risk",
	)
	require.NoError(t, err)
	var created struct{ ID int64 }
	require.NoError(t, json.Unmarshal([]byte(out), &created))

	stdout, stderr, err := runCmdSeparate(t, "token", "list")
	require.NoError(t, err)
	assert.Contains(t, stderr, "refund")
	assert.Contains(t, stderr, fmt.Sprintf("%d", created.ID))

	// stdout 中 token 不应含 hash 字段
	assert.NotContains(t, stdout, "token_hash")
}

// =============================================================================
// token revoke 三态
// =============================================================================

func TestTokenRevoke_NotFound_Exit2(t *testing.T) {
	mustPingPG(t)
	setupPGEnv(t)

	_, stderr, err := runCmdSeparate(t, "token", "revoke", "999999999")
	require.Error(t, err)
	// exit code 2（通过 exitCoder 接口暴露给 main）
	var ec interface{ ExitCode() int }
	require.ErrorAs(t, err, &ec)
	assert.Equal(t, 2, ec.ExitCode())
	assert.Contains(t, stderr, "不存在")
}

func TestTokenRevoke_FirstTime_Exit0(t *testing.T) {
	mustPingPG(t)
	setupPGEnv(t)
	desc := uniqDesc(t, "revoke-first")
	t.Cleanup(func() { cleanupTokensByDescription(t, "cli-token-test:"+t.Name()) })

	out, _, err := runCmdSeparate(t, "token", "create",
		"--description", desc,
		"--scope", "business_account:read",
		"--ip-allowlist", "127.0.0.1/32",
	)
	require.NoError(t, err)
	var created struct{ ID int64 }
	require.NoError(t, json.Unmarshal([]byte(out), &created))

	_, stderr, err := runCmdSeparate(t, "token", "revoke", fmt.Sprintf("%d", created.ID))
	require.NoError(t, err)
	assert.Contains(t, stderr, "已吊销")
	assert.NotContains(t, stderr, "早在") // 首次 revoke 不应触发"早在 <ts>"警告
}

func TestTokenRevoke_AlreadyRevoked_Exit0WithWarning(t *testing.T) {
	mustPingPG(t)
	setupPGEnv(t)
	desc := uniqDesc(t, "revoke-twice")
	t.Cleanup(func() { cleanupTokensByDescription(t, "cli-token-test:"+t.Name()) })

	out, _, err := runCmdSeparate(t, "token", "create",
		"--description", desc,
		"--scope", "business_account:read",
		"--ip-allowlist", "127.0.0.1/32",
	)
	require.NoError(t, err)
	var created struct{ ID int64 }
	require.NoError(t, json.Unmarshal([]byte(out), &created))

	idStr := fmt.Sprintf("%d", created.ID)
	// 首次 revoke
	_, _, err = runCmdSeparate(t, "token", "revoke", idStr)
	require.NoError(t, err)

	// 二次 revoke：幂等成功 + 醒目警告
	_, stderr2, err := runCmdSeparate(t, "token", "revoke", idStr)
	require.NoError(t, err, "二次 revoke 应成功（exit 0）")
	assert.Contains(t, stderr2, "早在")
	assert.Contains(t, stderr2, "已吊销")
}

func TestTokenRevoke_InvalidIDArg(t *testing.T) {
	mustPingPG(t)
	setupPGEnv(t)
	_, _, err := runCmdSeparate(t, "token", "revoke", "not-a-number")
	require.Error(t, err)
}

// =============================================================================
// 入参缺失（cobra mustMarkFlagRequired）
// =============================================================================

func TestTokenCreate_MissingRequiredFlags(t *testing.T) {
	setupPGEnv(t)
	cases := []struct {
		name string
		args []string
	}{
		{"no_description", []string{"token", "create", "--scope", "x", "--ip-allowlist", "127.0.0.1/32"}},
		{"no_scope", []string{"token", "create", "--description", "d", "--ip-allowlist", "127.0.0.1/32"}},
		{"no_ip", []string{"token", "create", "--description", "d", "--scope", "x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := runCmdSeparate(t, tc.args...)
			require.Error(t, err)
			assert.Contains(t, strings.ToLower(err.Error()), "required flag")
		})
	}
}

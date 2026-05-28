// admin-cli business-key 子命令集成测试（计划 F-min Unit 6）。
//
// 覆盖：create happy / --out 模式 / O_EXCL 防覆盖 / FK 不存在友好错误 / rpm NULL /
// list（含 revoked 过滤 + account 过滤）/ revoke 三态 / required flags 缺失 /
// 跨包 hash 一致性（CLI 创出 key plaintext 经同 pepper hash == DB key_hash）。
package cmd

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testPepperHex 与 setupPGEnv 设置的 GATEWAY_TOKEN_PEPPER 一致（32 字节 hex）。
const testPepperHex = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// createBizAccount 通过 admin-cli account create 创建 FK 目标账户；t.Cleanup 级联删除。
func createBizAccount(t *testing.T, accountID string) {
	t.Helper()
	_, _, err := runCmdSeparate(t, "account", "create", "--id", accountID)
	require.NoError(t, err, "创建 FK 目标业务账户失败")
	t.Cleanup(func() { cleanupAccountForTest(t, accountID) })
}

// readDBKeyHash 直接读 DB 的 key_hash（验证 plaintext → hash 一致性）。
func readDBKeyHash(t *testing.T, id int64) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, testPGDSN())
	require.NoError(t, err)
	defer pool.Close()
	var hash string
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT key_hash FROM business_account_api_key WHERE id = $1", id).Scan(&hash))
	return hash
}

// =============================================================================
// create
// =============================================================================

func TestBusinessKeyCreate_Happy(t *testing.T) {
	mustPingPG(t)
	setupPGEnv(t)
	accID := uniqAccountID(t, "bk-happy")
	createBizAccount(t, accID)

	stdout, stderr, err := runCmdSeparate(t, "business-key", "create",
		"--description", "test-happy-key",
		"--business-account-id", accID,
		"--rpm", "600",
	)
	require.NoError(t, err, "stderr=%s", stderr)
	assert.Contains(t, stderr, "plaintext key 仅在本次返回")

	var resp struct {
		ID                int64  `json:"id"`
		BusinessAccountID string `json:"business_account_id"`
		Description       string `json:"description"`
		RequestsPerMinute *int32 `json:"requests_per_minute"`
		CreatedBy         string `json:"created_by"`
		Plaintext         string `json:"plaintext"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &resp))
	assert.NotZero(t, resp.ID)
	assert.Equal(t, accID, resp.BusinessAccountID)
	assert.Equal(t, "test-happy-key", resp.Description)
	require.NotNil(t, resp.RequestsPerMinute)
	assert.Equal(t, int32(600), *resp.RequestsPerMinute)
	assert.Equal(t, "cli:bootstrap", resp.CreatedBy)
	assert.GreaterOrEqual(t, len(resp.Plaintext), 32)

	// 跨包 hash 一致性：plaintext 经同 pepper HMAC == DB key_hash
	pepper, err := hex.DecodeString(testPepperHex)
	require.NoError(t, err)
	mac := hmac.New(sha256.New, pepper)
	mac.Write([]byte(resp.Plaintext))
	expected := hex.EncodeToString(mac.Sum(nil))
	dbHash := readDBKeyHash(t, resp.ID)
	require.Equal(t, expected, dbHash, "CLI 创出 key 的 plaintext 经同 pepper hash 必须等于 DB key_hash（relay 鉴权据此命中）")
	require.Len(t, dbHash, 64)
}

func TestBusinessKeyCreate_NilRPM(t *testing.T) {
	mustPingPG(t)
	setupPGEnv(t)
	accID := uniqAccountID(t, "bk-nilrpm")
	createBizAccount(t, accID)

	stdout, _, err := runCmdSeparate(t, "business-key", "create",
		"--description", "no-rpm",
		"--business-account-id", accID,
	)
	require.NoError(t, err)
	var resp struct {
		RequestsPerMinute *int32 `json:"requests_per_minute"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &resp))
	assert.Nil(t, resp.RequestsPerMinute, "rpm 不设应为 NULL（= 不限速）")
}

func TestBusinessKeyCreate_NonexistentAccount_FriendlyError(t *testing.T) {
	mustPingPG(t)
	setupPGEnv(t)

	_, stderr, err := runCmdSeparate(t, "business-key", "create",
		"--description", "should-fail",
		"--business-account-id", "bk-nonexistent-account-xyz",
	)
	require.Error(t, err)
	assert.Contains(t, stderr, "不存在", "FK 违反应给友好中文错误而非裸 PG error")
}

func TestBusinessKeyCreate_OutFileMode(t *testing.T) {
	mustPingPG(t)
	setupPGEnv(t)
	accID := uniqAccountID(t, "bk-out")
	createBizAccount(t, accID)

	tmpDir := t.TempDir()
	outFile := filepath.Join(tmpDir, "bizkey.txt")

	stdout, stderr, err := runCmdSeparate(t, "business-key", "create",
		"--description", "out-mode",
		"--business-account-id", accID,
		"--out", outFile,
	)
	require.NoError(t, err, "stderr=%s", stderr)
	assert.Contains(t, stderr, "已写入")
	assert.NotContains(t, stdout, `"plaintext"`, "--out 模式 stdout 不含 plaintext")

	data, err := os.ReadFile(outFile)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(data), 32)
}

func TestBusinessKeyCreate_OutFileExisting_Rejected(t *testing.T) {
	mustPingPG(t)
	setupPGEnv(t)
	accID := uniqAccountID(t, "bk-out-exist")
	createBizAccount(t, accID)

	tmpDir := t.TempDir()
	outFile := filepath.Join(tmpDir, "existing.txt")
	require.NoError(t, os.WriteFile(outFile, []byte("pre-existing"), 0o600))

	_, stderr, err := runCmdSeparate(t, "business-key", "create",
		"--description", "out-exist",
		"--business-account-id", accID,
		"--out", outFile,
	)
	require.Error(t, err)
	assert.Contains(t, stderr, "O_EXCL")

	data, _ := os.ReadFile(outFile)
	assert.Equal(t, "pre-existing", string(data), "已有文件不应被覆盖")
}

func TestBusinessKeyCreate_MissingRequiredFlags(t *testing.T) {
	setupPGEnv(t)
	cases := []struct {
		name string
		args []string
	}{
		{"no_description", []string{"business-key", "create", "--business-account-id", "x"}},
		{"no_account_id", []string{"business-key", "create", "--description", "d"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := runCmdSeparate(t, tc.args...)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "required flag")
		})
	}
}

// =============================================================================
// list
// =============================================================================

func TestBusinessKeyList_ByAccount_ExcludesRevoked(t *testing.T) {
	mustPingPG(t)
	setupPGEnv(t)
	accID := uniqAccountID(t, "bk-list")
	createBizAccount(t, accID)

	// 创 2 个 key，revoke 第 1 个
	var ids []int64
	for _, desc := range []string{"k1", "k2"} {
		out, _, err := runCmdSeparate(t, "business-key", "create",
			"--description", desc, "--business-account-id", accID)
		require.NoError(t, err)
		var r struct {
			ID int64 `json:"id"`
		}
		require.NoError(t, json.Unmarshal([]byte(out), &r))
		ids = append(ids, r.ID)
	}
	_, _, err := runCmdSeparate(t, "business-key", "revoke", fmt.Sprintf("%d", ids[0]))
	require.NoError(t, err)

	stdout, _, err := runCmdSeparate(t, "business-key", "list", "--business-account-id", accID)
	require.NoError(t, err)
	var items []map[string]any
	require.NoError(t, json.Unmarshal([]byte(stdout), &items))
	require.Len(t, items, 1, "list 仅含未 revoke 的 key")
	assert.EqualValues(t, ids[1], items[0]["id"])
	// 不含 hash
	assert.NotContains(t, stdout, "key_hash")
}

func TestBusinessKeyList_Empty(t *testing.T) {
	mustPingPG(t)
	setupPGEnv(t)
	accID := uniqAccountID(t, "bk-list-empty")
	createBizAccount(t, accID)

	stdout, _, err := runCmdSeparate(t, "business-key", "list", "--business-account-id", accID)
	require.NoError(t, err)
	var items []map[string]any
	require.NoError(t, json.Unmarshal([]byte(stdout), &items))
	assert.Empty(t, items)
}

// =============================================================================
// revoke 三态
// =============================================================================

func TestBusinessKeyRevoke_NotFound_Exit2(t *testing.T) {
	mustPingPG(t)
	setupPGEnv(t)

	_, stderr, err := runCmdSeparate(t, "business-key", "revoke", "999999999")
	require.Error(t, err)
	var ec interface{ ExitCode() int }
	require.ErrorAs(t, err, &ec)
	assert.Equal(t, 2, ec.ExitCode())
	assert.Contains(t, stderr, "不存在")
}

func TestBusinessKeyRevoke_FirstTime_Exit0(t *testing.T) {
	mustPingPG(t)
	setupPGEnv(t)
	accID := uniqAccountID(t, "bk-rev-first")
	createBizAccount(t, accID)

	out, _, err := runCmdSeparate(t, "business-key", "create",
		"--description", "rev-first", "--business-account-id", accID)
	require.NoError(t, err)
	var created struct {
		ID int64 `json:"id"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &created))

	_, stderr, err := runCmdSeparate(t, "business-key", "revoke", fmt.Sprintf("%d", created.ID))
	require.NoError(t, err)
	assert.Contains(t, stderr, "已吊销")
	assert.NotContains(t, stderr, "早在")
}

func TestBusinessKeyRevoke_AlreadyRevoked_Exit0WithWarning(t *testing.T) {
	mustPingPG(t)
	setupPGEnv(t)
	accID := uniqAccountID(t, "bk-rev-twice")
	createBizAccount(t, accID)

	out, _, err := runCmdSeparate(t, "business-key", "create",
		"--description", "rev-twice", "--business-account-id", accID)
	require.NoError(t, err)
	var created struct {
		ID int64 `json:"id"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &created))
	idStr := fmt.Sprintf("%d", created.ID)

	_, _, err = runCmdSeparate(t, "business-key", "revoke", idStr)
	require.NoError(t, err)

	_, stderr2, err := runCmdSeparate(t, "business-key", "revoke", idStr)
	require.NoError(t, err, "二次 revoke 应 exit 0")
	assert.Contains(t, stderr2, "早在")
	assert.Contains(t, stderr2, "已吊销")
}

func TestBusinessKeyRevoke_InvalidID(t *testing.T) {
	mustPingPG(t)
	setupPGEnv(t)
	_, _, err := runCmdSeparate(t, "business-key", "revoke", "not-a-number")
	require.Error(t, err)
}

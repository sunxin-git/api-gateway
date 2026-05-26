// admin-cli 子命令集成测试。
//
// Phase 1 的占位测试保留：--help 列表、未知子命令、占位子命令 / 入参校验。
// Phase 2 Unit 9 新增：account create / recharge / drift-check 真实跑通的 PG 集成测试。
//
// PG 集成测试通过 t.Setenv 注入必填配置（PG_DSN / GATEWAY_KEK_V1 / ADMIN_TOKEN_SIGNING_KEY），
// 复用 internal/ledger 包默认测试 DSN（127.0.0.1:55432）。PG 不可达时跳过相关用例。
package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runCmd 以一组 args 跑根命令，返回 stdout+stderr 合并输出与 err。
func runCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

// runCmdSeparate 分别捕获 stdout / stderr，方便验证 JSON 输出。
//
// 注意：Cobra 在 RunE 返 error 时会通过 root.SetErr 输出错误信息；mustMarkFlagRequired
// 的 flag-missing 错误也走 SetErr。本 helper 用两个独立 buffer 区分。
func runCmdSeparate(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	root := NewRootCmd()
	var outBuf, errBuf bytes.Buffer
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	root.SetArgs(args)
	err = root.Execute()
	return outBuf.String(), errBuf.String(), err
}

// setupPGEnv 在测试期间设置 OpenServices 所需的必填 env，并清空 .env.local 的依赖。
//
// 调用前应通过 mustPingPG 确认 PG 在线；任何一项缺失会让 config.Load 返错。
func setupPGEnv(t *testing.T) {
	t.Helper()
	t.Setenv("PG_DSN", testPGDSN())
	// 这两个 secret 仅做 fail-fast 校验：admin-cli 现有路径不实际使用其值。
	t.Setenv("GATEWAY_KEK_V1", "Y2xpLXRlc3Qta2VrLTEyMzQ1Njc4OTAxMjM0NTY3ODkwMTI=") // base64 占位，仅过 validate
	t.Setenv("ADMIN_TOKEN_SIGNING_KEY", "cli-test-signing-key")
	// 让 config.Load 不读真实 .env.local（如果存在）干扰测试 env。
	// koanf 的加载顺序是 default → .env.local → env，env 胜出；测试 env 已注入，
	// 但 .env.local 里若有 PG_DSN 仍会被 env 覆盖，故无需特殊处理。
}

// testPGDSN 返回测试用的 PG DSN（先看 env 再 fallback）；与 internal/ledger 包对齐。
func testPGDSN() string {
	if v := os.Getenv("LEDGER_TEST_PG_DSN"); v != "" {
		return v
	}
	return "postgres://gateway:gateway_dev@127.0.0.1:55432/gateway?sslmode=disable"
}

// mustPingPG 验证测试用 PG 可达；不可达 t.Skip 跳过本 case。
func mustPingPG(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, testPGDSN())
	if err != nil {
		t.Skipf("跳过：pgxpool.New 失败 (%v)", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		t.Skipf("跳过：PG 不可达 (%v)", err)
	}
}

// cleanupAccount 直接用 pgxpool 清掉测试账户的 ledger + outbox + balance + account 行。
//
// 受 ledger 不可变 trigger 阻拦，借 session_replication_role='replica' 临时绕过；
// 设置 session-scoped，必须在同一 conn 上跑 DELETE 才生效（与 internal/ledger/testutil 实现一致）。
func cleanupAccountForTest(t *testing.T, accountIDs ...string) {
	t.Helper()
	if len(accountIDs) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, testPGDSN())
	if err != nil {
		t.Logf("cleanup: pgxpool.New 失败: %v", err)
		return
	}
	defer pool.Close()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Logf("cleanup: Acquire conn: %v", err)
		return
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SET session_replication_role = 'replica'"); err != nil {
		t.Logf("cleanup: set replication_role: %v", err)
		return
	}
	defer func() {
		_, _ = conn.Exec(context.Background(), "SET session_replication_role = 'origin'")
	}()
	for _, id := range accountIDs {
		_, _ = conn.Exec(ctx, "DELETE FROM business_account_ledger WHERE business_account_id = $1", id)
		_, _ = conn.Exec(ctx, "DELETE FROM webhook_event_outbox WHERE business_account_id = $1", id)
		_, _ = conn.Exec(ctx, "DELETE FROM business_account_balance WHERE business_account_id = $1", id)
		_, _ = conn.Exec(ctx, "DELETE FROM business_account WHERE id = $1", id)
	}
}

// uniqAccountID 生成带测试名 + 纳秒戳的账户 ID，避免并行测试互相污染。
func uniqAccountID(t *testing.T, prefix string) string {
	t.Helper()
	clean := make([]byte, 0, len(t.Name()))
	for i := 0; i < len(t.Name()); i++ {
		c := t.Name()[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			clean = append(clean, c)
		} else {
			clean = append(clean, '_')
		}
	}
	return fmt.Sprintf("%s-%s-%s", prefix, string(clean), time.Now().Format("150405.000000000"))
}

// queryBalance 直接读 balance 行（绕过 service，用于断言）。
func queryBalance(t *testing.T, accountID string) (available, reserved, usedTotal, rechargeTotal int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, testPGDSN())
	require.NoError(t, err)
	defer pool.Close()
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT available, reserved, used_total, recharge_total
		   FROM business_account_balance WHERE business_account_id = $1`, accountID).
		Scan(&available, &reserved, &usedTotal, &rechargeTotal))
	return
}

// countOutbox 统计指定账户 + 事件类型的 outbox 行数。
func countOutbox(t *testing.T, accountID, eventType string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, testPGDSN())
	require.NoError(t, err)
	defer pool.Close()
	var count int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM webhook_event_outbox WHERE business_account_id = $1 AND event_type = $2`,
		accountID, eventType).Scan(&count))
	return count
}

// countLedgerEntries 统计指定账户 + 类型的 ledger 行数。
func countLedgerEntries(t *testing.T, accountID, entryType string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, testPGDSN())
	require.NoError(t, err)
	defer pool.Close()
	var count int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM business_account_ledger WHERE business_account_id = $1 AND entry_type = $2`,
		accountID, entryType).Scan(&count))
	return count
}

// =============================================================================
// 基础 / 占位测试（保留 Phase 1 部分；移除被 U9 实装的占位用例）
// =============================================================================

func TestHelp列出所有子命令(t *testing.T) {
	out, err := runCmd(t, "--help")
	require.NoError(t, err)
	for _, sub := range []string{"migrate", "token", "account", "drift-check"} {
		assert.Contains(t, out, sub, "--help 应包含子命令 %s", sub)
	}
}

func TestUnknownSubcommandFails(t *testing.T) {
	_, err := runCmd(t, "xxxnotexist")
	require.Error(t, err)
}

// Phase 1 占位子命令保留：migrate / token 仍属占位
func TestMigrateUp占位返回错误(t *testing.T) {
	out, err := runCmd(t, "migrate", "up")
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "尚未实现") ||
		strings.Contains(out, "尚未实现"),
		"占位命令应返回中文「尚未实现」错误，实际 err=%q out=%q", err, out)
}

func TestMigrateDown占位返回错误(t *testing.T) {
	_, err := runCmd(t, "migrate", "down", "1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "尚未实现")
}

func TestMigrateVersion占位返回错误(t *testing.T) {
	_, err := runCmd(t, "migrate", "version")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "尚未实现")
}

func TestTokenCreate占位返回错误(t *testing.T) {
	_, err := runCmd(t, "token", "create",
		"--scope", "business_account:read",
		"--ip-allowlist", "10.0.0.0/8")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "尚未实现")
}

// =============================================================================
// account create — 入参校验测试（不需要 PG）
// =============================================================================

func TestAccountCreate要求id_flag(t *testing.T) {
	_, err := runCmd(t, "account", "create")
	require.Error(t, err, "缺 --id 应失败")
}

func TestAccountRecharge要求flags(t *testing.T) {
	_, err := runCmd(t, "account", "recharge", "--id", "biz-1")
	require.Error(t, err, "缺 --amount / --idempotency-key 应失败")
}

// =============================================================================
// account create — PG 集成
// =============================================================================

func TestAccountCreate_Happy(t *testing.T) {
	mustPingPG(t)
	setupPGEnv(t)
	accountID := uniqAccountID(t, "biz")
	t.Cleanup(func() { cleanupAccountForTest(t, accountID) })

	stdout, _, err := runCmdSeparate(t, "account", "create", "--id", accountID)
	require.NoError(t, err, "create 应成功；stdout=%s", stdout)

	// stdout 是账户 JSON：含 id 字段
	var account map[string]any
	require.NoError(t, json.Unmarshal([]byte(stdout), &account), "stdout 应是合法 JSON: %s", stdout)
	assert.Equal(t, accountID, account["ID"])

	// balance 零行已建
	available, reserved, used, recharge := queryBalance(t, accountID)
	assert.Equal(t, int64(0), available)
	assert.Equal(t, int64(0), reserved)
	assert.Equal(t, int64(0), used)
	assert.Equal(t, int64(0), recharge)

	// outbox 发了 account.created
	assert.Equal(t, 1, countOutbox(t, accountID, "account.created"))
}

func TestAccountCreate_DuplicateIDFails(t *testing.T) {
	mustPingPG(t)
	setupPGEnv(t)
	accountID := uniqAccountID(t, "biz")
	t.Cleanup(func() { cleanupAccountForTest(t, accountID) })

	_, _, err := runCmdSeparate(t, "account", "create", "--id", accountID)
	require.NoError(t, err)

	// 第二次同 id → 非零退出码（PG UNIQUE 冲突由 CreateBusinessAccount 包装为 error）
	_, _, err = runCmdSeparate(t, "account", "create", "--id", accountID)
	require.Error(t, err, "重复 id 应失败")
	assert.Contains(t, err.Error(), "创建账户失败")
}

func TestAccountCreate_EmptyIDFails(t *testing.T) {
	// 不需要 PG —— Cobra MarkFlagRequired 在 RunE 之前拦截，但若用户用 --id "" 显式传空，
	// 会通过 Cobra 校验进入 RunE，由我们的 businessID == "" 检查拦住。
	mustPingPG(t)
	setupPGEnv(t)
	_, _, err := runCmdSeparate(t, "account", "create", "--id", "")
	require.Error(t, err, "--id \"\" 应失败")
}

// =============================================================================
// account recharge — PG 集成
// =============================================================================

func TestAccountRecharge_Happy(t *testing.T) {
	mustPingPG(t)
	setupPGEnv(t)
	accountID := uniqAccountID(t, "biz")
	t.Cleanup(func() { cleanupAccountForTest(t, accountID) })

	_, _, err := runCmdSeparate(t, "account", "create", "--id", accountID)
	require.NoError(t, err)

	stdout, _, err := runCmdSeparate(t,
		"account", "recharge",
		"--id", accountID,
		"--amount", "1000",
		"--idempotency-key", "topup-001")
	require.NoError(t, err, "recharge 应成功；stdout=%s", stdout)

	// stdout 含 ledger entry ID（非零）
	var entry map[string]any
	require.NoError(t, json.Unmarshal([]byte(stdout), &entry))
	idF, ok := entry["ID"].(float64)
	require.True(t, ok, "ID 字段应为数字: %v", entry)
	assert.Greater(t, int64(idF), int64(0))

	// balance.available = 1000
	available, _, _, recharge := queryBalance(t, accountID)
	assert.Equal(t, int64(1000), available)
	assert.Equal(t, int64(1000), recharge)

	// outbox 多了 1 条 recharged
	assert.Equal(t, 1, countOutbox(t, accountID, "account.recharged"))
	assert.Equal(t, 1, countLedgerEntries(t, accountID, "recharge"))
}

func TestAccountRecharge_Idempotent(t *testing.T) {
	mustPingPG(t)
	setupPGEnv(t)
	accountID := uniqAccountID(t, "biz")
	t.Cleanup(func() { cleanupAccountForTest(t, accountID) })

	_, _, err := runCmdSeparate(t, "account", "create", "--id", accountID)
	require.NoError(t, err)

	stdout1, _, err := runCmdSeparate(t,
		"account", "recharge",
		"--id", accountID,
		"--amount", "500",
		"--idempotency-key", "topup-idempotent")
	require.NoError(t, err)

	stdout2, _, err := runCmdSeparate(t,
		"account", "recharge",
		"--id", accountID,
		"--amount", "500",
		"--idempotency-key", "topup-idempotent")
	require.NoError(t, err, "重复同 key + 同 body 应幂等成功")

	// 两次返回的 ledger entry ID 必须相同
	var e1, e2 map[string]any
	require.NoError(t, json.Unmarshal([]byte(stdout1), &e1))
	require.NoError(t, json.Unmarshal([]byte(stdout2), &e2))
	assert.Equal(t, e1["ID"], e2["ID"], "幂等命中应返同一 entry ID")

	// ledger 只多 1 条；outbox 只多 1 条
	assert.Equal(t, 1, countLedgerEntries(t, accountID, "recharge"))
	assert.Equal(t, 1, countOutbox(t, accountID, "account.recharged"))

	// 余额仍只 500
	available, _, _, _ := queryBalance(t, accountID)
	assert.Equal(t, int64(500), available)
}

func TestAccountRecharge_IdempotencyConflict(t *testing.T) {
	mustPingPG(t)
	setupPGEnv(t)
	accountID := uniqAccountID(t, "biz")
	t.Cleanup(func() { cleanupAccountForTest(t, accountID) })

	_, _, err := runCmdSeparate(t, "account", "create", "--id", accountID)
	require.NoError(t, err)

	_, _, err = runCmdSeparate(t,
		"account", "recharge",
		"--id", accountID,
		"--amount", "100",
		"--idempotency-key", "conflict-key")
	require.NoError(t, err)

	// 同 key 不同 amount → 必须拒绝
	_, _, err = runCmdSeparate(t,
		"account", "recharge",
		"--id", accountID,
		"--amount", "200", // 不同 amount → canonical body sha256 不同
		"--idempotency-key", "conflict-key")
	require.Error(t, err, "同 idempotency-key 不同 amount 应拒绝")
	assert.Contains(t, err.Error(), "充值失败")

	// ledger 仍只 1 条
	assert.Equal(t, 1, countLedgerEntries(t, accountID, "recharge"))
}

func TestAccountRecharge_InvalidAmount(t *testing.T) {
	mustPingPG(t)
	setupPGEnv(t)
	accountID := uniqAccountID(t, "biz")
	t.Cleanup(func() { cleanupAccountForTest(t, accountID) })

	_, _, err := runCmdSeparate(t, "account", "create", "--id", accountID)
	require.NoError(t, err)

	// amount=0
	_, _, err = runCmdSeparate(t,
		"account", "recharge",
		"--id", accountID, "--amount", "0",
		"--idempotency-key", "k-zero")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "amount")

	// amount=-100
	_, _, err = runCmdSeparate(t,
		"account", "recharge",
		"--id", accountID, "--amount", "-100",
		"--idempotency-key", "k-neg")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "amount")
}

func TestAccountRecharge_AccountNotFound(t *testing.T) {
	mustPingPG(t)
	setupPGEnv(t)
	missingID := uniqAccountID(t, "missing")
	// 故意不 create

	_, _, err := runCmdSeparate(t,
		"account", "recharge",
		"--id", missingID,
		"--amount", "100",
		"--idempotency-key", "any-key-"+missingID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "充值失败")
}

// =============================================================================
// drift-check — PG 集成
// =============================================================================

func TestDriftCheck_NoDrift(t *testing.T) {
	mustPingPG(t)
	setupPGEnv(t)
	accountID := uniqAccountID(t, "biz")
	t.Cleanup(func() { cleanupAccountForTest(t, accountID) })

	_, _, err := runCmdSeparate(t, "account", "create", "--id", accountID)
	require.NoError(t, err)
	_, _, err = runCmdSeparate(t,
		"account", "recharge",
		"--id", accountID,
		"--amount", "300",
		"--idempotency-key", "topup-no-drift")
	require.NoError(t, err)

	stdout, _, err := runCmdSeparate(t, "drift-check")
	require.NoError(t, err, "drift-check 应跑完一轮成功；stdout=%s", stdout)

	var result driftCheckResult
	require.NoError(t, json.Unmarshal([]byte(stdout), &result))
	// 该账户没有 drift；其他账户可能存在，但 drifted 应为 0（本测试账户无 drift）
	// 注：本测试不要求 checked > 0（其他账户可能 frozen），只要 drift-check 跑完即可。
	assert.Equal(t, 0, result.Drifted, "无人为 drift 时不应有真 drift")
}

func TestDriftCheck_DetectsDrift(t *testing.T) {
	mustPingPG(t)
	setupPGEnv(t)
	accountID := uniqAccountID(t, "biz")
	t.Cleanup(func() { cleanupAccountForTest(t, accountID) })

	_, _, err := runCmdSeparate(t, "account", "create", "--id", accountID)
	require.NoError(t, err)
	_, _, err = runCmdSeparate(t,
		"account", "recharge",
		"--id", accountID,
		"--amount", "1000",
		"--idempotency-key", "topup-drift-seed")
	require.NoError(t, err)

	// 守恒制造 drift：available -= 50, used_total += 50
	// CHECK 约束允许（三态非负 + 总和等式守恒），但与 ledger SUM 不一致。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, testPGDSN())
	require.NoError(t, err)
	defer pool.Close()

	tag, err := pool.Exec(ctx,
		`UPDATE business_account_balance
		   SET available = available - 50, used_total = used_total + 50, version = version + 1
		 WHERE business_account_id = $1`, accountID)
	require.NoError(t, err)
	require.Equal(t, int64(1), tag.RowsAffected(), "应改到 1 行")

	stdout, _, err := runCmdSeparate(t, "drift-check")
	require.NoError(t, err, "drift-check 跑完即 0 退出（不论是否发现 drift）；stdout=%s", stdout)

	var result driftCheckResult
	require.NoError(t, json.Unmarshal([]byte(stdout), &result))
	assert.GreaterOrEqual(t, result.Drifted, 1, "守恒制造的 drift 应被检出；result=%+v", result)
}

// =============================================================================
// 防御编译期 import 警告
// =============================================================================
var _ = pgx.ErrNoRows

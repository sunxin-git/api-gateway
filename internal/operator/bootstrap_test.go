package operator

import (
	"strings"
	"testing"
)

// TestBootstrap_SeedsWhenEmpty 表空 + 提供 bootstrap → 建初始管理员（created_by=seed），可登录。
func TestBootstrap_SeedsWhenEmpty(t *testing.T) {
	pool, svc := setupSuite(t)
	wipeOperatorAccounts(t, pool)
	t.Cleanup(func() { wipeOperatorAccounts(t, pool) })

	const username = "root_admin"
	const password = "bootstrap-secret-1"

	if err := Bootstrap(ctxT(t), svc, username, password, true /*production*/, newSilentLogger()); err != nil {
		t.Fatalf("Bootstrap 表空应种子成功，得 %v", err)
	}

	acct, err := svc.Authenticate(ctxT(t), username, password)
	if err != nil {
		t.Fatalf("种子后应能登录，得 %v", err)
	}
	if acct.CreatedBy != seedCreatedBy {
		t.Fatalf("初始管理员 created_by = %q, want %q", acct.CreatedBy, seedCreatedBy)
	}
}

// TestBootstrap_IdempotentWhenNonEmpty 表非空 → 跳过，不新建、不改既有账户。
func TestBootstrap_IdempotentWhenNonEmpty(t *testing.T) {
	pool, svc := setupSuite(t)
	wipeOperatorAccounts(t, pool)
	t.Cleanup(func() { wipeOperatorAccounts(t, pool) })

	// 先有一个账户
	createTestAccount(t, svc, "existing_op", "existing-pass-1")
	before, _ := svc.Count(ctxT(t))

	// bootstrap 提供了不同账户，但表非空 → 应跳过
	if err := Bootstrap(ctxT(t), svc, "would_be_root", "would-be-secret", true, newSilentLogger()); err != nil {
		t.Fatalf("Bootstrap 表非空应幂等跳过，得 %v", err)
	}
	after, _ := svc.Count(ctxT(t))
	if after != before {
		t.Fatalf("表非空时 bootstrap 不应新建：before=%d after=%d", before, after)
	}
	// would_be_root 不应被创建
	if _, err := svc.Authenticate(ctxT(t), "would_be_root", "would-be-secret"); err == nil {
		t.Fatalf("表非空时 bootstrap 不应创建 would_be_root")
	}
}

// TestBootstrap_ProductionEmptyNoEnv_Errors production + 表空 + 无 bootstrap env → fail-fast。
func TestBootstrap_ProductionEmptyNoEnv_Errors(t *testing.T) {
	pool, svc := setupSuite(t)
	wipeOperatorAccounts(t, pool)
	t.Cleanup(func() { wipeOperatorAccounts(t, pool) })

	err := Bootstrap(ctxT(t), svc, "", "", true /*production*/, newSilentLogger())
	if err == nil {
		t.Fatalf("production 表空且无种子应 fail-fast 返错")
	}
	if !strings.Contains(err.Error(), "production") {
		t.Fatalf("错误信息应点明 production，得 %v", err)
	}
}

// TestBootstrap_NonProductionEmptyNoEnv_Skips 非 production + 表空 + 无 env → 仅告警跳过，不返错。
func TestBootstrap_NonProductionEmptyNoEnv_Skips(t *testing.T) {
	pool, svc := setupSuite(t)
	wipeOperatorAccounts(t, pool)
	t.Cleanup(func() { wipeOperatorAccounts(t, pool) })

	if err := Bootstrap(ctxT(t), svc, "", "", false /*dev*/, newSilentLogger()); err != nil {
		t.Fatalf("非 production 表空无种子应跳过不返错，得 %v", err)
	}
	n, _ := svc.Count(ctxT(t))
	if n != 0 {
		t.Fatalf("跳过后表应仍空，得 count=%d", n)
	}
}

// TestBootstrap_WeakPasswordSeed_Errors 表空 + 提供过短口令 → Create 校验失败上抛。
func TestBootstrap_WeakPasswordSeed_Errors(t *testing.T) {
	pool, svc := setupSuite(t)
	wipeOperatorAccounts(t, pool)
	t.Cleanup(func() { wipeOperatorAccounts(t, pool) })

	if err := Bootstrap(ctxT(t), svc, "root_admin", "short", false, newSilentLogger()); err == nil {
		t.Fatalf("过短种子口令应返错")
	}
}

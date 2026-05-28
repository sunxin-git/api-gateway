package businesskey

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// =============================================================================
// Create
// =============================================================================

func TestCreate_Happy(t *testing.T) {
	pool, svc := setupSuite(t)
	ctx := ctxT(t)

	accID := uniqAccountID(t, "happy")
	createTestAccount(t, pool, accID)

	rpm := int32(600)
	params := CreateParams{
		BusinessAccountID: accID,
		Description:       "test-happy",
		RequestsPerMinute: &rpm,
		CreatedBy:         "cli:bootstrap",
	}
	key, plaintext, err := svc.Create(ctx, params)
	require.NoError(t, err)
	require.NotZero(t, key.ID)
	require.Equal(t, accID, key.BusinessAccountID)
	require.Equal(t, "test-happy", key.Description)
	require.NotNil(t, key.RequestsPerMinute)
	require.Equal(t, int32(600), *key.RequestsPerMinute)
	require.Equal(t, "cli:bootstrap", key.CreatedBy)
	require.Empty(t, key.KeyHash, "Create 返回 Key.KeyHash 必须置空（不暴露给上层）")
	require.GreaterOrEqual(t, len(plaintext), 32)

	// plaintext 经 HMAC(pepper) 应 = DB 中 key_hash
	mac := hmac.New(sha256.New, testPepper)
	mac.Write([]byte(plaintext))
	expectedHash := hex.EncodeToString(mac.Sum(nil))

	var dbHash string
	require.NoError(t, pool.QueryRow(ctx, "SELECT key_hash FROM business_account_api_key WHERE id = $1", key.ID).Scan(&dbHash))
	require.Equal(t, expectedHash, dbHash)
	require.Len(t, dbHash, 64)
}

func TestCreate_PepperSharedWithAdminToken(t *testing.T) {
	// 验证关键安全契约（F-min 决策 D4）：
	// 业务 key plaintext 经 pepper 算出的 hash，与 admintoken 同 plaintext 算出的 hash **完全一致**。
	// 这保证未来如果需要"业务系统接入 admin token"或 admin token 误用为 biz key 时，
	// hash 计算路径同源。
	pool, _ := setupSuite(t)

	customPepper := []byte("custom-pepper-32-bytes-for-cross!Z")
	svcA := newPostgresServiceBase(pool, customPepper, newSilentLogger())
	t.Cleanup(func() { _ = svcA.Close() })

	const plaintext = "fixed-plaintext-for-cross-pepper-check"

	hashA := svcA.hashKey(plaintext)

	// 手工算同 pepper + 同 plaintext 的 HMAC（模拟 admintoken 同源算法）
	mac := hmac.New(sha256.New, customPepper)
	mac.Write([]byte(plaintext))
	expectedShared := hex.EncodeToString(mac.Sum(nil))

	require.Equal(t, expectedShared, hashA, "businesskey hashKey 必须与 HMAC-SHA-256(pepper, plaintext) 字节级一致；admintoken 同 plaintext + pepper 算出同 hash")
}

func TestCreate_InvalidParams(t *testing.T) {
	_, svc := setupSuite(t)
	ctx := ctxT(t)

	rpmInvalid := int32(0)
	rpmNegative := int32(-1)

	cases := []struct {
		name   string
		params CreateParams
		expect string
	}{
		{"empty_account", CreateParams{BusinessAccountID: "", Description: "d", CreatedBy: "c"}, "business_account_id"},
		{"whitespace_account", CreateParams{BusinessAccountID: "   ", Description: "d", CreatedBy: "c"}, "business_account_id"},
		{"empty_description", CreateParams{BusinessAccountID: "a", Description: "", CreatedBy: "c"}, "description"},
		{"empty_created_by", CreateParams{BusinessAccountID: "a", Description: "d", CreatedBy: ""}, "created_by"},
		{"rpm_zero", CreateParams{BusinessAccountID: "a", Description: "d", CreatedBy: "c", RequestsPerMinute: &rpmInvalid}, "requests_per_minute"},
		{"rpm_negative", CreateParams{BusinessAccountID: "a", Description: "d", CreatedBy: "c", RequestsPerMinute: &rpmNegative}, "requests_per_minute"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := svc.Create(ctx, tc.params)
			require.ErrorIs(t, err, ErrInvalidParam)
			require.Contains(t, err.Error(), tc.expect)
		})
	}
}

func TestCreate_NonexistentAccountFails(t *testing.T) {
	_, svc := setupSuite(t)
	ctx := ctxT(t)

	_, _, err := svc.Create(ctx, CreateParams{
		BusinessAccountID: "bk-nonexistent-12345",
		Description:       "should-fail-fk",
		CreatedBy:         "cli:bootstrap",
	})
	require.Error(t, err, "FK 违反必须报错")
	// PG FK violation = SQLSTATE 23503；wrapped 的 InsertBusinessKey 错误应含上下文
	require.Contains(t, err.Error(), "InsertBusinessKey")
}

func TestCreate_NilRPMAllowed(t *testing.T) {
	pool, svc := setupSuite(t)
	ctx := ctxT(t)
	accID := uniqAccountID(t, "nil-rpm")
	createTestAccount(t, pool, accID)

	key, _, err := svc.Create(ctx, CreateParams{
		BusinessAccountID: accID,
		Description:       "no-rpm",
		CreatedBy:         "cli:bootstrap",
	})
	require.NoError(t, err)
	require.Nil(t, key.RequestsPerMinute, "RPM nil 应允许（= 不限速）")
}

// =============================================================================
// ValidateByPlaintext
// =============================================================================

func TestValidate_Happy(t *testing.T) {
	pool, svc := setupSuite(t)
	ctx := ctxT(t)
	accID := uniqAccountID(t, "validate")
	createTestAccount(t, pool, accID)

	key, plaintext, err := svc.Create(ctx, CreateParams{
		BusinessAccountID: accID,
		Description:       "validate-happy",
		CreatedBy:         "cli:bootstrap",
	})
	require.NoError(t, err)

	vr, err := svc.ValidateByPlaintext(ctx, plaintext)
	require.NoError(t, err)
	require.NotNil(t, vr.Key)
	require.Equal(t, key.ID, vr.Key.ID)
	require.Equal(t, accID, vr.Key.BusinessAccountID)
	require.Empty(t, vr.Key.KeyHash, "ValidationResult.Key.KeyHash 必须置空")
}

func TestValidate_EmptyPlaintext(t *testing.T) {
	_, svc := setupSuite(t)
	ctx := ctxT(t)
	_, err := svc.ValidateByPlaintext(ctx, "")
	require.ErrorIs(t, err, ErrKeyNotFound)
}

func TestValidate_UnknownPlaintext(t *testing.T) {
	_, svc := setupSuite(t)
	ctx := ctxT(t)
	_, err := svc.ValidateByPlaintext(ctx, "bk-never-issued-xxx-yyy")
	require.ErrorIs(t, err, ErrKeyNotFound)
}

func TestValidate_RevokedKeyRejected(t *testing.T) {
	pool, svc := setupSuite(t)
	ctx := ctxT(t)
	accID := uniqAccountID(t, "rev-reject")
	createTestAccount(t, pool, accID)

	key, plaintext, err := svc.Create(ctx, CreateParams{
		BusinessAccountID: accID, Description: "rev", CreatedBy: "cli:bootstrap",
	})
	require.NoError(t, err)
	already, err := svc.Revoke(ctx, key.ID)
	require.NoError(t, err)
	require.False(t, already)

	_, err = svc.ValidateByPlaintext(ctx, plaintext)
	require.ErrorIs(t, err, ErrKeyNotFound, "revoked key 应返 ErrKeyNotFound（与未知 plaintext 同语义，防泄漏")
}

func TestValidate_TriggersAsyncTouch(t *testing.T) {
	pool, svc := setupSuite(t)
	ctx := ctxT(t)
	accID := uniqAccountID(t, "touch")
	createTestAccount(t, pool, accID)

	key, plaintext, err := svc.Create(ctx, CreateParams{
		BusinessAccountID: accID, Description: "touch-test", CreatedBy: "cli:bootstrap",
	})
	require.NoError(t, err)

	// 初次创建 last_used_at 应为 nil
	got, err := svc.GetByID(ctx, key.ID)
	require.NoError(t, err)
	require.Nil(t, got.LastUsedAt)

	// 鉴权命中（注入 markTouched）
	_, err = svc.ValidateByPlaintext(ctx, plaintext)
	require.NoError(t, err)

	// pendingTouches 应含本 id（异步未 flush 前）
	_, marked := svc.pendingTouches.Load(key.ID)
	require.True(t, marked, "ValidateByPlaintext 命中后必须 markTouched(key.ID)")

	// 手动 flushOnce → DB last_used_at 应更新
	svc.flushOnce(ctx)
	got, err = svc.GetByID(ctx, key.ID)
	require.NoError(t, err)
	require.NotNil(t, got.LastUsedAt, "flushOnce 后 last_used_at 应非 nil")
	require.WithinDuration(t, time.Now(), *got.LastUsedAt, 30*time.Second)

	// flush 后 pendingTouches 应清空（避免下轮重复 flush）
	_, stillMarked := svc.pendingTouches.Load(key.ID)
	require.False(t, stillMarked, "flushOnce 后 pendingTouches 应清空")
}

// =============================================================================
// Revoke
// =============================================================================

func TestRevoke_FirstTime(t *testing.T) {
	pool, svc := setupSuite(t)
	ctx := ctxT(t)
	accID := uniqAccountID(t, "rev-first")
	createTestAccount(t, pool, accID)

	key, _, err := svc.Create(ctx, CreateParams{
		BusinessAccountID: accID, Description: "rev-first", CreatedBy: "cli:bootstrap",
	})
	require.NoError(t, err)

	already, err := svc.Revoke(ctx, key.ID)
	require.NoError(t, err)
	require.False(t, already)

	var rev *time.Time
	require.NoError(t, pool.QueryRow(ctx, "SELECT revoked_at FROM business_account_api_key WHERE id = $1", key.ID).Scan(&rev))
	require.NotNil(t, rev)
	require.WithinDuration(t, time.Now(), *rev, 30*time.Second)
}

func TestRevoke_AlreadyRevoked_PreservesTimestamp(t *testing.T) {
	pool, svc := setupSuite(t)
	ctx := ctxT(t)
	accID := uniqAccountID(t, "rev-twice")
	createTestAccount(t, pool, accID)

	key, _, err := svc.Create(ctx, CreateParams{
		BusinessAccountID: accID, Description: "rev-twice", CreatedBy: "cli:bootstrap",
	})
	require.NoError(t, err)

	_, err = svc.Revoke(ctx, key.ID)
	require.NoError(t, err)
	var firstRev time.Time
	require.NoError(t, pool.QueryRow(ctx, "SELECT revoked_at FROM business_account_api_key WHERE id = $1", key.ID).Scan(&firstRev))

	time.Sleep(50 * time.Millisecond)
	already, err := svc.Revoke(ctx, key.ID)
	require.NoError(t, err)
	require.True(t, already)

	var secondRev time.Time
	require.NoError(t, pool.QueryRow(ctx, "SELECT revoked_at FROM business_account_api_key WHERE id = $1", key.ID).Scan(&secondRev))
	require.Equal(t, firstRev, secondRev, "二次 revoke 必须保留首次 timestamp（COALESCE 语义）")
}

func TestRevoke_NotFound(t *testing.T) {
	_, svc := setupSuite(t)
	ctx := ctxT(t)
	_, err := svc.Revoke(ctx, 999_999_999_999)
	require.ErrorIs(t, err, ErrKeyNotFound)
}

// =============================================================================
// List
// =============================================================================

func TestListByAccount_ExcludesRevoked(t *testing.T) {
	pool, svc := setupSuite(t)
	ctx := ctxT(t)
	accID := uniqAccountID(t, "list-acc")
	createTestAccount(t, pool, accID)

	// 创 3 个 key；revoke 第 2 个
	var keptIDs []int64
	for i, suffix := range []string{"k1", "k2", "k3"} {
		key, _, err := svc.Create(ctx, CreateParams{
			BusinessAccountID: accID, Description: "list-" + suffix, CreatedBy: "cli:bootstrap",
		})
		require.NoError(t, err)
		if i == 1 {
			_, err := svc.Revoke(ctx, key.ID)
			require.NoError(t, err)
		} else {
			keptIDs = append(keptIDs, key.ID)
		}
	}

	got, err := svc.ListByAccount(ctx, accID)
	require.NoError(t, err)
	require.Len(t, got, 2)
	gotIDs := []int64{got[0].ID, got[1].ID}
	require.ElementsMatch(t, keptIDs, gotIDs)

	for _, k := range got {
		require.Empty(t, k.KeyHash, "ListByAccount 返回的 KeyHash 必须置空")
	}
}

func TestListAll_AcrossAccounts(t *testing.T) {
	pool, svc := setupSuite(t)
	ctx := ctxT(t)
	a1 := uniqAccountID(t, "listall-a1")
	a2 := uniqAccountID(t, "listall-a2")
	createTestAccount(t, pool, a1)
	createTestAccount(t, pool, a2)

	for _, acc := range []string{a1, a2} {
		_, _, err := svc.Create(ctx, CreateParams{
			BusinessAccountID: acc, Description: "ll-" + acc, CreatedBy: "cli:bootstrap",
		})
		require.NoError(t, err)
	}

	all, err := svc.ListAll(ctx)
	require.NoError(t, err)
	// 全库可能含别测试的 key；过滤本测试的两个
	mine := filterByAccountIDs(all, a1, a2)
	require.Len(t, mine, 2)
	for _, k := range mine {
		require.Empty(t, k.KeyHash)
	}
}

// =============================================================================
// GetByID
// =============================================================================

func TestGetByID_Found(t *testing.T) {
	pool, svc := setupSuite(t)
	ctx := ctxT(t)
	accID := uniqAccountID(t, "get")
	createTestAccount(t, pool, accID)

	key, _, err := svc.Create(ctx, CreateParams{
		BusinessAccountID: accID, Description: "get-by-id", CreatedBy: "cli:bootstrap",
	})
	require.NoError(t, err)

	got, err := svc.GetByID(ctx, key.ID)
	require.NoError(t, err)
	require.Equal(t, key.ID, got.ID)
	require.Empty(t, got.KeyHash)
}

func TestGetByID_NotFound(t *testing.T) {
	_, svc := setupSuite(t)
	ctx := ctxT(t)
	_, err := svc.GetByID(ctx, 999_999_999_999)
	require.ErrorIs(t, err, ErrKeyNotFound)
}

func TestGetByID_IncludesRevoked(t *testing.T) {
	pool, svc := setupSuite(t)
	ctx := ctxT(t)
	accID := uniqAccountID(t, "get-rev")
	createTestAccount(t, pool, accID)

	key, _, err := svc.Create(ctx, CreateParams{
		BusinessAccountID: accID, Description: "get-rev", CreatedBy: "cli:bootstrap",
	})
	require.NoError(t, err)
	_, err = svc.Revoke(ctx, key.ID)
	require.NoError(t, err)

	got, err := svc.GetByID(ctx, key.ID)
	require.NoError(t, err)
	require.NotNil(t, got.RevokedAt, "GetByID 必须能查到已 revoke 的 key（运维 / audit 用）")
}

// =============================================================================
// FK CASCADE 行为
// =============================================================================

func TestFKCascade_DeleteAccountDeletesKeys(t *testing.T) {
	pool, svc := setupSuite(t)
	ctx := ctxT(t)
	accID := uniqAccountID(t, "fk-cascade")
	createTestAccount(t, pool, accID)

	key, _, err := svc.Create(ctx, CreateParams{
		BusinessAccountID: accID, Description: "fk", CreatedBy: "cli:bootstrap",
	})
	require.NoError(t, err)

	// 直接 DELETE 触发 FK CASCADE；不能 SET session_replication_role='replica'，那会
	// 同时关闭 RI 约束（PG 实现 FK 用 constraint trigger），CASCADE 不会触发。
	// 本测试没创 ledger entry，不需要绕过 ledger 不可变 trigger。
	_, err = pool.Exec(ctx, "DELETE FROM business_account WHERE id = $1", accID)
	require.NoError(t, err)

	// key 应被 CASCADE 自动删除
	_, err = svc.GetByID(ctx, key.ID)
	require.ErrorIs(t, err, ErrKeyNotFound)
}

// =============================================================================
// flush 行为
// =============================================================================

func TestFlush_PartialFailureContinues(t *testing.T) {
	pool, svc := setupSuite(t)
	ctx := ctxT(t)
	accID := uniqAccountID(t, "flush-partial")
	createTestAccount(t, pool, accID)

	key1, _, err := svc.Create(ctx, CreateParams{
		BusinessAccountID: accID, Description: "flush-1", CreatedBy: "cli:bootstrap",
	})
	require.NoError(t, err)
	key2, _, err := svc.Create(ctx, CreateParams{
		BusinessAccountID: accID, Description: "flush-2", CreatedBy: "cli:bootstrap",
	})
	require.NoError(t, err)

	// 标记两个 + 一个不存在 id（模拟 race：UPDATE 时 key 被外部删除）
	svc.markTouched(key1.ID)
	svc.markTouched(key2.ID)
	svc.markTouched(999_999_999_999) // UPDATE 0 rows，不报错

	svc.flushOnce(ctx)

	// 两个真实 key 都应 last_used_at 写入
	for _, id := range []int64{key1.ID, key2.ID} {
		k, err := svc.GetByID(ctx, id)
		require.NoError(t, err)
		require.NotNil(t, k.LastUsedAt, "key %d 应被 flush", id)
	}
	// 全部 marked 应清空
	require.Equal(t, 0, syncMapLen(&svc.pendingTouches))
}

func TestFlush_Concurrent_NoDataRace(t *testing.T) {
	pool, svc := setupSuite(t)
	ctx := ctxT(t)
	accID := uniqAccountID(t, "flush-concurrent")
	createTestAccount(t, pool, accID)

	key, plaintext, err := svc.Create(ctx, CreateParams{
		BusinessAccountID: accID, Description: "flush-concurrent", CreatedBy: "cli:bootstrap",
	})
	require.NoError(t, err)

	// 100 goroutine 并发 Validate → 都 markTouched 同 ID
	var wg sync.WaitGroup
	const N = 100
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			_, err := svc.ValidateByPlaintext(ctx, plaintext)
			require.NoError(t, err)
		}()
	}
	wg.Wait()

	// pendingTouches 应仅含 1 个 entry（同 ID 重复 Store 覆盖）
	_, marked := svc.pendingTouches.Load(key.ID)
	require.True(t, marked)
}

// =============================================================================
// Constructor panic 路径
// =============================================================================

func TestNewPostgresService_PanicOnShortPepper(t *testing.T) {
	pool := mustOpenTestPool(t)
	t.Cleanup(pool.Close)
	defer func() {
		r := recover()
		require.NotNil(t, r, "短 pepper 必须 panic")
	}()
	NewPostgresService(pool, []byte("too-short"), newSilentLogger())
}

func TestNewPostgresService_PanicOnNilPool(t *testing.T) {
	defer func() {
		require.NotNil(t, recover(), "nil pool 必须 panic")
	}()
	NewPostgresService(nil, testPepper, newSilentLogger())
}

func TestNewPostgresService_PanicOnNilLogger(t *testing.T) {
	pool := mustOpenTestPool(t)
	t.Cleanup(pool.Close)
	defer func() {
		require.NotNil(t, recover(), "nil logger 必须 panic")
	}()
	NewPostgresService(pool, testPepper, nil)
}

// =============================================================================
// Close 幂等 + 触发最终 flush
// =============================================================================

func TestClose_Idempotent(t *testing.T) {
	pool := mustOpenTestPool(t)
	t.Cleanup(pool.Close)
	svc := newTestService(t, pool)
	require.NoError(t, svc.Close())
	require.NoError(t, svc.Close(), "二次 Close 必须幂等")
}

func TestClose_TriggersFinalFlush(t *testing.T) {
	pool, svc := setupSuite(t)
	ctx := ctxT(t)
	accID := uniqAccountID(t, "close-flush")
	createTestAccount(t, pool, accID)

	key, _, err := svc.Create(ctx, CreateParams{
		BusinessAccountID: accID, Description: "close-flush", CreatedBy: "cli:bootstrap",
	})
	require.NoError(t, err)
	svc.markTouched(key.ID)

	// Close 应触发最终 flush
	require.NoError(t, svc.Close())

	// 直接查 DB（绕过 svc，因为 svc 已 Close；用 pool 直查）
	var lastUsed *time.Time
	require.NoError(t, pool.QueryRow(ctx, "SELECT last_used_at FROM business_account_api_key WHERE id = $1", key.ID).Scan(&lastUsed))
	require.NotNil(t, lastUsed, "Close 应触发最终 flush")
}

// =============================================================================
// 测试 helpers
// =============================================================================

func filterByAccountIDs(keys []*Key, accountIDs ...string) []*Key {
	set := make(map[string]struct{}, len(accountIDs))
	for _, id := range accountIDs {
		set[id] = struct{}{}
	}
	var out []*Key
	for _, k := range keys {
		if _, ok := set[k.BusinessAccountID]; ok {
			out = append(out, k)
		}
	}
	return out
}

func syncMapLen(m *sync.Map) int {
	n := 0
	m.Range(func(_, _ any) bool { n++; return true })
	return n
}

// 防 staticcheck "unused import" 误报；strings 在 test 文件其他路径可能用到。
var _ = strings.TrimSpace

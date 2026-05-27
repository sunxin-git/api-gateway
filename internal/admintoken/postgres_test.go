package admintoken

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Create
// =============================================================================

func TestCreate_Happy(t *testing.T) {
	pool, svc := setupSuite(t)
	ctx := ctxT(t)

	desc := testDescription(t, "happy")
	t.Cleanup(func() { cleanupTokensByDescription(t, pool, "admintoken-test:"+t.Name()) })

	params := baseCreateParams(t)
	params.Description = desc
	params.Scopes = []string{"business_account:read", "business_account:recharge"}

	tok, plaintext, err := svc.Create(ctx, params)
	require.NoError(t, err)
	require.NotZero(t, tok.ID)
	require.Equal(t, desc, tok.Description)
	require.Equal(t, []string{"business_account:read", "business_account:recharge"}, tok.Scopes)
	require.Len(t, tok.AllowedCIDRs, 2)
	require.Equal(t, "cli:bootstrap", tok.CreatedBy)
	require.Nil(t, tok.RevokedAt)
	require.Nil(t, tok.ExpiresAt)
	require.False(t, tok.CircuitBreakerEnabled)
	require.Empty(t, tok.TokenHash, "Create 返回的 Token.TokenHash 必须置空（防泄漏）")

	// plaintext 是 32 字节 → base64url → 至少 32 字符
	require.GreaterOrEqual(t, len(plaintext), 32)
}

func TestCreate_PlaintextHashMatchesDB(t *testing.T) {
	pool, svc := setupSuite(t)
	ctx := ctxT(t)

	t.Cleanup(func() { cleanupTokensByDescription(t, pool, "admintoken-test:"+t.Name()) })

	params := baseCreateParams(t)
	params.Description = testDescription(t, "hashmatch")

	tok, plaintext, err := svc.Create(ctx, params)
	require.NoError(t, err)

	// 直接读 DB 的 token_hash
	var dbHash string
	require.NoError(t, pool.QueryRow(ctx, "SELECT token_hash FROM gateway_admin_token WHERE id = $1", tok.ID).Scan(&dbHash))

	// service 算出的 hash 应等于 DB hash
	mac := hmac.New(sha256.New, testPepper)
	mac.Write([]byte(plaintext))
	expected := hex.EncodeToString(mac.Sum(nil))
	require.Equal(t, expected, dbHash, "DB 中 token_hash 必须等于 HMAC-SHA-256(pepper, plaintext) 的 hex")

	// 关键 negative assertion：DB hash 不应等于裸 SHA-256(plaintext)（验证 pepper 真起作用）
	sum := sha256.Sum256([]byte(plaintext))
	plainSha := hex.EncodeToString(sum[:])
	require.NotEqual(t, plainSha, dbHash, "DB hash 不应等于裸 SHA-256（说明漏带 pepper）")
}

func TestCreate_DifferentPepperGivesDifferentHash(t *testing.T) {
	pool, _ := setupSuite(t)
	ctx := ctxT(t)
	t.Cleanup(func() { cleanupTokensByDescription(t, pool, "admintoken-test:"+t.Name()) })

	pepperA := []byte("pepperA-pepperA-pepperA-pepperA-AA")
	pepperB := []byte("pepperB-pepperB-pepperB-pepperB-BB")

	svcA := NewPostgresService(pool, pepperA, newSilentLogger())
	svcB := NewPostgresService(pool, pepperB, newSilentLogger())

	paramsA := baseCreateParams(t)
	paramsA.Description = testDescription(t, "pepperA")
	tokA, ptA, err := svcA.Create(ctx, paramsA)
	require.NoError(t, err)

	paramsB := baseCreateParams(t)
	paramsB.Description = testDescription(t, "pepperB")
	tokB, ptB, err := svcB.Create(ctx, paramsB)
	require.NoError(t, err)

	// 即便 plaintext 撞了（极小概率，但理论上）；用同一 plaintext 跨 pepper 算
	macA := hmac.New(sha256.New, pepperA)
	macA.Write([]byte(ptA))
	hashA := hex.EncodeToString(macA.Sum(nil))

	macB := hmac.New(sha256.New, pepperB)
	macB.Write([]byte(ptA)) // 用同一 plaintext 但 pepperB
	hashB := hex.EncodeToString(macB.Sum(nil))
	require.NotEqual(t, hashA, hashB, "同 plaintext + 不同 pepper 必须产生不同 hash")

	// svcB 用 pepperB；用 ptA 调 ValidateByPlaintext 必查不到（hash 不匹配 DB 中 pepperA 写入的 row）
	_, err = svcB.ValidateByPlaintext(ctx, ptA, mustAddr(t, "10.0.0.1"))
	require.ErrorIs(t, err, ErrTokenNotFound, "用错 pepper 鉴权必须无匹配")

	_ = tokA
	_ = tokB
	_ = ptB
}

func TestCreate_InvalidParams(t *testing.T) {
	_, svc := setupSuite(t)
	ctx := ctxT(t)

	base := baseCreateParams(t)
	base.Description = "valid-description"

	cases := []struct {
		name   string
		mutate func(*CreateParams)
		expect string
	}{
		{"empty description", func(p *CreateParams) { p.Description = "" }, "description"},
		{"whitespace description", func(p *CreateParams) { p.Description = "   " }, "description"},
		{"empty scopes", func(p *CreateParams) { p.Scopes = nil }, "scopes"},
		{"empty scope string", func(p *CreateParams) { p.Scopes = []string{"business_account:read", ""} }, "空字符串"},
		{"empty allowed cidrs", func(p *CreateParams) { p.AllowedCIDRs = nil }, "ip_allowlist"},
		{"empty created_by", func(p *CreateParams) { p.CreatedBy = "" }, "created_by"},
		{"past expires_at", func(p *CreateParams) {
			past := time.Now().Add(-time.Hour)
			p.ExpiresAt = &past
		}, "expires_at"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := base
			p.Scopes = append([]string{}, base.Scopes...)
			p.AllowedCIDRs = append([]netip.Prefix{}, base.AllowedCIDRs...)
			tc.mutate(&p)
			_, _, err := svc.Create(ctx, p)
			require.ErrorIs(t, err, ErrInvalidParam)
			require.Contains(t, err.Error(), tc.expect)
		})
	}
}

func TestCreate_WithFullThrottleFields(t *testing.T) {
	pool, svc := setupSuite(t)
	ctx := ctxT(t)
	t.Cleanup(func() { cleanupTokensByDescription(t, pool, "admintoken-test:"+t.Name()) })

	params := baseCreateParams(t)
	params.Description = testDescription(t, "full-throttle")
	srm := int64(500_000)
	drl := int64(10_000_000)
	srfm := int64(200_000)
	drfl := int64(5_000_000)
	dac := int32(50)
	rpm := int32(600)
	exp := time.Now().Add(24 * time.Hour)
	params.SingleRechargeMax = &srm
	params.DailyRechargeQuotaLimit = &drl
	params.SingleRefundMax = &srfm
	params.DailyRefundQuotaLimit = &drfl
	params.DailyAccountCreateLimit = &dac
	params.RequestsPerMinute = &rpm
	params.CircuitBreakerEnabled = true
	params.ExpiresAt = &exp

	tok, _, err := svc.Create(ctx, params)
	require.NoError(t, err)
	require.NotNil(t, tok.SingleRechargeMax)
	require.Equal(t, srm, *tok.SingleRechargeMax)
	require.NotNil(t, tok.DailyRechargeQuotaLimit)
	require.Equal(t, drl, *tok.DailyRechargeQuotaLimit)
	require.NotNil(t, tok.SingleRefundMax)
	require.Equal(t, srfm, *tok.SingleRefundMax)
	require.NotNil(t, tok.DailyRefundQuotaLimit)
	require.Equal(t, drfl, *tok.DailyRefundQuotaLimit)
	require.NotNil(t, tok.DailyAccountCreateLimit)
	require.Equal(t, dac, *tok.DailyAccountCreateLimit)
	require.NotNil(t, tok.RequestsPerMinute)
	require.Equal(t, rpm, *tok.RequestsPerMinute)
	require.True(t, tok.CircuitBreakerEnabled)
	require.NotNil(t, tok.ExpiresAt)
	// expires_at 容差 1s（PG / Go time 精度）
	require.WithinDuration(t, exp, *tok.ExpiresAt, time.Second)
}

// =============================================================================
// ValidateByPlaintext
// =============================================================================

func TestValidate_Happy(t *testing.T) {
	pool, svc := setupSuite(t)
	ctx := ctxT(t)
	t.Cleanup(func() { cleanupTokensByDescription(t, pool, "admintoken-test:"+t.Name()) })

	params := baseCreateParams(t)
	params.Description = testDescription(t, "validate")
	params.Scopes = []string{"business_account:read", "business_account:recharge"}
	tok, plaintext, err := svc.Create(ctx, params)
	require.NoError(t, err)

	vr, err := svc.ValidateByPlaintext(ctx, plaintext, mustAddr(t, "10.0.0.5"))
	require.NoError(t, err)
	require.NotNil(t, vr)
	require.Equal(t, tok.ID, vr.Token.ID)
	require.ElementsMatch(t, []string{"business_account:read", "business_account:recharge"}, vr.Token.Scopes)
	require.Empty(t, vr.Token.TokenHash, "ValidationResult 不应含 token_hash")
}

func TestValidate_EmptyPlaintext(t *testing.T) {
	_, svc := setupSuite(t)
	ctx := ctxT(t)
	_, err := svc.ValidateByPlaintext(ctx, "", mustAddr(t, "10.0.0.1"))
	require.ErrorIs(t, err, ErrTokenNotFound)
}

func TestValidate_TokenNotFound(t *testing.T) {
	_, svc := setupSuite(t)
	ctx := ctxT(t)
	_, err := svc.ValidateByPlaintext(ctx, "never-issued-token-xxx", mustAddr(t, "10.0.0.1"))
	require.ErrorIs(t, err, ErrTokenNotFound)
}

func TestValidate_RevokedToken(t *testing.T) {
	pool, svc := setupSuite(t)
	ctx := ctxT(t)
	t.Cleanup(func() { cleanupTokensByDescription(t, pool, "admintoken-test:"+t.Name()) })

	params := baseCreateParams(t)
	params.Description = testDescription(t, "revoked")
	tok, plaintext, err := svc.Create(ctx, params)
	require.NoError(t, err)

	already, err := svc.Revoke(ctx, tok.ID)
	require.NoError(t, err)
	require.False(t, already)

	// 已 revoke 的 token 鉴权返 ErrTokenNotFound（query 含 WHERE revoked_at IS NULL）
	_, err = svc.ValidateByPlaintext(ctx, plaintext, mustAddr(t, "10.0.0.1"))
	require.ErrorIs(t, err, ErrTokenNotFound)
}

func TestValidate_ExpiredToken(t *testing.T) {
	pool, svc := setupSuite(t)
	ctx := ctxT(t)
	t.Cleanup(func() { cleanupTokensByDescription(t, pool, "admintoken-test:"+t.Name()) })

	params := baseCreateParams(t)
	params.Description = testDescription(t, "expired")
	// 用未来 expires_at 通过 validateCreateParams；随后手工 UPDATE 改成过去
	future := time.Now().Add(time.Hour)
	params.ExpiresAt = &future
	tok, plaintext, err := svc.Create(ctx, params)
	require.NoError(t, err)

	past := time.Now().Add(-time.Hour)
	_, err = pool.Exec(ctx, "UPDATE gateway_admin_token SET expires_at = $1 WHERE id = $2", past, tok.ID)
	require.NoError(t, err)

	_, err = svc.ValidateByPlaintext(ctx, plaintext, mustAddr(t, "10.0.0.1"))
	require.ErrorIs(t, err, ErrTokenNotFound, "expires_at 过去时鉴权 query WHERE 不通过")
}

func TestValidate_NeverExpires(t *testing.T) {
	pool, svc := setupSuite(t)
	ctx := ctxT(t)
	t.Cleanup(func() { cleanupTokensByDescription(t, pool, "admintoken-test:"+t.Name()) })

	params := baseCreateParams(t)
	params.Description = testDescription(t, "never-expires")
	params.ExpiresAt = nil // 永不过期
	_, plaintext, err := svc.Create(ctx, params)
	require.NoError(t, err)

	_, err = svc.ValidateByPlaintext(ctx, plaintext, mustAddr(t, "10.0.0.1"))
	require.NoError(t, err)
}

func TestValidate_IPNotInAllowlist(t *testing.T) {
	pool, svc := setupSuite(t)
	ctx := ctxT(t)
	t.Cleanup(func() { cleanupTokensByDescription(t, pool, "admintoken-test:"+t.Name()) })

	params := baseCreateParams(t)
	params.Description = testDescription(t, "ip-deny")
	params.AllowedCIDRs = []netip.Prefix{mustPrefix(t, "192.168.0.0/24")} // 仅允许 192.168.0.x
	_, plaintext, err := svc.Create(ctx, params)
	require.NoError(t, err)

	_, err = svc.ValidateByPlaintext(ctx, plaintext, mustAddr(t, "10.0.0.5"))
	require.ErrorIs(t, err, ErrIPNotAllowed)
}

func TestValidate_IPInAllowlist(t *testing.T) {
	pool, svc := setupSuite(t)
	ctx := ctxT(t)
	t.Cleanup(func() { cleanupTokensByDescription(t, pool, "admintoken-test:"+t.Name()) })

	params := baseCreateParams(t)
	params.Description = testDescription(t, "ip-ok")
	params.AllowedCIDRs = []netip.Prefix{mustPrefix(t, "10.10.0.0/16")}
	_, plaintext, err := svc.Create(ctx, params)
	require.NoError(t, err)

	_, err = svc.ValidateByPlaintext(ctx, plaintext, mustAddr(t, "10.10.1.42"))
	require.NoError(t, err)
}

func TestValidate_EmptyAllowlist_FailsClosed(t *testing.T) {
	pool, svc := setupSuite(t)
	ctx := ctxT(t)
	t.Cleanup(func() { cleanupTokensByDescription(t, pool, "admintoken-test:"+t.Name()) })

	// 用合法 CIDR 创建，然后手工 UPDATE 把 ip_allowlist 改为空数组
	// （Create 入口禁止空数组；但 DB 行能被外部改空，必须 fail-closed）
	params := baseCreateParams(t)
	params.Description = testDescription(t, "empty-allowlist")
	tok, plaintext, err := svc.Create(ctx, params)
	require.NoError(t, err)

	_, err = pool.Exec(ctx, "UPDATE gateway_admin_token SET ip_allowlist = '{}'::cidr[] WHERE id = $1", tok.ID)
	require.NoError(t, err)

	_, err = svc.ValidateByPlaintext(ctx, plaintext, mustAddr(t, "10.0.0.1"))
	require.ErrorIs(t, err, ErrIPNotAllowed, "空 allowlist 必须 fail-closed 拒全部")
}

func TestValidate_InvalidClientIP(t *testing.T) {
	pool, svc := setupSuite(t)
	ctx := ctxT(t)
	t.Cleanup(func() { cleanupTokensByDescription(t, pool, "admintoken-test:"+t.Name()) })

	params := baseCreateParams(t)
	params.Description = testDescription(t, "invalid-ip")
	_, plaintext, err := svc.Create(ctx, params)
	require.NoError(t, err)

	// 零值 netip.Addr（IsValid=false）→ fail-closed 拒绝
	_, err = svc.ValidateByPlaintext(ctx, plaintext, netip.Addr{})
	require.ErrorIs(t, err, ErrIPNotAllowed)
}

func TestValidate_IPv6(t *testing.T) {
	pool, svc := setupSuite(t)
	ctx := ctxT(t)
	t.Cleanup(func() { cleanupTokensByDescription(t, pool, "admintoken-test:"+t.Name()) })

	params := baseCreateParams(t)
	params.Description = testDescription(t, "ipv6")
	params.AllowedCIDRs = []netip.Prefix{mustPrefix(t, "2001:db8::/32")}
	_, plaintext, err := svc.Create(ctx, params)
	require.NoError(t, err)

	vr, err := svc.ValidateByPlaintext(ctx, plaintext, mustAddr(t, "2001:db8::1"))
	require.NoError(t, err)
	require.NotNil(t, vr)

	_, err = svc.ValidateByPlaintext(ctx, plaintext, mustAddr(t, "2001:beef::1"))
	require.ErrorIs(t, err, ErrIPNotAllowed)
}

// =============================================================================
// CheckScope
// =============================================================================

func TestCheckScope(t *testing.T) {
	_, svc := setupSuite(t)

	tok := &Token{Scopes: []string{"business_account:read", "business_account:recharge"}}

	require.True(t, svc.CheckScope(tok, "business_account:read"))
	require.True(t, svc.CheckScope(tok, "business_account:recharge"))
	require.False(t, svc.CheckScope(tok, "business_account:refund"))
	require.False(t, svc.CheckScope(tok, ""), "空 requiredScope 必须 fail-closed")
	require.False(t, svc.CheckScope(nil, "business_account:read"), "nil token 必须 fail-closed")

	empty := &Token{Scopes: nil}
	require.False(t, svc.CheckScope(empty, "business_account:read"), "空 scopes 必须 fail-closed")
}

// =============================================================================
// Revoke
// =============================================================================

func TestRevoke_FirstTime(t *testing.T) {
	pool, svc := setupSuite(t)
	ctx := ctxT(t)
	t.Cleanup(func() { cleanupTokensByDescription(t, pool, "admintoken-test:"+t.Name()) })

	params := baseCreateParams(t)
	params.Description = testDescription(t, "revoke-first")
	tok, _, err := svc.Create(ctx, params)
	require.NoError(t, err)

	already, err := svc.Revoke(ctx, tok.ID)
	require.NoError(t, err)
	require.False(t, already, "首次 revoke 应返 alreadyRevoked=false")

	// DB 检查 revoked_at 已写入
	var rev *time.Time
	require.NoError(t, pool.QueryRow(ctx, "SELECT revoked_at FROM gateway_admin_token WHERE id = $1", tok.ID).Scan(&rev))
	require.NotNil(t, rev)
	require.WithinDuration(t, time.Now(), *rev, 30*time.Second)
}

func TestRevoke_AlreadyRevoked_PreservesTimestamp(t *testing.T) {
	pool, svc := setupSuite(t)
	ctx := ctxT(t)
	t.Cleanup(func() { cleanupTokensByDescription(t, pool, "admintoken-test:"+t.Name()) })

	params := baseCreateParams(t)
	params.Description = testDescription(t, "revoke-twice")
	tok, _, err := svc.Create(ctx, params)
	require.NoError(t, err)

	_, err = svc.Revoke(ctx, tok.ID)
	require.NoError(t, err)

	// 读首次 revoked_at
	var firstRev time.Time
	require.NoError(t, pool.QueryRow(ctx, "SELECT revoked_at FROM gateway_admin_token WHERE id = $1", tok.ID).Scan(&firstRev))

	// 等待 50ms 后第二次 revoke
	time.Sleep(50 * time.Millisecond)
	already, err := svc.Revoke(ctx, tok.ID)
	require.NoError(t, err)
	require.True(t, already, "已 revoked token 再调 Revoke 应返 alreadyRevoked=true")

	// 二次 revoke 不应覆盖原 timestamp（COALESCE 语义）
	var secondRev time.Time
	require.NoError(t, pool.QueryRow(ctx, "SELECT revoked_at FROM gateway_admin_token WHERE id = $1", tok.ID).Scan(&secondRev))
	require.Equal(t, firstRev, secondRev, "二次 revoke 必须保留首次 revoke 时间戳")
}

func TestRevoke_NotFound(t *testing.T) {
	_, svc := setupSuite(t)
	ctx := ctxT(t)
	_, err := svc.Revoke(ctx, 999_999_999_999)
	require.ErrorIs(t, err, ErrTokenNotFound)
}

// =============================================================================
// List
// =============================================================================

func TestList_ContainsActiveExcludesRevoked(t *testing.T) {
	pool, svc := setupSuite(t)
	ctx := ctxT(t)
	t.Cleanup(func() { cleanupTokensByDescription(t, pool, "admintoken-test:"+t.Name()) })

	// 创 3 个 token，revoke 其中一个
	var keptIDs []int64
	for i, name := range []string{"k1", "k2", "k3"} {
		p := baseCreateParams(t)
		p.Description = testDescription(t, name)
		tok, _, err := svc.Create(ctx, p)
		require.NoError(t, err)
		if i == 1 {
			_, err := svc.Revoke(ctx, tok.ID)
			require.NoError(t, err)
		} else {
			keptIDs = append(keptIDs, tok.ID)
		}
	}

	all, err := svc.List(ctx)
	require.NoError(t, err)

	// 过滤本测试自己的 token（List 返全库未 revoke token；并行测试会有别的）
	mine := filterByDescPrefix(all, "admintoken-test:"+t.Name())
	require.Len(t, mine, 2, "List 应包含 2 个未 revoke token")
	gotIDs := []int64{mine[0].ID, mine[1].ID}
	require.ElementsMatch(t, keptIDs, gotIDs)

	for _, tk := range mine {
		require.Empty(t, tk.TokenHash, "List 返回的 Token.TokenHash 必须置空")
	}
}

// =============================================================================
// GetByID
// =============================================================================

func TestGetByID_Found(t *testing.T) {
	pool, svc := setupSuite(t)
	ctx := ctxT(t)
	t.Cleanup(func() { cleanupTokensByDescription(t, pool, "admintoken-test:"+t.Name()) })

	p := baseCreateParams(t)
	p.Description = testDescription(t, "getbyid")
	tok, _, err := svc.Create(ctx, p)
	require.NoError(t, err)

	got, err := svc.GetByID(ctx, tok.ID)
	require.NoError(t, err)
	require.Equal(t, tok.ID, got.ID)
	require.Equal(t, tok.Description, got.Description)
	require.Empty(t, got.TokenHash, "GetByID 返回的 TokenHash 必须置空")
}

func TestGetByID_NotFound(t *testing.T) {
	_, svc := setupSuite(t)
	ctx := ctxT(t)
	_, err := svc.GetByID(ctx, 999_999_999_999)
	require.ErrorIs(t, err, ErrTokenNotFound)
}

func TestGetByID_IncludesRevoked(t *testing.T) {
	pool, svc := setupSuite(t)
	ctx := ctxT(t)
	t.Cleanup(func() { cleanupTokensByDescription(t, pool, "admintoken-test:"+t.Name()) })

	p := baseCreateParams(t)
	p.Description = testDescription(t, "getbyid-rev")
	tok, _, err := svc.Create(ctx, p)
	require.NoError(t, err)
	_, err = svc.Revoke(ctx, tok.ID)
	require.NoError(t, err)

	got, err := svc.GetByID(ctx, tok.ID)
	require.NoError(t, err)
	require.NotNil(t, got.RevokedAt, "GetByID 必须能查到已 revoke 的 token（运维 / audit 用）")
}

// =============================================================================
// Constructor fail-fast
// =============================================================================

func TestNewPostgresService_PanicOnShortPepper(t *testing.T) {
	pool := mustOpenTestPool(t)
	t.Cleanup(pool.Close)

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on short pepper")
		} else {
			msg, _ := r.(string)
			require.Contains(t, msg, "32", "panic 信息应指明 32 字节要求")
		}
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
// Hash 一致性（pepper 隔离）
// =============================================================================

func TestHash_StableSamePepperSamePlaintext(t *testing.T) {
	pool := mustOpenTestPool(t)
	t.Cleanup(pool.Close)
	svc1 := newTestService(t, pool)
	svc2 := newTestService(t, pool) // 同 pepper 不同实例

	const plaintext = "fixed-test-plaintext-stable"
	h1 := svc1.hashToken(plaintext)
	h2 := svc2.hashToken(plaintext)
	require.Equal(t, h1, h2, "同 pepper 同 plaintext 必须产生稳定 hash")
	require.Len(t, h1, 64, "HMAC-SHA-256 hex 长度必须为 64")
}

// =============================================================================
// 并发安全性（hashToken 多 goroutine 不互相污染 HMAC 内部状态）
// =============================================================================

func TestHash_ConcurrentSafe(t *testing.T) {
	pool := mustOpenTestPool(t)
	t.Cleanup(pool.Close)
	svc := newTestService(t, pool)

	const N = 100
	want := svc.hashToken("baseline-plaintext")

	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			got := svc.hashToken("baseline-plaintext")
			require.Equal(t, want, got)
		}()
	}
	wg.Wait()
}

// =============================================================================
// helpers used by tests
// =============================================================================

// filterByDescPrefix 按 description 前缀过滤 token 列表，便于并行测试中拿到自己的子集。
func filterByDescPrefix(tokens []*Token, prefix string) []*Token {
	var out []*Token
	for _, tk := range tokens {
		if strings.HasPrefix(tk.Description, prefix) {
			out = append(out, tk)
		}
	}
	return out
}

// 编译时确认 pool 类型；tests 用 pool 类型而非接口。
var _ = (*pgxpool.Pool)(nil)

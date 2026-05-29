package channel

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sunxin-git/api-gateway/internal/crypto"
)

// 测试 DSN：优先 CHANNEL_TEST_PG_DSN，再复用共享 LEDGER_TEST_PG_DSN，最后 docker 默认。
const defaultChannelTestDSN = "postgres://gateway:gateway_dev@127.0.0.1:55432/gateway?sslmode=disable"

func testDSN() string {
	for _, k := range []string{"CHANNEL_TEST_PG_DSN", "LEDGER_TEST_PG_DSN"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return defaultChannelTestDSN
}

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func mustKeyring(t *testing.T) *crypto.Keyring {
	t.Helper()
	key := make([]byte, crypto.KEKBytes)
	_, err := rand.Read(key)
	require.NoError(t, err)
	kr, err := crypto.NewKeyring(map[int32][]byte{1: key})
	require.NoError(t, err)
	return kr
}

func mustPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := testDSN()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skipf("跳过：无法构造 pool (%s)：%v", dsn, err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("跳过：无法连 PG (%s)：%v", dsn, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// uniqueName 生成本测试唯一的名字/标识，避开 channel.name UNIQUE 冲突 + 并发干扰。
func uniqueName(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// createChannel 创建渠道并注册 cleanup（按 id 删除）。
func createChannel(t *testing.T, svc *PostgresService, pool *pgxpool.Pool, params CreateParams) *Channel {
	t.Helper()
	ch, err := svc.Create(context.Background(), params)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = pool.Exec(ctx, "DELETE FROM channel WHERE id = $1", ch.ID)
	})
	return ch
}

func fullCreds() ChannelCredentials {
	return ChannelCredentials{
		APIKey:       "ark-12345678-aaaa-bbbb-cccc-APIKEYSECRETTAIL",
		ARKAccessKey: "AKLTarkaccesskey0001",
		ARKSecretKey: "ARK-SECRET-VALUE-must-not-leak",
		TOSAccessKey: "AKLTtosaccesskey0002",
		TOSSecretKey: "TOS-SECRET-VALUE-must-not-leak",
		TOSBucket:    "biz-001-videos",
		TOSEndpoint:  "tos-cn-beijing.volces.com",
		TOSRegion:    "cn-beijing",
		ProjectID:    "proj-abc-123",
	}
}

// secretSubstrings 是绝不应出现在任何掩码视图里的明文片段。
func secretSubstrings() []string {
	return []string{
		"APIKEYSECRETTAIL",
		"ARK-SECRET-VALUE-must-not-leak",
		"TOS-SECRET-VALUE-must-not-leak",
	}
}

func assertNoSecretLeak(t *testing.T, masked MaskedCredentials) {
	t.Helper()
	blob, err := json.Marshal(masked)
	require.NoError(t, err)
	for _, secret := range secretSubstrings() {
		assert.NotContains(t, string(blob), secret, "掩码视图泄露了明文片段")
	}
}

func TestCreate_GetByID_Masked(t *testing.T) {
	pool := mustPool(t)
	svc := NewPostgresService(pool, mustKeyring(t), silentLogger())

	created := createChannel(t, svc, pool, CreateParams{
		Name:         uniqueName("test-chan-masked"),
		ProviderType: uniqueName("test-prov"),
		Enabled:      true,
		Credentials:  fullCreds(),
	})

	// 创建返回的视图即掩码
	assertNoSecretLeak(t, created.Credentials)
	assert.Equal(t, maskSet, created.Credentials.ARKSecretKey, "密钥类应固定占位")
	assert.Equal(t, maskSet, created.Credentials.TOSSecretKey)
	assert.Equal(t, "biz-001-videos", created.Credentials.TOSBucket, "非机密标识符原样")
	assert.Equal(t, "ark-12***", created.Credentials.APIKey, "APIKey 仅前缀")
	assert.Equal(t, int32(1), created.KeyVersion)

	// GetByID 同样掩码
	got, err := svc.GetByID(context.Background(), created.ID)
	require.NoError(t, err)
	assertNoSecretLeak(t, got.Credentials)
	assert.Equal(t, maskSet, got.Credentials.ARKSecretKey)
}

func TestGetCredentialsForUpstream_Roundtrip(t *testing.T) {
	pool := mustPool(t)
	svc := NewPostgresService(pool, mustKeyring(t), silentLogger())
	want := fullCreds()

	ch := createChannel(t, svc, pool, CreateParams{
		Name:         uniqueName("test-chan-rt"),
		ProviderType: uniqueName("test-prov"),
		Enabled:      true,
		Credentials:  want,
	})

	got, err := svc.GetCredentialsForUpstream(context.Background(), ch.ID)
	require.NoError(t, err)
	assert.Equal(t, want, *got, "5 段凭据 encrypt→store→decrypt 往返必须一致")
}

func TestEmptyCreds_Roundtrip(t *testing.T) {
	pool := mustPool(t)
	svc := NewPostgresService(pool, mustKeyring(t), silentLogger())

	ch := createChannel(t, svc, pool, CreateParams{
		Name:         uniqueName("test-chan-empty"),
		ProviderType: uniqueName("test-prov"),
		Enabled:      true,
		Credentials:  ChannelCredentials{},
	})

	got, err := svc.GetCredentialsForUpstream(context.Background(), ch.ID)
	require.NoError(t, err)
	assert.Equal(t, ChannelCredentials{}, *got)

	view, err := svc.GetByID(context.Background(), ch.ID)
	require.NoError(t, err)
	assert.Equal(t, maskUnset, view.Credentials.ARKSecretKey, "空密钥应显示未设置")
}

func TestUpdateCredentials(t *testing.T) {
	pool := mustPool(t)
	svc := NewPostgresService(pool, mustKeyring(t), silentLogger())

	ch := createChannel(t, svc, pool, CreateParams{
		Name:         uniqueName("test-chan-upd"),
		ProviderType: uniqueName("test-prov"),
		Enabled:      true,
		Credentials:  fullCreds(),
	})

	newCreds := fullCreds()
	newCreds.APIKey = "ark-99999999-rotated-NEWKEYTAIL"
	newCreds.TOSBucket = "biz-001-videos-v2"

	_, err := svc.UpdateCredentials(context.Background(), ch.ID, newCreds)
	require.NoError(t, err)

	got, err := svc.GetCredentialsForUpstream(context.Background(), ch.ID)
	require.NoError(t, err)
	assert.Equal(t, newCreds, *got)
}

func TestDecryptFail_WrongKEK(t *testing.T) {
	pool := mustPool(t)
	svcA := NewPostgresService(pool, mustKeyring(t), silentLogger())
	ch := createChannel(t, svcA, pool, CreateParams{
		Name:         uniqueName("test-chan-wrongkek"),
		ProviderType: uniqueName("test-prov"),
		Enabled:      true,
		Credentials:  fullCreds(),
	})

	// 不同 KEK（同版本号 1）→ 解密 fail-closed
	svcB := NewPostgresService(pool, mustKeyring(t), silentLogger())

	_, err := svcB.GetCredentialsForUpstream(context.Background(), ch.ID)
	assert.ErrorIs(t, err, ErrDecryptFailed, "错误 KEK 必须 fail-closed，不返明文")

	// 掩码视图降级为「解密失败」，不暴露明文
	view, err := svcB.GetByID(context.Background(), ch.ID)
	require.NoError(t, err)
	assert.Equal(t, maskDecFailed, view.Credentials.ARKSecretKey)
	assertNoSecretLeak(t, view.Credentials)
}

func TestListActiveAndByProvider(t *testing.T) {
	pool := mustPool(t)
	svc := NewPostgresService(pool, mustKeyring(t), silentLogger())
	prov := uniqueName("test-prov")

	ch := createChannel(t, svc, pool, CreateParams{
		Name:         uniqueName("test-chan-list"),
		ProviderType: prov,
		Enabled:      true,
		Credentials:  fullCreds(),
	})

	byProv, err := svc.ListActiveByProvider(context.Background(), prov)
	require.NoError(t, err)
	require.Len(t, byProv, 1, "唯一 provider_type 应只命中本测试渠道")
	assert.Equal(t, ch.ID, byProv[0].ID)
	assertNoSecretLeak(t, byProv[0].Credentials)

	all, err := svc.ListActive(context.Background())
	require.NoError(t, err)
	var found bool
	for _, c := range all {
		if c.ID == ch.ID {
			found = true
		}
	}
	assert.True(t, found, "ListActive 应含本测试渠道")
}

func TestSetEnabledAndDelete(t *testing.T) {
	pool := mustPool(t)
	svc := NewPostgresService(pool, mustKeyring(t), silentLogger())
	ch := createChannel(t, svc, pool, CreateParams{
		Name:         uniqueName("test-chan-toggle"),
		ProviderType: uniqueName("test-prov"),
		Enabled:      true,
		Credentials:  fullCreds(),
	})

	require.NoError(t, svc.SetEnabled(context.Background(), ch.ID, false))
	got, err := svc.GetByID(context.Background(), ch.ID)
	require.NoError(t, err)
	assert.False(t, got.Enabled)

	existed, err := svc.Delete(context.Background(), ch.ID)
	require.NoError(t, err)
	assert.True(t, existed)

	_, err = svc.GetByID(context.Background(), ch.ID)
	assert.ErrorIs(t, err, ErrChannelNotFound)
}

func TestGetByID_NotFound(t *testing.T) {
	pool := mustPool(t)
	svc := NewPostgresService(pool, mustKeyring(t), silentLogger())
	_, err := svc.GetByID(context.Background(), -99999)
	assert.ErrorIs(t, err, ErrChannelNotFound)
}

func TestSetEnabled_NotFound(t *testing.T) {
	pool := mustPool(t)
	svc := NewPostgresService(pool, mustKeyring(t), silentLogger())
	err := svc.SetEnabled(context.Background(), -99999, false)
	assert.ErrorIs(t, err, ErrChannelNotFound)
}

package storage

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func validTOSConfig() TOSConfig {
	return TOSConfig{
		AccessKey: "AKLTtest",
		SecretKey: "secrettest",
		Bucket:    "gw-results",
		Endpoint:  "https://tos-cn-beijing.volces.com",
		Region:    "cn-beijing",
	}
}

func TestNewTOSObjectStore_Validation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*TOSConfig)
	}{
		{"empty_access_key", func(c *TOSConfig) { c.AccessKey = "" }},
		{"empty_secret_key", func(c *TOSConfig) { c.SecretKey = "" }},
		{"empty_bucket", func(c *TOSConfig) { c.Bucket = "" }},
		{"empty_endpoint", func(c *TOSConfig) { c.Endpoint = "" }},
		{"empty_region", func(c *TOSConfig) { c.Region = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validTOSConfig()
			tc.mutate(&cfg)
			_, err := NewTOSObjectStore(cfg)
			require.Error(t, err, "缺字段须 fail-fast")
		})
	}
}

func TestNewTOSObjectStore_Happy(t *testing.T) {
	store, err := NewTOSObjectStore(validTOSConfig())
	require.NoError(t, err)
	require.NotNil(t, store)
	assert.Equal(t, "gw-results", store.Bucket())
	assert.Equal(t, "cn-beijing", store.Region())
	assert.Equal(t, "https://tos-cn-beijing.volces.com", store.Endpoint())
}

// TestPresignGet 验证签名 URL 现签（本地计算，无网络）：含 key + 过期/签名查询参数。
func TestPresignGet(t *testing.T) {
	store, err := NewTOSObjectStore(validTOSConfig())
	require.NoError(t, err)

	url, err := store.PresignGet("video/vtask_abc123.mp4", 15*time.Minute)
	require.NoError(t, err)
	require.NotEmpty(t, url)
	assert.True(t, strings.HasPrefix(url, "https://"), "签名 URL 应为 https")
	assert.Contains(t, url, "vtask_abc123.mp4", "URL 含对象 key")
	assert.Contains(t, url, "?", "签名 URL 带查询参数（签名/过期）")
	// TOS V4 签名参数前缀 X-Tos-（大小写不敏感地校验存在签名要素）。
	assert.Contains(t, strings.ToLower(url), "x-tos-", "含 TOS V4 签名查询参数")
}

func TestClampPresignSeconds(t *testing.T) {
	assert.Equal(t, int64(1), clampPresignSeconds(0), "下界截断到 1s")
	assert.Equal(t, int64(1), clampPresignSeconds(-5*time.Second), "负值截断到 1s")
	assert.Equal(t, int64(900), clampPresignSeconds(15*time.Minute), "正常值取秒")
	assert.Equal(t, int64(604800), clampPresignSeconds(8*24*time.Hour), "上界截断到 7d")
}

package relay

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validCatalogConfig 测试基线；各 case 在此基础上 mutate 单字段验证 fail-fast。
func validCatalogConfig() CatalogConfig {
	return CatalogConfig{
		ModelName:             "gw-default",
		UpstreamProviderType:  "openai_compat",
		UpstreamBaseURL:       "https://ark.cn-beijing.volces.com/api/v3",
		UpstreamAPIKey:        "test-ark-key-xxx",
		UpstreamModelName:     "doubao-1-5-pro-32k-250115",
		PriceInputPer1MMinor:  800,
		PriceOutputPer1MMinor: 2000,
		MaxContextTokens:      32768,
	}
}

func TestNewEnvCatalog_Happy(t *testing.T) {
	cfg := validCatalogConfig()
	cat, err := NewEnvCatalog(cfg)
	require.NoError(t, err)
	require.NotNil(t, cat)

	entry := cat.DefaultEntry()
	require.NotNil(t, entry)
	assert.Equal(t, "gw-default", entry.GatewayModelName)
	assert.Equal(t, "openai_compat", entry.UpstreamProviderType)
	assert.Equal(t, "https://ark.cn-beijing.volces.com/api/v3", entry.UpstreamBaseURL)
	assert.Equal(t, "doubao-1-5-pro-32k-250115", entry.UpstreamModelName)
	assert.Equal(t, int64(800), entry.PriceInputPer1MMinor)
	assert.Equal(t, int64(2000), entry.PriceOutputPer1MMinor)
	assert.Equal(t, int32(32768), entry.MaxContextTokens)
}

func TestEnvCatalog_LookupIgnoresInput(t *testing.T) {
	cfg := validCatalogConfig()
	cat, err := NewEnvCatalog(cfg)
	require.NoError(t, err)

	// MVP 单条字典：业务传任何 model 都路由到唯一条
	for _, name := range []string{"gw-default", "gw-fast", "gpt-4", "random-model"} {
		entry, ok := cat.Lookup(name)
		require.True(t, ok, "Lookup %q 必须命中", name)
		assert.Equal(t, "gw-default", entry.GatewayModelName)
	}
}

func TestEnvCatalog_All(t *testing.T) {
	cfg := validCatalogConfig()
	cat, err := NewEnvCatalog(cfg)
	require.NoError(t, err)

	all := cat.All()
	require.Len(t, all, 1, "MVP 单条字典")
	assert.Equal(t, "gw-default", all[0].GatewayModelName)
}

func TestNewEnvCatalog_FailFastMatrix(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(*CatalogConfig)
		expectSub string
	}{
		{"empty_model_name", func(c *CatalogConfig) { c.ModelName = "" }, "ModelName"},
		{"whitespace_model_name", func(c *CatalogConfig) { c.ModelName = "   " }, "ModelName"},
		{"empty_provider", func(c *CatalogConfig) { c.UpstreamProviderType = "" }, "UpstreamProviderType"},
		{"unknown_provider", func(c *CatalogConfig) { c.UpstreamProviderType = "ollama" }, "openai_compat"},
		{"empty_base_url", func(c *CatalogConfig) { c.UpstreamBaseURL = "" }, "UpstreamBaseURL"},
		{"invalid_url", func(c *CatalogConfig) { c.UpstreamBaseURL = "://invalid" }, "UpstreamBaseURL"},
		{"non_http_scheme", func(c *CatalogConfig) { c.UpstreamBaseURL = "ftp://x.com" }, "http|https"},
		{"missing_host", func(c *CatalogConfig) { c.UpstreamBaseURL = "https:///path" }, "host"},
		{"empty_api_key", func(c *CatalogConfig) { c.UpstreamAPIKey = "" }, "UpstreamAPIKey"},
		{"empty_upstream_model", func(c *CatalogConfig) { c.UpstreamModelName = "" }, "UpstreamModelName"},
		{"zero_input_price", func(c *CatalogConfig) { c.PriceInputPer1MMinor = 0 }, "PriceInputPer1MMinor"},
		{"negative_input_price", func(c *CatalogConfig) { c.PriceInputPer1MMinor = -1 }, "PriceInputPer1MMinor"},
		{"zero_output_price", func(c *CatalogConfig) { c.PriceOutputPer1MMinor = 0 }, "PriceOutputPer1MMinor"},
		{"zero_max_context", func(c *CatalogConfig) { c.MaxContextTokens = 0 }, "MaxContextTokens"},
		{"huge_max_context", func(c *CatalogConfig) { c.MaxContextTokens = 10_000_000 }, "MaxContextTokens"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validCatalogConfig()
			tc.mutate(&cfg)
			_, err := NewEnvCatalog(cfg)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.expectSub)
		})
	}
}

func TestEnvCatalog_HTTPBaseURLAllowedForDev(t *testing.T) {
	// dev 模式（RequireHTTPS=false）允许 http upstream（如本地 mock）
	cfg := validCatalogConfig()
	cfg.UpstreamBaseURL = "http://localhost:11434/v1"
	_, err := NewEnvCatalog(cfg)
	require.NoError(t, err)
}

func TestEnvCatalog_RequireHTTPS_RejectsHTTP(t *testing.T) {
	// production 模式（RequireHTTPS=true）拒绝 http upstream
	cfg := validCatalogConfig()
	cfg.UpstreamBaseURL = "http://ark.example.com/v1"
	cfg.RequireHTTPS = true
	_, err := NewEnvCatalog(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "https")
}

func TestEnvCatalog_RequireHTTPS_AcceptsHTTPS(t *testing.T) {
	cfg := validCatalogConfig()
	cfg.UpstreamBaseURL = "https://ark.cn-beijing.volces.com/api/v3"
	cfg.RequireHTTPS = true
	_, err := NewEnvCatalog(cfg)
	require.NoError(t, err)
}

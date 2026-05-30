package video

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validVideoCatalogConfig 测试基线；各 case 在此基础上 mutate 单字段验证 fail-fast。
func validVideoCatalogConfig() CatalogConfig {
	return CatalogConfig{
		GatewayModelName:     "gw-video",
		UpstreamProviderType: ProviderTypeVolcSeedance,
		UpstreamBaseURL:      "https://ark.cn-beijing.volces.com/api/v3",
		UpstreamModelName:    "doubao-seedance-2-0-t2v",
		ChannelName:          "seedance-main",
		RequireHTTPS:         false,

		Price480pPer1MMinor:  4600,
		Price720pPer1MMinor:  4600,
		Price1080pPer1MMinor: 5100,
		BillingMultiplierBP:  11000,

		DurationMinSeconds:     4,
		DurationMaxSeconds:     15,
		DurationDefaultSeconds: 5,
		FpsDefault:             24,
		FpsMax:                 30,
		Ratios:                 []string{"16:9", "9:16", "1:1", "adaptive"},
		RatioDefault:           "16:9",
		ResolutionDefault:      "720p",
	}
}

func TestNewEnvVideoCatalog_Happy(t *testing.T) {
	cat, err := NewEnvVideoCatalog(validVideoCatalogConfig())
	require.NoError(t, err)
	require.NotNil(t, cat)

	entry := cat.DefaultEntry()
	require.NotNil(t, entry)
	assert.Equal(t, "gw-video", entry.GatewayModelName)
	assert.Equal(t, ProviderTypeVolcSeedance, entry.UpstreamProviderType)
	assert.Equal(t, "https://ark.cn-beijing.volces.com/api/v3", entry.UpstreamBaseURL)
	assert.Equal(t, "doubao-seedance-2-0-t2v", entry.UpstreamModelName)
	assert.Equal(t, "seedance-main", entry.ChannelName)

	// pricing：三档均在售
	require.NotNil(t, entry.Pricing)
	assert.Equal(t, int64(11000), entry.Pricing.BillingMultiplierBP)
	assert.Equal(t, []string{"480p", "720p", "1080p"}, entry.Pricing.OfferedResolutions())

	t720, ok := entry.Pricing.Tier("720p")
	require.True(t, ok)
	assert.Equal(t, int64(4600), t720.PricePerMillionTokensMinor)
	assert.Equal(t, int32(1280), t720.LongSidePx)
	assert.Equal(t, int64(1280*1280), t720.MaxFramePixels())

	t1080, ok := entry.Pricing.Tier("1080p")
	require.True(t, ok)
	assert.Equal(t, int64(5100), t1080.PricePerMillionTokensMinor)
	assert.Equal(t, int64(1920*1920), t1080.MaxFramePixels())

	// capability：仅 text_to_video 在支持集
	capb := entry.Capability
	require.NotNil(t, capb)
	assert.Equal(t, capabilitySchemaV1, capb.SchemaVersion)
	assert.True(t, capb.SupportsTaskType(TaskTypeTextToVideo))
	assert.False(t, capb.SupportsTaskType(TaskTypeImageToVideo))
	assert.False(t, capb.SupportsTaskType(TaskTypeStartEndFrame))

	// resolution 枚举 = 在售档（与 pricing 一致）
	resSpec, ok := capb.paramByKey("resolution")
	require.True(t, ok)
	assert.Equal(t, ParamTypeEnum, resSpec.Type)
	assert.Equal(t, []string{"480p", "720p", "1080p"}, resSpec.Enum)
	assert.Equal(t, "720p", resSpec.Default)

	// prompt 必填
	promptSpec, ok := capb.paramByKey("prompt")
	require.True(t, ok)
	assert.True(t, promptSpec.Required)

	// duration 取值档
	durSpec, ok := capb.paramByKey("duration")
	require.True(t, ok)
	assert.Equal(t, int64(4), durSpec.Min)
	assert.Equal(t, int64(15), durSpec.Max)
	assert.Equal(t, int64(5), durSpec.Default)
}

func TestEnvVideoCatalog_LookupIgnoresInput(t *testing.T) {
	cat, err := NewEnvVideoCatalog(validVideoCatalogConfig())
	require.NoError(t, err)

	for _, name := range []string{"gw-video", "gw-fast", "seedance", "random"} {
		entry, ok := cat.Lookup(name)
		require.True(t, ok, "Lookup %q 必须命中", name)
		assert.Equal(t, "gw-video", entry.GatewayModelName)
	}
}

func TestEnvVideoCatalog_All(t *testing.T) {
	cat, err := NewEnvVideoCatalog(validVideoCatalogConfig())
	require.NoError(t, err)

	all := cat.All()
	require.Len(t, all, 1, "MVP 单条字典")
	assert.Equal(t, "gw-video", all[0].GatewayModelName)
}

// TestVideoCatalog_PriceZeroExcludesTier：未配价的档不在售（不进枚举/定价表）。
func TestVideoCatalog_PriceZeroExcludesTier(t *testing.T) {
	cfg := validVideoCatalogConfig()
	cfg.Price480pPer1MMinor = 0 // 480p 下线

	cat, err := NewEnvVideoCatalog(cfg)
	require.NoError(t, err)
	entry := cat.DefaultEntry()

	assert.Equal(t, []string{"720p", "1080p"}, entry.Pricing.OfferedResolutions())
	_, ok := entry.Pricing.Tier("480p")
	assert.False(t, ok, "480p 未配价应不在售")

	resSpec, ok := entry.Capability.paramByKey("resolution")
	require.True(t, ok)
	assert.Equal(t, []string{"720p", "1080p"}, resSpec.Enum, "capability 枚举须随在售档收缩")
}

// TestVideoCatalog_SingleTier：仅一档在售也合法（最小可售集）。
func TestVideoCatalog_SingleTier(t *testing.T) {
	cfg := validVideoCatalogConfig()
	cfg.Price480pPer1MMinor = 0
	cfg.Price1080pPer1MMinor = 0
	cfg.ResolutionDefault = "720p"

	cat, err := NewEnvVideoCatalog(cfg)
	require.NoError(t, err)
	assert.Equal(t, []string{"720p"}, cat.DefaultEntry().Pricing.OfferedResolutions())
}

// TestResolutionTier_MaxFramePixels：长边² 上界正确（reserve 可证上界依据）。
func TestResolutionTier_MaxFramePixels(t *testing.T) {
	cases := map[string]int64{
		"480p":  854 * 854,
		"720p":  1280 * 1280,
		"1080p": 1920 * 1920,
	}
	cat, err := NewEnvVideoCatalog(validVideoCatalogConfig())
	require.NoError(t, err)
	pricing := cat.DefaultEntry().Pricing
	for res, want := range cases {
		tier, ok := pricing.Tier(res)
		require.True(t, ok)
		assert.Equal(t, want, tier.MaxFramePixels(), "%s 长边² 上界", res)
	}
}

func TestNewEnvVideoCatalog_FailFastMatrix(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(*CatalogConfig)
		expectSub string
	}{
		{"empty_model_name", func(c *CatalogConfig) { c.GatewayModelName = "" }, "GatewayModelName"},
		{"whitespace_model_name", func(c *CatalogConfig) { c.GatewayModelName = "   " }, "GatewayModelName"},
		{"empty_provider", func(c *CatalogConfig) { c.UpstreamProviderType = "" }, "UpstreamProviderType"},
		{"wrong_provider_openai", func(c *CatalogConfig) { c.UpstreamProviderType = "openai_compat" }, "volc_seedance"},
		{"empty_base_url", func(c *CatalogConfig) { c.UpstreamBaseURL = "" }, "UpstreamBaseURL"},
		{"invalid_url", func(c *CatalogConfig) { c.UpstreamBaseURL = "://invalid" }, "UpstreamBaseURL"},
		{"non_http_scheme", func(c *CatalogConfig) { c.UpstreamBaseURL = "ftp://x.com" }, "http|https"},
		{"missing_host", func(c *CatalogConfig) { c.UpstreamBaseURL = "https:///path" }, "host"},
		{"require_https_http", func(c *CatalogConfig) {
			c.RequireHTTPS = true
			c.UpstreamBaseURL = "http://ark.cn-beijing.volces.com/api/v3"
		}, "https"},
		{"empty_upstream_model", func(c *CatalogConfig) { c.UpstreamModelName = "" }, "UpstreamModelName"},
		{"empty_channel", func(c *CatalogConfig) { c.ChannelName = "" }, "ChannelName"},
		{"all_prices_zero", func(c *CatalogConfig) {
			c.Price480pPer1MMinor = 0
			c.Price720pPer1MMinor = 0
			c.Price1080pPer1MMinor = 0
		}, "至少一个分辨率档"},
		{"negative_price", func(c *CatalogConfig) { c.Price720pPer1MMinor = -1 }, "不能为负"},
		{"zero_multiplier", func(c *CatalogConfig) { c.BillingMultiplierBP = 0 }, "BillingMultiplierBP"},
		{"negative_multiplier", func(c *CatalogConfig) { c.BillingMultiplierBP = -1 }, "BillingMultiplierBP"},
		{"duration_min_zero", func(c *CatalogConfig) { c.DurationMinSeconds = 0 }, "DurationMinSeconds"},
		{"duration_max_lt_min", func(c *CatalogConfig) {
			c.DurationMinSeconds = 10
			c.DurationMaxSeconds = 5
		}, "DurationMaxSeconds"},
		{"duration_default_oob", func(c *CatalogConfig) { c.DurationDefaultSeconds = 99 }, "DurationDefaultSeconds"},
		{"fps_max_zero", func(c *CatalogConfig) { c.FpsMax = 0 }, "FpsMax"},
		{"fps_default_oob", func(c *CatalogConfig) { c.FpsDefault = 99 }, "FpsDefault"},
		{"duration_max_over_cap", func(c *CatalogConfig) { c.DurationMaxSeconds = maxDurationSecondsCap + 1 }, "硬顶"},
		{"fps_max_over_cap", func(c *CatalogConfig) {
			c.FpsMax = maxFpsCap + 1
			c.FpsDefault = 24
		}, "硬顶"},
		{"empty_ratios", func(c *CatalogConfig) { c.Ratios = nil }, "Ratios"},
		{"blank_ratios", func(c *CatalogConfig) { c.Ratios = []string{"  ", ""} }, "Ratios"},
		{"ratio_default_not_in_set", func(c *CatalogConfig) { c.RatioDefault = "21:9" }, "RatioDefault"},
		{"resolution_default_not_offered", func(c *CatalogConfig) {
			c.Price480pPer1MMinor = 0
			c.ResolutionDefault = "480p" // 已下线
		}, "ResolutionDefault"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validVideoCatalogConfig()
			tc.mutate(&cfg)
			_, err := NewEnvVideoCatalog(cfg)
			require.Error(t, err, "应 fail-fast")
			assert.Contains(t, err.Error(), tc.expectSub)
		})
	}
}

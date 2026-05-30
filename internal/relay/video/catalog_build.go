package video

import (
	"fmt"
	"net/url"
	"strings"
)

// CatalogConfig 是 EnvVideoCatalog 的加载配置（计划 §决策：relay 包不 import config）。
//
// 解耦：video 包不直接依赖 internal/config（避免循环依赖 + 让 video 包可独立测试）。
// main.go（Unit 10 装配）从 cfg.VideoRelay* 字段映射到本结构。
type CatalogConfig struct {
	GatewayModelName     string
	UpstreamProviderType string
	UpstreamBaseURL      string
	UpstreamModelName    string
	ChannelName          string

	// RequireHTTPS production 模式强制上游 base url scheme = https（防明文上游泄露凭据）。
	// main.go 装配时设 = (cfg.GatewayEnv == production)。
	RequireHTTPS bool

	// 分辨率档单价（CNY 分 / 百万 token）；**0 = 该档不在售**（不进 capability 枚举、不进定价表）。
	// 至少一档须 > 0；负值非法。
	Price480pPer1MMinor  int64
	Price720pPer1MMinor  int64
	Price1080pPer1MMinor int64
	// BillingMultiplierBP 商业加价倍率基点（10000=1.0×）；须 > 0。
	BillingMultiplierBP int64

	// 取值档（能力描述符约束来源）。
	DurationMinSeconds     int64
	DurationMaxSeconds     int64
	DurationDefaultSeconds int64
	FpsDefault             int64
	FpsMax                 int64
	Ratios                 []string // aspect ratio 枚举（如 16:9 / 9:16 / 1:1 / adaptive）
	RatioDefault           string
	ResolutionDefault      string
}

// duration / fps 上界硬顶（ce-review adversarial/api-contract）。
//
// 仅校验 >= 下界不足以防 Unit 7 的 reserve token 公式 `W×H × duration × fps`（W×H 上界
// 已达 1920²≈3.7e6）在 int64 溢出：若运维误配 FpsMax=2e9，下游相乘即溢出。设宽松但安全
// 的硬顶（覆盖任何真实视频场景，远低于溢出阈值），把误配挡在启动期（fail-closed）。
const (
	maxDurationSecondsCap = 3600 // 1 小时，远超 seedance 单任务时长
	maxFpsCap             = 240  // 远超任何真实视频帧率
)

// NewEnvVideoCatalog 从 CatalogConfig 加载视频字典 + fail-fast 校验（计划 §Verification：构造 fail-fast 矩阵）。
//
// 校验失败拒启动（main.go 装配时调用）。构造同时从配置派生 capability（其 resolution
// 枚举 = 在售档），保证「能选的档 = 有价的档」自洽。
func NewEnvVideoCatalog(cfg CatalogConfig) (*EnvVideoCatalog, error) {
	if err := validateCatalogIdentity(cfg); err != nil {
		return nil, err
	}

	pricing, err := buildPricing(cfg)
	if err != nil {
		return nil, err
	}
	offered := pricing.OfferedResolutions()

	capability, err := buildCapability(cfg, offered)
	if err != nil {
		return nil, err
	}

	return &EnvVideoCatalog{
		entry: &VideoModelEntry{
			GatewayModelName:     cfg.GatewayModelName,
			UpstreamProviderType: cfg.UpstreamProviderType,
			UpstreamBaseURL:      cfg.UpstreamBaseURL,
			UpstreamModelName:    cfg.UpstreamModelName,
			ChannelName:          cfg.ChannelName,
			Capability:           capability,
			Pricing:              pricing,
		},
	}, nil
}

func validateCatalogIdentity(cfg CatalogConfig) error {
	if strings.TrimSpace(cfg.GatewayModelName) == "" {
		return fmt.Errorf("video catalog: GatewayModelName 不能为空")
	}
	if strings.TrimSpace(cfg.UpstreamProviderType) == "" {
		return fmt.Errorf("video catalog: UpstreamProviderType 不能为空")
	}
	if _, ok := validVideoProviderTypes[cfg.UpstreamProviderType]; !ok {
		return fmt.Errorf(
			"video catalog: UpstreamProviderType=%q 非法；MVP 唯一合法值 %s",
			cfg.UpstreamProviderType, ProviderTypeVolcSeedance,
		)
	}
	if strings.TrimSpace(cfg.UpstreamBaseURL) == "" {
		return fmt.Errorf("video catalog: UpstreamBaseURL 不能为空")
	}
	u, err := url.Parse(cfg.UpstreamBaseURL)
	if err != nil {
		return fmt.Errorf("video catalog: UpstreamBaseURL 解析失败 %q: %w", cfg.UpstreamBaseURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("video catalog: UpstreamBaseURL scheme 必须 http|https（当前 %q）", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("video catalog: UpstreamBaseURL 缺 host: %q", cfg.UpstreamBaseURL)
	}
	if cfg.RequireHTTPS && u.Scheme != "https" {
		return fmt.Errorf(
			"video catalog: production 模式 UpstreamBaseURL 必须 https（防明文上游泄露凭据；当前 %q）",
			u.Scheme,
		)
	}
	if strings.TrimSpace(cfg.UpstreamModelName) == "" {
		return fmt.Errorf("video catalog: UpstreamModelName 不能为空")
	}
	if strings.TrimSpace(cfg.ChannelName) == "" {
		return fmt.Errorf("video catalog: ChannelName 不能为空（凭据来源 channel 绑定）")
	}
	return nil
}

// buildPricing 从配置组装分辨率档定价表（price > 0 的档才在售）。
func buildPricing(cfg CatalogConfig) (*Pricing, error) {
	if cfg.BillingMultiplierBP <= 0 {
		return nil, fmt.Errorf("video catalog: BillingMultiplierBP 必须 > 0（当前 %d）", cfg.BillingMultiplierBP)
	}

	priceByRes := map[string]int64{
		Resolution480p:  cfg.Price480pPer1MMinor,
		Resolution720p:  cfg.Price720pPer1MMinor,
		Resolution1080p: cfg.Price1080pPer1MMinor,
	}

	tiers := make(map[string]ResolutionTier)
	for _, res := range orderedResolutions {
		price := priceByRes[res]
		if price < 0 {
			return nil, fmt.Errorf("video catalog: %s 单价不能为负（当前 %d）", res, price)
		}
		if price == 0 {
			continue // 未配价 = 该档不在售
		}
		tiers[res] = ResolutionTier{
			Resolution:                 res,
			LongSidePx:                 resolutionLongSidePx[res],
			PricePerMillionTokensMinor: price,
		}
	}
	if len(tiers) == 0 {
		return nil, fmt.Errorf(
			"video catalog: 至少一个分辨率档须配价 > 0（480p/720p/1080p 全为 0 = 无可售档）",
		)
	}
	return &Pricing{tiers: tiers, BillingMultiplierBP: cfg.BillingMultiplierBP}, nil
}

// buildCapability 从配置派生 text_to_video 能力描述符（resolution 枚举 = 在售档）。
func buildCapability(cfg CatalogConfig, offeredResolutions []string) (*Capability, error) {
	if err := validateDurationFpsBounds(cfg); err != nil {
		return nil, err
	}

	// ratio 枚举校验 + 去重
	ratios := dedupeNonEmpty(cfg.Ratios)
	if len(ratios) == 0 {
		return nil, fmt.Errorf("video catalog: Ratios 枚举不能为空")
	}
	if !containsString(ratios, cfg.RatioDefault) {
		return nil, fmt.Errorf("video catalog: RatioDefault(%q) 不在 Ratios %v 内", cfg.RatioDefault, ratios)
	}

	// resolution 默认值须在在售档内
	if !containsString(offeredResolutions, cfg.ResolutionDefault) {
		return nil, fmt.Errorf(
			"video catalog: ResolutionDefault(%q) 不在在售档 %v 内（在售档 = 配价 > 0 的档）",
			cfg.ResolutionDefault, offeredResolutions,
		)
	}

	capability := &Capability{
		SchemaVersion: capabilitySchemaV1,
		supportedTaskTypes: map[TaskType]struct{}{
			TaskTypeTextToVideo: {},
		},
		params: buildTextToVideoParams(cfg, offeredResolutions, ratios),
	}
	if err := capability.validateSpec(); err != nil {
		return nil, err
	}
	return capability, nil
}

// validateDurationFpsBounds 校验 duration / fps 取值档自洽 + 上界硬顶。
func validateDurationFpsBounds(cfg CatalogConfig) error {
	if cfg.DurationMinSeconds < 1 {
		return fmt.Errorf("video catalog: DurationMinSeconds 必须 ≥ 1（当前 %d）", cfg.DurationMinSeconds)
	}
	if cfg.DurationMaxSeconds < cfg.DurationMinSeconds {
		return fmt.Errorf(
			"video catalog: DurationMaxSeconds(%d) 不能小于 DurationMinSeconds(%d)",
			cfg.DurationMaxSeconds, cfg.DurationMinSeconds,
		)
	}
	if cfg.DurationMaxSeconds > maxDurationSecondsCap {
		return fmt.Errorf(
			"video catalog: DurationMaxSeconds(%d) 超过硬顶 %d（防下游 token 计费 int64 溢出 / 拦误配）",
			cfg.DurationMaxSeconds, maxDurationSecondsCap,
		)
	}
	if cfg.DurationDefaultSeconds < cfg.DurationMinSeconds || cfg.DurationDefaultSeconds > cfg.DurationMaxSeconds {
		return fmt.Errorf(
			"video catalog: DurationDefaultSeconds(%d) 须 ∈ [%d, %d]",
			cfg.DurationDefaultSeconds, cfg.DurationMinSeconds, cfg.DurationMaxSeconds,
		)
	}
	if cfg.FpsMax < 1 {
		return fmt.Errorf("video catalog: FpsMax 必须 ≥ 1（当前 %d）", cfg.FpsMax)
	}
	if cfg.FpsMax > maxFpsCap {
		return fmt.Errorf(
			"video catalog: FpsMax(%d) 超过硬顶 %d（防下游 token 计费 int64 溢出 / 拦误配）",
			cfg.FpsMax, maxFpsCap,
		)
	}
	if cfg.FpsDefault < 1 || cfg.FpsDefault > cfg.FpsMax {
		return fmt.Errorf("video catalog: FpsDefault(%d) 须 ∈ [1, %d]", cfg.FpsDefault, cfg.FpsMax)
	}
	return nil
}

// buildTextToVideoParams 组装 text_to_video 的参数声明集（从 catalog 取值档派生）。
//
// 第一刀校验子集：prompt 必填、duration / fps 整数取值档、resolution / ratio 枚举。
// generate_audio / remove_watermark 等布尔参数推后到 Unit 5（adapter 透传时再纳入声明）。
func buildTextToVideoParams(cfg CatalogConfig, offeredResolutions, ratios []string) []ParamSpec {
	return []ParamSpec{
		{
			Key:      "prompt",
			Type:     ParamTypeString,
			Required: true,
			Title:    "提示词",
		},
		{
			Key:      "duration",
			Type:     ParamTypeInteger,
			Required: false,
			Min:      cfg.DurationMinSeconds,
			Max:      cfg.DurationMaxSeconds,
			Default:  cfg.DurationDefaultSeconds,
			Title:    "时长（秒）",
		},
		{
			Key:      "resolution",
			Type:     ParamTypeEnum,
			Required: false,
			Enum:     offeredResolutions,
			Default:  cfg.ResolutionDefault,
			Title:    "分辨率",
		},
		{
			Key:      "ratio",
			Type:     ParamTypeEnum,
			Required: false,
			Enum:     ratios,
			Default:  cfg.RatioDefault,
			Title:    "画面比例",
		},
		{
			Key:      "fps",
			Type:     ParamTypeInteger,
			Required: false,
			Min:      1,
			Max:      cfg.FpsMax,
			Default:  cfg.FpsDefault,
			Title:    "帧率",
		},
	}
}

// dedupeNonEmpty 去掉空白项并去重，保持首次出现顺序（ratio 枚举规范化）。
func dedupeNonEmpty(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

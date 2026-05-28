package relay

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// CatalogConfig env 字典加载所需配置（plan §决策 D11 字段精确）。
//
// 解耦设计：relay 包不直接 import internal/config（避免循环依赖 + 让 relay 可独立测试）。
// Unit 7 装配时 main.go 从 cfg.RelayModelName / cfg.RelayUpstream* 等映射到此结构。
type CatalogConfig struct {
	ModelName             string
	UpstreamProviderType  string
	UpstreamBaseURL       string
	UpstreamAPIKey        string
	UpstreamModelName     string
	PriceInputPer1MMinor  int64
	PriceOutputPer1MMinor int64
	MaxContextTokens      int32

	// RequireHTTPS production 模式强制上游 base url scheme = https（防明文上游连接泄露凭据）。
	// main.go 装配时设 = (cfg.GatewayEnv == production)；dev / test 允许 http（本地 mock）。
	RequireHTTPS bool
}

// Catalog model 字典接口（plan §Unit 3 Approach）。
//
// MVP 仅 EnvCatalog 单条实现；P1+ 升级为 YAMLFileCatalog / DBCatalog 时实现同接口。
//
// 业务侧 handler 路径调 Lookup(req.Model)；MVP 单条字典 Lookup 永远返同一条
// （DefaultEntry），与业务请求中的 model 字段值无关（plan §Scope Boundaries）。
type Catalog interface {
	// Lookup 按业务可见 model 名查 ModelEntry；不命中返 (nil, false)。
	// MVP 单条 EnvCatalog 实现忽略入参，永远返 DefaultEntry。
	Lookup(gatewayModelName string) (*ModelEntry, bool)

	// DefaultEntry 返字典首条；MVP 业务请求中传任何 model 都路由到此条。
	// P1+ 多条字典时本方法用于运维 fallback / 调试。
	DefaultEntry() *ModelEntry

	// All 列出所有字典条目；运维 / whoami 用。
	All() []*ModelEntry
}

// validProviderTypes MVP 唯一合法值；新增 provider 时在此扩展。
var validProviderTypes = map[string]struct{}{
	"openai_compat": {},
}

// =============================================================================
// EnvCatalog —— MVP 单条 env 实现
// =============================================================================

// EnvCatalog 从 env 加载的单条字典实现（plan §字典硬编码 1 条）。
//
// 不可变：构造后所有字段只读；可安全并发 Lookup / DefaultEntry / All。
type EnvCatalog struct {
	entry *ModelEntry
}

// 编译期断言实现 Catalog 接口。
var _ Catalog = (*EnvCatalog)(nil)

// NewEnvCatalog 从 CatalogConfig 加载字典 + fail-fast 校验。
//
// 校验：所有字段非空 + provider type 枚举 + base url 合法 http/https + 价格 > 0 +
// max_context_tokens 在合理范围 [1, 1_000_000]。
//
// Unit 7 装配时 main.go 调本函数；任一校验失败拒启动。
func NewEnvCatalog(cfg CatalogConfig) (*EnvCatalog, error) {
	if err := validateCatalogConfig(cfg); err != nil {
		return nil, err
	}
	return &EnvCatalog{
		entry: &ModelEntry{
			GatewayModelName:      cfg.ModelName,
			UpstreamProviderType:  cfg.UpstreamProviderType,
			UpstreamBaseURL:       cfg.UpstreamBaseURL,
			UpstreamAPIKey:        cfg.UpstreamAPIKey,
			UpstreamModelName:     cfg.UpstreamModelName,
			PriceInputPer1MMinor:  cfg.PriceInputPer1MMinor,
			PriceOutputPer1MMinor: cfg.PriceOutputPer1MMinor,
			MaxContextTokens:      cfg.MaxContextTokens,
		},
	}, nil
}

// Lookup MVP 单条字典：忽略入参，永远返同一条（plan §Scope Boundaries）。
//
// 业务请求中传 "gw-default" / "gw-fast" / "anything" 都命中同一字典记录。
// P1+ 多条字典时本方法按 gatewayModelName 查 map；不命中返 (nil, false)。
func (c *EnvCatalog) Lookup(_ string) (*ModelEntry, bool) {
	return c.entry, true
}

// DefaultEntry 返字典首条。
func (c *EnvCatalog) DefaultEntry() *ModelEntry {
	return c.entry
}

// All 返字典全条目列表（MVP 单条）。
func (c *EnvCatalog) All() []*ModelEntry {
	return []*ModelEntry{c.entry}
}

// =============================================================================
// fail-fast 校验
// =============================================================================

const (
	minMaxContextTokens = 1
	maxMaxContextTokens = 1_000_000
)

func validateCatalogConfig(cfg CatalogConfig) error {
	if strings.TrimSpace(cfg.ModelName) == "" {
		return errors.New("relay catalog: ModelName 不能为空")
	}
	if strings.TrimSpace(cfg.UpstreamProviderType) == "" {
		return errors.New("relay catalog: UpstreamProviderType 不能为空")
	}
	if _, ok := validProviderTypes[cfg.UpstreamProviderType]; !ok {
		return fmt.Errorf(
			"relay catalog: UpstreamProviderType=%q 非法；MVP 唯一合法值 openai_compat",
			cfg.UpstreamProviderType,
		)
	}
	if strings.TrimSpace(cfg.UpstreamBaseURL) == "" {
		return errors.New("relay catalog: UpstreamBaseURL 不能为空")
	}
	u, err := url.Parse(cfg.UpstreamBaseURL)
	if err != nil {
		return fmt.Errorf("relay catalog: UpstreamBaseURL 解析失败 %q: %w", cfg.UpstreamBaseURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf(
			"relay catalog: UpstreamBaseURL scheme 必须 http|https（当前 %q）",
			u.Scheme,
		)
	}
	if u.Host == "" {
		return fmt.Errorf("relay catalog: UpstreamBaseURL 缺 host: %q", cfg.UpstreamBaseURL)
	}
	if cfg.RequireHTTPS && u.Scheme != "https" {
		return fmt.Errorf(
			"relay catalog: production 模式 UpstreamBaseURL 必须 https（防明文上游连接泄露凭据；当前 %q）",
			u.Scheme,
		)
	}
	if strings.TrimSpace(cfg.UpstreamAPIKey) == "" {
		return errors.New("relay catalog: UpstreamAPIKey 不能为空")
	}
	if strings.TrimSpace(cfg.UpstreamModelName) == "" {
		return errors.New("relay catalog: UpstreamModelName 不能为空")
	}
	if cfg.PriceInputPer1MMinor <= 0 {
		return fmt.Errorf("relay catalog: PriceInputPer1MMinor 必须 > 0（当前 %d）", cfg.PriceInputPer1MMinor)
	}
	if cfg.PriceOutputPer1MMinor <= 0 {
		return fmt.Errorf("relay catalog: PriceOutputPer1MMinor 必须 > 0（当前 %d）", cfg.PriceOutputPer1MMinor)
	}
	if cfg.MaxContextTokens < minMaxContextTokens || cfg.MaxContextTokens > maxMaxContextTokens {
		return fmt.Errorf(
			"relay catalog: MaxContextTokens 必须 ∈ [%d, %d]（当前 %d）",
			minMaxContextTokens, maxMaxContextTokens, cfg.MaxContextTokens,
		)
	}
	return nil
}

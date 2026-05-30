package video

// ProviderTypeVolcSeedance 异步视频 provider 协议簇标识（计划 §决策：provider_type 扩 volc_seedance）。
//
// 与同步 relay 的 "openai_compat" 平行；新增第二个视频 provider 时在此扩展枚举。
const ProviderTypeVolcSeedance = "volc_seedance"

// validVideoProviderTypes MVP 唯一合法视频 provider 值。
var validVideoProviderTypes = map[string]struct{}{
	ProviderTypeVolcSeedance: {},
}

// 分辨率档 key 常量（与参考实现 / 价格表对齐）。
const (
	Resolution480p  = "480p"
	Resolution720p  = "720p"
	Resolution1080p = "1080p"
)

// resolutionLongSidePx 是各分辨率档的**长边像素数**（物理常量，非业务配置，故硬编码）。
//
// 来源：生产参考实现 storyboard-assistant `credit/pricing.py:_resolution_dimensions`
// （480p→854 / 720p→1280 / 1080p→1920，对应 16:9 的宽边）。
//
// **reserve 可证上界的关键**（计划 §决策 / Risks）：任一 aspect ratio 下视频帧的任意
// 边都 ≤ 长边，故 W×H ≤ 长边²（取等于 1:1 正方形档）。reserve 估算（Unit 7）用
// 长边² 作该档 W×H 上界，保证无论请求实际 ratio（含 adaptive 未知档）settle ≤ reserve，
// 不撞 ledger 的 ErrCommitExceedsReserved。代价：16:9 实际面积仅 9/16 长边²，reserve
// 偏保守（多锁约 1.78×）；over-reserve 安全，settle 时释放差额（计划接受此 MVP 取舍）。
//
// ⚠️ 上界成立的前提：上游各档「长边」定义不超过此处硬编码值。须在 Unit 5 集成阶段对照
// 官方文档 / 实测确认 seedance 各分辨率档实际帧尺寸 ≤ 此长边（含 adaptive 档），否则
// 会反向 under-reserve 击穿可证上界（见 ce-review residual）。
var resolutionLongSidePx = map[string]int32{
	Resolution480p:  854,
	Resolution720p:  1280,
	Resolution1080p: 1920,
}

// orderedResolutions 分辨率档的规范展示顺序（由低到高；enum / All 用，保证确定性）。
var orderedResolutions = []string{Resolution480p, Resolution720p, Resolution1080p}

// ResolutionTier 是单个分辨率档的定价 + 尺寸上界（计划 §Unit 4 Approach）。
//
// 不可变：catalog 构造后只读。
type ResolutionTier struct {
	// Resolution 档位 key（"480p"/"720p"/"1080p"）。
	Resolution string
	// LongSidePx 该档长边像素（物理常量，见 resolutionLongSidePx）。
	LongSidePx int32
	// PricePerMillionTokensMinor 该档单价：CNY 分（minor）/ 百万 token。
	// 例：seedance 2.0 720p text_to_video = ¥46.00/M → 4600。
	PricePerMillionTokensMinor int64
}

// MaxFramePixels 返回该档单帧 W×H 的**可证上界**（= 长边²）。
//
// reserve 估算（Unit 7）按请求实际档调本方法取 W×H 上界；返回 int64 防大分辨率溢出
// （1920² = 3_686_400，int32 够用但 token 公式 W×H×duration×fps 会溢 int32，故上界即用 int64）。
func (t ResolutionTier) MaxFramePixels() int64 {
	return int64(t.LongSidePx) * int64(t.LongSidePx)
}

// Pricing 是视频模型的分辨率档定价表 + 全局倍率（计划 §Unit 4：按分辨率档 {W×H, 单价, 倍率}）。
//
// 设计说明（倍率为何在 Pricing 级而非 per-tier）：参考实现 `video_credit_multiplier`
// 是单一商业加价倍率（对所有档统一），per-tier 倍率属 YAGNI。如未来某档需差异化加价，
// 再下沉到 ResolutionTier。reserve 与 settle 同用此倍率（仅 token 来源不同），故归 Pricing。
type Pricing struct {
	// tiers 按 resolution key 索引的档位表（仅含 price > 0 的「在售」档）。
	tiers map[string]ResolutionTier
	// BillingMultiplierBP 商业加价倍率，基点表示（10000 = 1.0×，11000 = 1.1×）。
	// 整数基点避免浮点货币运算（CLAUDE.md：涉钱用整数）。reserve / settle 同用。
	BillingMultiplierBP int64
}

// Tier 按分辨率档查定价；不在售（未配价）返 (zero, false)。
func (p *Pricing) Tier(resolution string) (ResolutionTier, bool) {
	t, ok := p.tiers[resolution]
	return t, ok
}

// OfferedResolutions 返回在售分辨率档（规范顺序：低→高）。
// 每次返回新建切片，调用方修改不影响内部状态。
// 同时是 capability resolution 枚举的来源，保证「能选的档 = 有价的档」一致。
func (p *Pricing) OfferedResolutions() []string {
	out := make([]string, 0, len(p.tiers))
	for _, r := range orderedResolutions {
		if _, ok := p.tiers[r]; ok {
			out = append(out, r)
		}
	}
	return out
}

// VideoModelEntry 是视频字典单条记录（计划 §Unit 4：含 pricing + capability + channel 绑定）。
//
// 不可变：catalog 构造后只读，可安全并发读。
type VideoModelEntry struct {
	// GatewayModelName 业务可见 model 名（如 "gw-video"）。
	GatewayModelName string
	// UpstreamProviderType provider 协议簇；MVP 唯一 ProviderTypeVolcSeedance。
	UpstreamProviderType string
	// UpstreamBaseURL 上游 base URL（不含 endpoint path）。
	// 例：火山 ARK = "https://ark.cn-beijing.volces.com/api/v3"。
	UpstreamBaseURL string
	// UpstreamModelName 上游真实 model 名（如 "doubao-seedance-2-0-..."）。
	// Unit 5 adapter 组装上游 submit body 时取此字段（adapter 同时持有 entry + ValidatedRequest）。
	UpstreamModelName string
	// ChannelName 绑定的 channel 名（凭据来源；Unit 3 channel service 据此取 5 段凭据）。
	// **catalog 不持有凭据**（凭据加密在 channel 表）；此处仅是绑定标识，
	// Unit 6 task service 提交时按名查 channel 解密注入上游。
	ChannelName string

	// Capability 能力描述符（驱动请求校验）。
	Capability *Capability
	// Pricing 分辨率档定价表。
	Pricing *Pricing
}

// VideoCatalog 视频模型字典接口（计划 §Unit 4）。
//
// MVP 仅 EnvVideoCatalog 单条实现；P1+ 升级为 DB / 文件字典时实现同接口（OCP）。
// 与同步 relay 的 Catalog 接口形态一致（Lookup / DefaultEntry / All），便于心智迁移。
type VideoCatalog interface {
	// Lookup 按业务可见 model 名查 entry；不命中返 (nil, false)。
	// MVP 单条实现忽略入参永远命中 DefaultEntry（业务传任何 model 都路由到唯一条，
	// 与同步 relay 的 Catalog.Lookup 一致）。P1+ 多条字典升级时实现真实按名查找。
	Lookup(gatewayModelName string) (*VideoModelEntry, bool)
	// DefaultEntry 返字典首条。
	DefaultEntry() *VideoModelEntry
	// All 列出所有字典条目（运维 / whoami 用）。
	All() []*VideoModelEntry
}

// EnvVideoCatalog 从 env 加载的单条视频字典实现（计划 §Unit 4：env 单 model 多分辨率档）。
//
// 不可变：构造后所有字段只读，可安全并发 Lookup / DefaultEntry / All。
// 构造逻辑见 catalog_build.go（NewEnvVideoCatalog + fail-fast 校验链）。
type EnvVideoCatalog struct {
	entry *VideoModelEntry
}

// 编译期断言实现接口。
var _ VideoCatalog = (*EnvVideoCatalog)(nil)

// Lookup MVP 单条字典：忽略入参永远命中（业务传任何 model 都路由唯一条）。
func (c *EnvVideoCatalog) Lookup(_ string) (*VideoModelEntry, bool) {
	return c.entry, true
}

// DefaultEntry 返字典首条。
func (c *EnvVideoCatalog) DefaultEntry() *VideoModelEntry {
	return c.entry
}

// All 返字典全条目（MVP 单条）。
func (c *EnvVideoCatalog) All() []*VideoModelEntry {
	return []*VideoModelEntry{c.entry}
}

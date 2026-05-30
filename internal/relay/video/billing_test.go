package video

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testPricing 三档定价（与 catalog_test 基线同价；720p/480p=4600，1080p=5100，倍率 1.1×）。
// 直接构造 *Pricing（同包可访问 unexported tiers），便于按档精确断言。
func testPricing() *Pricing {
	return &Pricing{
		tiers: map[string]ResolutionTier{
			Resolution480p:  {Resolution: Resolution480p, LongSidePx: 854, PricePerMillionTokensMinor: 4600},
			Resolution720p:  {Resolution: Resolution720p, LongSidePx: 1280, PricePerMillionTokensMinor: 4600},
			Resolution1080p: {Resolution: Resolution1080p, LongSidePx: 1920, PricePerMillionTokensMinor: 5100},
		},
		BillingMultiplierBP: 11000,
	}
}

func req(duration int, resolution string, fps int) *ValidatedRequest {
	return &ValidatedRequest{
		TaskType:   TaskTypeTextToVideo,
		Prompt:     "p",
		Duration:   duration,
		Resolution: resolution,
		Fps:        fps,
	}
}

// TestBilledMinorCeil 直测 token→minor 的权威换算（钱必须精确；双段整数 ceil）。
//
// **与 internal/task/snapshot.go SettleMinor 末段对拍的锚点**：snapshot_test 的同输入须得同值。
func TestBilledMinorCeil(t *testing.T) {
	cases := []struct {
		name              string
		tokens, price, bp int64
		want              int64
	}{
		// base=ceil(100000×6000/1e6)=600；×1.0
		{"happy_1x", 100_000, 6000, 10_000, 600},
		// 600×1.1=660
		{"multiplier_1.1x", 100_000, 6000, 11_000, 660},
		// base=ceil(230400×4600/1e6)=ceil(1059.84)=1060；×1.1=1166
		{"reserve_720p_default", 230_400, 4600, 11_000, 1166},
		// base=ceil(1×1/1e6)=1（ceil 把不足 1 的非零向上取 1）；×1.0=1
		{"ceil_floor_to_one", 1, 1, 10_000, 1},
		// tokens=0 → 0
		{"zero_tokens", 0, 4600, 11_000, 0},
		// 整除边界：base=ceil(1e6×6000/1e6)=6000；×1.0=6000
		{"exact_division", 1_000_000, 6000, 10_000, 6000},
		// tokens×price 溢出 int64 → 饱和到 MaxInt64（fail-closed，绝不静默 wrap 成偏低值）。
		{"overflow_saturates", math.MaxInt64, 1000, 10_000, math.MaxInt64},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, BilledMinorCeil(tc.tokens, tc.price, tc.bp))
		})
	}
}

// TestBilledMinorCeil_Monotone 钉死 BilledMinorCeil 对 token 单调非减——这是「可证 settle ≤ reserve」
// 论证的核心性质（settle=f(usage) ≤ f(reserveTokens)=reserve 当 usage ≤ reserveTokens）。
func TestBilledMinorCeil_Monotone(t *testing.T) {
	const price, bp = 4600, 11000
	var prev int64 = -1
	for tokens := int64(0); tokens <= 2_000_000; tokens += 7919 { // 步长取质数遍历非对齐边界
		got := BilledMinorCeil(tokens, price, bp)
		assert.GreaterOrEqualf(t, got, prev, "BilledMinorCeil 须单调非减（tokens=%d）", tokens)
		prev = got
	}
}

// TestEstimateReserveMinor_Happy 精确断言 720p/5s/24fps 的 reserveTokens / reserveMinor。
//
// 手算：framePixels=1280²=1,638,400；frames=5×24=120；totalPixels=196,608,000；
// baseTokens=196,608,000/1024=192,000（整除）。
func TestEstimateReserveMinor_Happy(t *testing.T) {
	p := testPricing()

	// 默认安全系数 1.2×：safeTokens=ceil(192000×12000/1e4)=230,400；reserveMinor=BilledMinorCeil(230400,4600,11000)=1166。
	rt, rm, err := EstimateReserveMinor(req(5, Resolution720p, 24), p, ReserveOptions{})
	require.NoError(t, err)
	assert.Equal(t, int64(230_400), rt, "reserveTokens（含 1.2× 安全系数）")
	assert.Equal(t, int64(1166), rm, "reserveMinor")
	assert.Equal(t, BilledMinorCeil(rt, 4600, 11000), rm, "reserveMinor 须由 BilledMinorCeil(reserveTokens) 导出")

	// 安全系数 1.0×：reserveTokens=192,000；reserveMinor=BilledMinorCeil(192000,4600,11000)=973。
	rt1, rm1, err := EstimateReserveMinor(req(5, Resolution720p, 24), p, ReserveOptions{SafetyFactorBP: 10_000})
	require.NoError(t, err)
	assert.Equal(t, int64(192_000), rt1)
	assert.Equal(t, int64(973), rm1)
	assert.Less(t, rt1, rt, "1.0× 估算应小于默认 1.2×（安全系数确放大上界）")
}

// TestEstimateReserveMinor_MinTokenFloor 极短时长命中最低 token 下限时，reserveTokens 抬到下限。
func TestEstimateReserveMinor_MinTokenFloor(t *testing.T) {
	p := testPricing()
	// 480p/4s/24fps 估算约 8.2 万 token；floor=500,000 显著更高 → reserveTokens=floor。
	rt, rm, err := EstimateReserveMinor(req(4, Resolution480p, 24), p, ReserveOptions{MinTokenFloor: 500_000})
	require.NoError(t, err)
	assert.Equal(t, int64(500_000), rt, "reserveTokens 抬到 MinTokenFloor")
	assert.Equal(t, BilledMinorCeil(500_000, 4600, 11000), rm)

	// floor 低于估算 → 不影响（取估算值）。
	rtLow, _, err := EstimateReserveMinor(req(4, Resolution480p, 24), p, ReserveOptions{MinTokenFloor: 1})
	require.NoError(t, err)
	assert.Greater(t, rtLow, int64(1), "floor 过低时取估算值，不被 floor 限制")

	// 负 floor 归一化为 0（无下限）：结果须与 floor=0 完全一致。
	rtZero, rmZero, err := EstimateReserveMinor(req(4, Resolution480p, 24), p, ReserveOptions{MinTokenFloor: 0})
	require.NoError(t, err)
	rtNeg, rmNeg, err := EstimateReserveMinor(req(4, Resolution480p, 24), p, ReserveOptions{MinTokenFloor: -100})
	require.NoError(t, err)
	assert.Equal(t, rtZero, rtNeg, "负 floor 须归一化为 0（reserveTokens 与 floor=0 同）")
	assert.Equal(t, rmZero, rmNeg, "负 floor 须归一化为 0（reserveMinor 与 floor=0 同）")
}

// TestEstimateReserveMinor_TierMonotone 同 duration/fps 下，分辨率越高 reserveTokens/Minor 越大
// （W×H 上界单调）；reserveMinor 恒等于 BilledMinorCeil(reserveTokens, 档单价, 倍率)。
func TestEstimateReserveMinor_TierMonotone(t *testing.T) {
	p := testPricing()
	var prevTokens int64
	for _, res := range []string{Resolution480p, Resolution720p, Resolution1080p} {
		rt, rm, err := EstimateReserveMinor(req(8, res, 30), p, ReserveOptions{})
		require.NoError(t, err)
		assert.Greater(t, rt, prevTokens, "分辨率 %s 的 reserveTokens 应高于更低档", res)
		tier, _ := p.Tier(res)
		assert.Equal(t, BilledMinorCeil(rt, tier.PricePerMillionTokensMinor, p.BillingMultiplierBP), rm,
			"档 %s 的 reserveMinor 须由该档单价导出", res)
		prevTokens = rt
	}
}

// TestEstimateReserveMinor_DurationFpsMonotone duration / fps 越大 reserveTokens 越大（单调非减）。
func TestEstimateReserveMinor_DurationFpsMonotone(t *testing.T) {
	p := testPricing()
	base, _, err := EstimateReserveMinor(req(5, Resolution720p, 24), p, ReserveOptions{})
	require.NoError(t, err)
	longer, _, err := EstimateReserveMinor(req(10, Resolution720p, 24), p, ReserveOptions{})
	require.NoError(t, err)
	faster, _, err := EstimateReserveMinor(req(5, Resolution720p, 30), p, ReserveOptions{})
	require.NoError(t, err)
	assert.Greater(t, longer, base, "时长翻倍 reserveTokens 增大")
	assert.Greater(t, faster, base, "帧率提高 reserveTokens 增大")
}

// TestEstimateReserveMinor_Errors fail-closed 错误路径。
func TestEstimateReserveMinor_Errors(t *testing.T) {
	p := testPricing()

	t.Run("nil_req", func(t *testing.T) {
		_, _, err := EstimateReserveMinor(nil, p, ReserveOptions{})
		require.Error(t, err)
	})
	t.Run("nil_pricing", func(t *testing.T) {
		_, _, err := EstimateReserveMinor(req(5, Resolution720p, 24), nil, ReserveOptions{})
		require.Error(t, err)
	})
	t.Run("unknown_resolution", func(t *testing.T) {
		_, _, err := EstimateReserveMinor(req(5, "4320p", 24), p, ReserveOptions{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "无定价")
	})
	t.Run("negative_duration", func(t *testing.T) {
		_, _, err := EstimateReserveMinor(req(-1, Resolution720p, 24), p, ReserveOptions{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "不能为负")
	})
	t.Run("negative_fps", func(t *testing.T) {
		// duration>=0 但 fps<0 → 走同一 (duration<0 || fps<0) 分支（之前仅覆盖 duration 侧）。
		_, _, err := EstimateReserveMinor(req(5, Resolution720p, -1), p, ReserveOptions{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "不能为负")
	})
	t.Run("overflow_duration_fps", func(t *testing.T) {
		// duration×fps 溢出 int64 → checkedMul 拦截（运维误配 / 非法巨值兜底）。
		_, _, err := EstimateReserveMinor(req(1<<33, Resolution720p, 1<<33), p, ReserveOptions{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "溢出")
	})
}

// TestEstimateReserveMinor_ProvableUpperBound 包内自证：reserveMinor = BilledMinorCeil(reserveTokens)，
// 且对任意 usage ≤ reserveTokens，BilledMinorCeil(usage) ≤ reserveMinor（单调非减 → 不撞 cap）。
//
// 跨包（settle 侧 SettleMinor）的端到端不变量在 internal/task 包另测（task→video 依赖方向）。
func TestEstimateReserveMinor_ProvableUpperBound(t *testing.T) {
	p := testPricing()
	for _, res := range []string{Resolution480p, Resolution720p, Resolution1080p} {
		for _, dur := range []int{4, 7, 15} {
			for _, fps := range []int{24, 30} {
				rt, rm, err := EstimateReserveMinor(req(dur, res, fps), p, ReserveOptions{})
				require.NoError(t, err)
				tier, _ := p.Tier(res)
				for _, usage := range []int64{0, 1, rt / 2, rt - 1, rt} {
					if usage < 0 {
						continue
					}
					got := BilledMinorCeil(usage, tier.PricePerMillionTokensMinor, p.BillingMultiplierBP)
					assert.LessOrEqualf(t, got, rm,
						"res=%s dur=%d fps=%d usage=%d：settle 换算须 ≤ reserveMinor", res, dur, fps, usage)
				}
			}
		}
	}
}

func TestCeilDivInt64(t *testing.T) {
	cases := []struct{ a, b, want int64 }{
		{0, 1024, 0},
		{1, 1024, 1},
		{1024, 1024, 1},
		{1025, 1024, 2},
		{-5, 1024, 0},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, ceilDivInt64(tc.a, tc.b))
	}
}

func TestCheckedMul(t *testing.T) {
	p, ok := checkedMul(3, 4)
	assert.True(t, ok)
	assert.Equal(t, int64(12), p)

	_, ok = checkedMul(0, math.MaxInt64)
	assert.True(t, ok, "0 因子不溢出")

	_, ok = checkedMul(math.MaxInt64, 2)
	assert.False(t, ok, "溢出须被检出")
}

package task

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sunxin-git/api-gateway/internal/relay/video"
)

// 本测试钉死 Unit 7 的核心账务不变量：reserve 侧 video.EstimateReserveMinor 与 settle 侧
// snapshot.SettleMinor **跨包同口径**，保证「可证 settle ≤ reserve」端到端成立。
//
// 为什么放在 task 包：依赖方向 task→video（video 不能 import task），故唯一能同时引用
// EstimateReserveMinor 与 SettleMinor 的位置在 task 包。两实现各自持有换算（video.BilledMinorCeil
// 与 snapshot.go 内联段），本测试是它们不漂移的回归闸（CLAUDE.md：涉钱必附边界 + 不变量测试）。

func invariantTestPricing(t *testing.T) *video.Pricing {
	t.Helper()
	cat, err := video.NewEnvVideoCatalog(video.CatalogConfig{
		GatewayModelName:       "gw-video",
		UpstreamProviderType:   video.ProviderTypeVolcSeedance,
		UpstreamBaseURL:        "https://ark.cn-beijing.volces.com/api/v3",
		UpstreamModelName:      "doubao-seedance-2-0-t2v",
		ChannelName:            "seedance-main",
		Price480pPer1MMinor:    4600,
		Price720pPer1MMinor:    4600,
		Price1080pPer1MMinor:   5100,
		BillingMultiplierBP:    11000,
		DurationMinSeconds:     4,
		DurationMaxSeconds:     15,
		DurationDefaultSeconds: 5,
		FpsDefault:             24,
		FpsMax:                 30,
		Ratios:                 []string{"16:9", "9:16", "1:1", "adaptive"},
		RatioDefault:           "16:9",
		ResolutionDefault:      "720p",
	})
	require.NoError(t, err)
	return cat.DefaultEntry().Pricing
}

// snapshotFromReserve 模拟 service.Submit 组装财务快照的相关字段（仅计费相关）。
func snapshotFromReserve(p *video.Pricing, resolution string, reserveTokens, reserveMinor, floor int64) TaskFinancialSnapshot {
	tier, _ := p.Tier(resolution)
	return TaskFinancialSnapshot{
		Resolution:                 resolution,
		ReserveMinor:               reserveMinor,
		ReserveTokens:              reserveTokens,
		PricePerMillionTokensMinor: tier.PricePerMillionTokensMinor,
		BillingMultiplierBP:        p.BillingMultiplierBP,
		MinTokenFloor:              floor,
	}
}

// TestReserveSettleInvariant_NoCapWithinBound 对任意 usage ≤ reserveTokens：
//   - SettleMinor 不触顶（capped=false）——证明 reserveMinor 是真实上界；
//   - SettleMinor 的金额逐字等于 video.BilledMinorCeil(max(usage,floor),…)——证明两侧换算不漂移。
func TestReserveSettleInvariant_NoCapWithinBound(t *testing.T) {
	p := invariantTestPricing(t)
	for _, res := range []string{video.Resolution480p, video.Resolution720p, video.Resolution1080p} {
		for _, dur := range []int{4, 5, 10, 15} {
			for _, fps := range []int{24, 30} {
				for _, floor := range []int64{0, 50_000, 300_000} {
					rt, rm, err := video.EstimateReserveMinor(
						&video.ValidatedRequest{Resolution: res, Duration: dur, Fps: fps},
						p, video.ReserveOptions{MinTokenFloor: floor},
					)
					require.NoError(t, err)
					require.Positive(t, rm)
					snap := snapshotFromReserve(p, res, rt, rm, floor)
					tier, _ := p.Tier(res)

					// usage 取样含 0、floor 边界（floor-1/floor/floor+1）、reserveTokens 边界——
					// 整数 ceil + floor clamp 的 bug 易藏在这些边界（adversarial/testing ce-review）。
					usages := []int64{0, 1, rt / 3, rt - 1, rt}
					for _, u := range []int64{floor - 1, floor, floor + 1} {
						usages = append(usages, u)
					}
					for _, usage := range usages {
						if usage < 0 || usage > rt {
							continue // 不变量断言仅对 usage ≤ reserveTokens 成立（超界走 cap，另测）
						}
						minor, capped := snap.SettleMinor(usage)
						billed := usage
						if billed < floor {
							billed = floor
						}
						want := video.BilledMinorCeil(billed, tier.PricePerMillionTokensMinor, p.BillingMultiplierBP)
						assert.Falsef(t, capped, "res=%s dur=%d fps=%d floor=%d usage=%d：usage≤reserveTokens 不应触顶",
							res, dur, fps, floor, usage)
						assert.Equalf(t, want, minor, "res=%s dur=%d fps=%d floor=%d usage=%d：settle 与 BilledMinorCeil 须逐字一致",
							res, dur, fps, floor, usage)
						assert.LessOrEqual(t, minor, rm, "settle ≤ reserve")
					}
				}
			}
		}
	}
}

// TestReserveSettleInvariant_CapAboveBound usage 超 reserveTokens（疑似 provider 计费异常）→
// SettleMinor 触顶 reserveMinor 且 capped=true（账本硬约束 commit ≤ reserve 的最后一道闸）。
func TestReserveSettleInvariant_CapAboveBound(t *testing.T) {
	p := invariantTestPricing(t)
	rt, rm, err := video.EstimateReserveMinor(
		&video.ValidatedRequest{Resolution: video.Resolution720p, Duration: 5, Fps: 24},
		p, video.ReserveOptions{},
	)
	require.NoError(t, err)
	snap := snapshotFromReserve(p, video.Resolution720p, rt, rm, 0)

	minor, capped := snap.SettleMinor(rt + 1_000_000)
	assert.True(t, capped, "usage 超 reserveTokens 应触顶")
	assert.Equal(t, rm, minor, "触顶值 = reserveMinor")
}

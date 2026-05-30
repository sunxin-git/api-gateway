package task

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestSettleMinor 直测结算金额公式（钱必须精确；之前仅经端到端间接覆盖 1.0×/floor=0）。
func TestSettleMinor(t *testing.T) {
	base := TaskFinancialSnapshot{
		ReserveMinor:               1000,
		ReserveTokens:              1_000_000_000,
		PricePerMillionTokensMinor: 6000, // 6000 minor / 百万 token
		BillingMultiplierBP:        bpScale,
		MinTokenFloor:              0,
	}
	cases := []struct {
		name       string
		mutate     func(*TaskFinancialSnapshot)
		usage      int64
		wantMinor  int64
		wantCapped bool
	}{
		{"happy_1x", nil, 100_000, 600, false}, // ceil(100000×6000/1e6)=600；×1.0
		{"multiplier_1.1x", func(s *TaskFinancialSnapshot) { s.BillingMultiplierBP = 11000 }, 100_000, 660, false},              // 600×1.1=660
		{"min_floor_applies", func(s *TaskFinancialSnapshot) { s.MinTokenFloor = 50_000 }, 10_000, 300, false},                  // billed=50000→300
		{"settle_exceeds_reserve_capped", nil, 200_000, 1000, true},                                                             // 1200 > 1000 → cap
		{"usage_over_reserve_tokens_capped", func(s *TaskFinancialSnapshot) { s.ReserveTokens = 100_000 }, 200_000, 1000, true}, // 防溢出 cap
		{"zero_usage_floor_zero", nil, 0, 0, false},                                                                             // billed=0 → 0（settleCompleted 另有 ≤0 守卫）
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := base
			if tc.mutate != nil {
				tc.mutate(&snap)
			}
			minor, capped := snap.SettleMinor(tc.usage)
			assert.Equal(t, tc.wantMinor, minor)
			assert.Equal(t, tc.wantCapped, capped)
			assert.LessOrEqual(t, minor, snap.ReserveMinor, "可证 settle ≤ reserve")
		})
	}
}

func TestCeilDiv(t *testing.T) {
	cases := []struct {
		a, b, want int64
	}{
		{0, 1_000_000, 0},
		{1, 1_000_000, 1},
		{1_000_000, 1_000_000, 1},
		{1_000_001, 1_000_000, 2},
		{-5, 1_000_000, 0}, // a≤0 防御
		{6_000_000_00, 1_000_000, 600},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, ceilDiv(tc.a, tc.b))
	}
}

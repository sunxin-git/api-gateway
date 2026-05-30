package video

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConcurrencyLimits_Cap(t *testing.T) {
	limits, err := NewConcurrencyLimits(5, map[ConcurrencyKey]int32{
		{AccountID: "acc-vip", Model: "gw-video"}: 20,
		{AccountID: "acc-ban", Model: "gw-video"}: 0, // 禁用该 (account, model)
	})
	require.NoError(t, err)

	assert.Equal(t, int32(5), limits.Cap("acc-normal", "gw-video"), "无覆写 → 默认值")
	assert.Equal(t, int32(20), limits.Cap("acc-vip", "gw-video"), "命中覆写 → 覆写值")
	assert.Equal(t, int32(0), limits.Cap("acc-ban", "gw-video"), "覆写为 0 → 禁用（claim 必占不到）")
	assert.Equal(t, int32(5), limits.Cap("acc-vip", "other-model"), "model 不同 → 不命中该覆写")
	assert.Equal(t, int32(5), limits.DefaultCap())
}

func TestConcurrencyLimits_NilOverrides(t *testing.T) {
	limits, err := NewConcurrencyLimits(3, nil)
	require.NoError(t, err)
	assert.Equal(t, int32(3), limits.Cap("anyone", "any-model"))
}

func TestConcurrencyLimits_Validation(t *testing.T) {
	t.Run("negative_default", func(t *testing.T) {
		_, err := NewConcurrencyLimits(-1, nil)
		require.Error(t, err)
	})
	t.Run("negative_override", func(t *testing.T) {
		_, err := NewConcurrencyLimits(5, map[ConcurrencyKey]int32{
			{AccountID: "a", Model: "m"}: -1,
		})
		require.Error(t, err)
	})
	t.Run("zero_default_ok", func(t *testing.T) {
		// cap=0 默认是合法值（全局禁用）；上界由 config 校验（VideoRelayConcurrencyDefault≥1），非本类型。
		limits, err := NewConcurrencyLimits(0, nil)
		require.NoError(t, err)
		assert.Equal(t, int32(0), limits.Cap("a", "m"))
	})
}

// TestConcurrencyLimits_OverrideCopy 构造后改原 map 不影响已构造的 limits（深拷贝不可变）。
func TestConcurrencyLimits_OverrideCopy(t *testing.T) {
	src := map[ConcurrencyKey]int32{{AccountID: "a", Model: "m"}: 7}
	limits, err := NewConcurrencyLimits(5, src)
	require.NoError(t, err)
	src[ConcurrencyKey{AccountID: "a", Model: "m"}] = 99 // 改原 map
	assert.Equal(t, int32(7), limits.Cap("a", "m"), "limits 持深拷贝，不受原 map 后续修改影响")
}

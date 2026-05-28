package relay

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestEstimateInputTokens_Empty(t *testing.T) {
	assert.Equal(t, 0, estimateInputTokens(nil))
	assert.Equal(t, 0, estimateInputTokens([]any{}))
}

func TestEstimateInputTokens_ShortEnglish(t *testing.T) {
	// 1 个 message "hello"，JSON 序列化后约 41 字节 / 4 ≈ 10
	messages := []any{
		map[string]any{"role": "user", "content": "hello"},
	}
	got := estimateInputTokens(messages)
	assert.Greater(t, got, 0)
	assert.Less(t, got, 50, "短英文 messages 估算应 < 50 token")
}

func TestEstimateInputTokens_LongContent(t *testing.T) {
	// 长内容
	longText := strings.Repeat("hello world ", 100) // 1200 字节
	messages := []any{
		map[string]any{"role": "user", "content": longText},
	}
	got := estimateInputTokens(messages)
	assert.Greater(t, got, 250, "长 messages 估算应 ≥ 长度/4")
}

func TestEstimateInputTokens_MixedChineseEnglish(t *testing.T) {
	// 中文 UTF-8 每字符约 3 字节，比英文 1 字节多 → JSON 长度大 → 估算 token 数更多
	chinese := strings.Repeat("你好世界这是中文测试", 50)
	messages := []any{
		map[string]any{"role": "user", "content": chinese},
	}
	got := estimateInputTokens(messages)
	assert.Greater(t, got, 100, "中文 messages 估算应保守上界 → 数值偏高")
}

func TestEstimateInputTokens_MultipleMessages(t *testing.T) {
	messages := []any{
		map[string]any{"role": "system", "content": "you are helpful"},
		map[string]any{"role": "user", "content": "question 1"},
		map[string]any{"role": "assistant", "content": "answer 1"},
		map[string]any{"role": "user", "content": "question 2"},
	}
	got := estimateInputTokens(messages)
	assert.Greater(t, got, 20)
}

func TestEstimateInputTokens_NonSerializable(t *testing.T) {
	// 包含 chan / func 等不可 marshal 字段 → defensive 返 0
	messages := []any{
		map[string]any{"role": "user", "content": make(chan int)},
	}
	got := estimateInputTokens(messages)
	assert.Equal(t, 0, got, "marshal 失败时返 0 让上层兜底")
}

func TestComputeReserveMinor(t *testing.T) {
	// input 100 tokens × 800 ¥/M + output 200 tokens × 2000 ¥/M
	// = 100*800 + 200*2000 = 80000 + 400000 = 480000 → ceil(480000 / 1M) = 1
	got := computeReserveMinor(100, 200, 800, 2000)
	assert.Equal(t, int64(1), got)
}

func TestComputeReserveMinor_LargeNumbers(t *testing.T) {
	// 1000 input × 800 + 5000 output × 2000 = 800k + 10M = 10.8M → ceil(10.8M / 1M) = 11
	got := computeReserveMinor(1000, 5000, 800, 2000)
	assert.Equal(t, int64(11), got)
}

func TestComputeReserveMinor_NegativeInputClamped(t *testing.T) {
	// 防御：负数 token 被 clamp 到 0
	got := computeReserveMinor(-100, 200, 800, 2000)
	expected := int64(0*800+200*2000) / 1_000_000
	if int64(200*2000)%1_000_000 != 0 {
		expected++
	}
	assert.Equal(t, expected, got)
}

func TestComputeCostMinor_SameFormula(t *testing.T) {
	// settle 阶段公式与 reserve 一致（input/output 用 usage 实际值）
	got := computeCostMinor(50, 100, 800, 2000)
	// 50*800 + 100*2000 = 40k + 200k = 240k → ceil(240k/1M) = 1
	assert.Equal(t, int64(1), got)
}

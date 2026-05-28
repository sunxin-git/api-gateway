package relay

import (
	"encoding/json"
)

// estimateInputTokens 简化估算业务请求的 input token 数（plan §决策 D1）。
//
// 算法：`len(json.Marshal(messages)) / 4`（保守上界）
//
// 推理：
//   - OpenAI BPE 英文 ≈ 4 字符 / token；中文每字符 ≈ 1 token
//   - 除 4 对中文请求保守（实际 estimate 远小于真实），但 reserve 是上界：
//     真实 input × in_price 永远 ≤ estimate × in_price
//   - settle 阶段用上游真实 usage 退多余 reserve；overestimate 由 ledger.Commit 自动 release
//
// 不引 tiktoken / 上游 tokenizer 的原因（plan §Scope Boundaries）：
//   - 50MB 数据文件 + 豆包 ByteDance BPE 与 OpenAI cl100k 不同源，引入也不准
//   - MVP 接受 ~5% 估算误差；settle 真实 usage 退回精确金额
//
// 输入：业务 request body 的 messages 字段值（[]any，OpenAI 协议）
// 输出：估算的 input tokens（保守上界）；marshal 失败时返 0 让上层兜底处理
func estimateInputTokens(messages []any) int {
	if len(messages) == 0 {
		return 0
	}
	b, err := json.Marshal(messages)
	if err != nil {
		// marshal 不应失败（input 来自 gin bind 的合法 JSON），防御性返 0
		return 0
	}
	return len(b) / 4
}

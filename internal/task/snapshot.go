package task

import (
	"encoding/json"
	"fmt"

	"github.com/sunxin-git/api-gateway/internal/relay/video"
)

// TaskFinancialSnapshot 是任务的财务 + 请求快照，序列化进 task.financial_snapshot（jsonb）。
//
// 三类内容（plan §Unit 6 snapshot.go：授权快照 + 价格快照 + reserve ledger ref）：
//  1. 路由/审计：gateway/upstream model、provider、channel（提交时定，settle/审计读）。
//  2. 请求参数：task_type + prompt + duration/resolution/ratio/fps——供 submit worker 调
//     adapter.Submit、及 reconciler（6b）从 task 行**重建**丢失的 submit job（无需额外列）。
//  3. 价格快照：ReservationCorrelationID（钉死 reserve↔commit/release 同 correlation）+ 冻结
//     单价/倍率/最低 token 下限 + reserve minor/tokens（settle 用冻结价，调价不影响 inflight）。
//
// 不可变：提交时写入后只读（settle / 审计读，绝不改）。
type TaskFinancialSnapshot struct {
	// --- 路由 / 审计 ---
	GatewayModel  string `json:"gateway_model"`
	UpstreamModel string `json:"upstream_model"`
	ProviderType  string `json:"provider_type"`
	ChannelName   string `json:"channel_name"`

	// --- 请求参数（重建 submit / adapter 调用）---
	TaskType   string `json:"task_type"`
	Prompt     string `json:"prompt"`
	Duration   int    `json:"duration"`
	Resolution string `json:"resolution"`
	Ratio      string `json:"ratio"`
	Fps        int    `json:"fps"`

	// --- 价格快照 ---
	// ReservationCorrelationID 钉死 reserve 与 commit/release 用同一 correlation（plan §Unit 7）。
	// 取 task_id（唯一稳定，且不以 ":release" 结尾，不与账本内部 release 后缀冲突）。
	ReservationCorrelationID string `json:"reservation_correlation_id"`
	// ReserveMinor 提交时预占金额（minor）；settle 的 commit/release 以此为上界/释放额。
	ReserveMinor int64 `json:"reserve_minor"`
	// ReserveTokens 提交时估的 token 上界（settle 防溢出 + 反查口径）。
	ReserveTokens int64 `json:"reserve_tokens"`
	// PricePerMillionTokensMinor 冻结单价（CNY 分 / 百万 token）；settle 用此价，调价不影响 inflight。
	PricePerMillionTokensMinor int64 `json:"price_per_million_tokens_minor"`
	// BillingMultiplierBP 冻结商业加价倍率基点（10000=1.0×）。
	BillingMultiplierBP int64 `json:"billing_multiplier_bp"`
	// MinTokenFloor 最低 token 计费下限（seedance 2.0，ADR-0006）；settle/reserve 都须覆盖。
	MinTokenFloor int64 `json:"min_token_floor"`
}

const (
	perMillionTokens = 1_000_000 // 单价口径：minor / 百万 token
	bpScale          = 10_000    // 倍率口径：basis points（10000 = 1.0×）
)

// Marshal 序列化为 financial_snapshot jsonb 字节。
func (s TaskFinancialSnapshot) Marshal() ([]byte, error) {
	return json.Marshal(s)
}

// ParseSnapshot 从 financial_snapshot jsonb 字节反序列化。
func ParseSnapshot(raw []byte) (TaskFinancialSnapshot, error) {
	var s TaskFinancialSnapshot
	if err := json.Unmarshal(raw, &s); err != nil {
		return TaskFinancialSnapshot{}, fmt.Errorf("task: 解析 financial_snapshot 失败: %w", err)
	}
	return s, nil
}

// ToValidatedRequest 从快照重建 adapter 所需的 ValidatedRequest（submit worker / reconciler 用）。
func (s TaskFinancialSnapshot) ToValidatedRequest() *video.ValidatedRequest {
	return &video.ValidatedRequest{
		TaskType:   video.TaskType(s.TaskType),
		Prompt:     s.Prompt,
		Duration:   s.Duration,
		Resolution: s.Resolution,
		Ratio:      s.Ratio,
		Fps:        s.Fps,
	}
}

// SettleMinor 按上游真实 usage token 算结算金额（minor），返回 (settleMinor, capped)。
//
// 公式（与 reserve 估算同口径，仅 token 来源不同——reserve 用上界，settle 用真实）：
//
//	billed   = max(usageTokens, MinTokenFloor)                       # 覆盖上游最低计费下限
//	base     = ceil(billed × 单价 / 1e6)
//	settle   = ceil(base × 倍率BP / 1e4)                              # 整数基点，不浮点算钱
//	settle  := min(settle, ReserveMinor)                             # 账本硬约束：commit ≤ reserve
//
// capped=true 表示 settle 触顶到 ReserveMinor（疑似 provider 计费异常，调用方告警）。
// **可证 settle ≤ reserve**：reserve 用同公式 + ReserveTokens（≥ usage 上界）算；公式对 token
// 单调非减，故 f(usage) ≤ f(ReserveTokens) = reserve；末尾 cap 再兜底。
//
// 注：Unit 7 拥有 reserve 估算的权威公式（internal/relay/video/billing.go）；本方法是 settle 侧
// 对偶实现，置于 snapshot 让 settle worker 自洽（读冻结价即可，无需额外注入 billing）。
func (s TaskFinancialSnapshot) SettleMinor(usageTokens int64) (settleMinor int64, capped bool) {
	billed := usageTokens
	if billed < s.MinTokenFloor {
		billed = s.MinTokenFloor
	}
	if billed < 0 {
		billed = 0
	}
	// usage 超 reserve 上界（异常/provider bug）→ 直接 cap 到 reserve：既对齐账本硬约束，
	// 又避免 billed×单价 在 int64 溢出（billed 已被 ReserveTokens 上界约束在安全量级）。
	if s.ReserveTokens > 0 && billed > s.ReserveTokens {
		return s.ReserveMinor, true
	}
	base := ceilDiv(billed*s.PricePerMillionTokensMinor, perMillionTokens)
	settle := ceilDiv(base*s.BillingMultiplierBP, bpScale)
	if settle > s.ReserveMinor {
		return s.ReserveMinor, true
	}
	return settle, false
}

// ceilDiv 正整数向上取整除法 ceil(a/b)；要求 b > 0、a ≥ 0。
func ceilDiv(a, b int64) int64 {
	if a <= 0 {
		return 0
	}
	return (a + b - 1) / b
}

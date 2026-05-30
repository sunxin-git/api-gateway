package video

import (
	"fmt"
	"math"
)

// 计费 token 单位实现（plan §Unit 7 / §计费 token 单位；ADR-0006 决策 5：usage.completion_tokens
// + Seedance 2.0 最低 token 计费下限）。
//
// 职责划分（务必对照 internal/task/snapshot.go）：
//   - 本文件（video 包）：**reserve 侧权威公式** EstimateReserveMinor + token→minor 的单一换算
//     真相源 BilledMinorCeil。
//   - snapshot.go（task 包）：**settle 侧对偶** SettleMinor，用冻结快照价对真实 usage 调同一
//     换算口径（floor/cap 包装）。
//
// 为什么换算逻辑不集中到一个包：task 包已 import video（snapshot.ToValidatedRequest 返回
// *video.ValidatedRequest），video 不能反向 import task（循环）。故 BilledMinorCeil 在 video 暴露，
// 是两侧公认的「权威换算」；SettleMinor 与之**逐字一致**，由 billing_test 对拍 + task 包跨包不变量
// 测试守卫不漂移（CLAUDE.md 原则 6：涉钱显式 + 稳定，宁可测试钉死也不在两包间引入隐式耦合）。

// 计费口径常量（与 internal/task/snapshot.go 的 perMillionTokens / bpScale 同值；各包自持，
// 一致性由测试守卫）。
const (
	// perMillionTokens 单价口径：minor / 百万 token。
	perMillionTokens = 1_000_000
	// bpScale 倍率口径：basis points（10000 = 1.0×）。
	bpScale = 10_000
	// framePixelTokenDivisor 帧像素 → token 换算除数：每帧 token = W×H / 1024
	//（参考实现 storyboard-assistant credit/pricing.py estimate 口径）。
	framePixelTokenDivisor = 1024
)

// DefaultReserveSafetyFactorBP reserve 估算默认安全系数基点（12000 = 1.2×）。
//
// 作用：在「W×H 上界（catalog 各档长边²）」之上再留余量，吸收两类不确定性——
//  1. 上游真实 usage.completion_tokens 与参考公式 `duration×W×H×fps/1024` 的常数 / 取整方向差异；
//  2. catalog.go resolutionLongSidePx 各档「长边」定义须 Unit 5 集成核对的 residual（含 adaptive 档）。
//
// 取 1.2× 偏保守（fail-closed，CLAUDE.md 原则 5）：宁可多锁余额、settle 时释放差额，也不可
// under-reserve 致 settle 触顶 ReserveMinor cap → 系统性少收。**落地须用真实 usage 校准**
// （ADR-0006「落地核对官方」；plan Open Questions「安全系数具体值 Deferred to Implementation」）。
// caller（Unit 10 handler）可经 ReserveOptions.SafetyFactorBP 覆写。
const DefaultReserveSafetyFactorBP = 12_000

// ReserveOptions 是 EstimateReserveMinor 的非定价入参（与 Pricing 正交）。
//
// **floor 一致性约束**：caller 须把同一 MinTokenFloor 既传本结构、又传 task.SubmitParams.MinTokenFloor
// （→ 冻结进 financial_snapshot，settle 侧 SettleMinor 复用）。reserve 与 settle 用同 floor 是
// 「可证 settle ≤ reserve」的前提之一（floor 抬高的是两侧的同一下界）。
type ReserveOptions struct {
	// SafetyFactorBP 安全系数基点（10000=1.0×）；<=0 回落 DefaultReserveSafetyFactorBP。
	SafetyFactorBP int64
	// MinTokenFloor 最低 token 计费下限（seedance 2.0，ADR-0006）；<0 视为 0（无下限）。
	MinTokenFloor int64
}

// EstimateReserveMinor 是 reserve 预扣的**权威估算公式**（Unit 7 owns）。
//
// 整数运算（绝不浮点算钱，CLAUDE.md：涉钱用整数）：
//
//	framePixels   = pricing.Tier(resolution).MaxFramePixels()        // 该档 W×H 可证上界（长边²）
//	baseTokens    = ceil(duration × framePixels × fps / 1024)        // 参考实现帧像素→token 口径
//	safeTokens    = ceil(baseTokens × SafetyFactorBP / 10000)        // 加安全余量
//	reserveTokens = max(safeTokens, MinTokenFloor)                   // 覆盖上游最低计费下限
//	reserveMinor  = BilledMinorCeil(reserveTokens, 档单价, 倍率)      // 与 settle 同一换算函数
//
// **可证 settle ≤ reserve**（不撞 ledger ErrCommitExceedsReserved）：settle 侧 SettleMinor 对真实
// usage 用同口径——billed = max(usage, floor)。因 reserveTokens 取 W×H 长边² + 安全系数，是 usage 的
// 上界，且 reserveTokens ≥ floor，故 billed ≤ reserveTokens；BilledMinorCeil 对 token 单调非减 →
// settle = BilledMinorCeil(billed,…) ≤ BilledMinorCeil(reserveTokens,…) = reserveMinor。
//
// 返回 error（fail-closed）：req/pricing 为 nil；duration/fps 为负；分辨率无定价（catalog 不一致，
// validate 本应已挡）；估算乘积溢出 int64（运维误配兜底，catalog 已有 duration/fps 硬顶，此处纯函数级再防）。
//
// 注：本函数**不**保证 reserveMinor > 0（退化输入如 duration=0 可得 0）；正常路径 validate 已保证
// duration ≥ DurationMin ≥ 1，且 task.Submit 有 ReserveMinor>0 硬校验兜底。
func EstimateReserveMinor(req *ValidatedRequest, pricing *Pricing, opts ReserveOptions) (reserveTokens, reserveMinor int64, err error) {
	if req == nil || pricing == nil {
		return 0, 0, fmt.Errorf("video.EstimateReserveMinor: req / pricing 不能为 nil")
	}
	tier, ok := pricing.Tier(req.Resolution)
	if !ok {
		return 0, 0, fmt.Errorf(
			"video.EstimateReserveMinor: 分辨率 %q 无定价（catalog 不一致，validate 应已挡）", req.Resolution)
	}

	duration := int64(req.Duration)
	fps := int64(req.Fps)
	if duration < 0 || fps < 0 {
		return 0, 0, fmt.Errorf("video.EstimateReserveMinor: duration/fps 不能为负（duration=%d fps=%d）", duration, fps)
	}

	safetyBP := opts.SafetyFactorBP
	if safetyBP <= 0 {
		safetyBP = DefaultReserveSafetyFactorBP
	}
	floor := opts.MinTokenFloor
	if floor < 0 {
		floor = 0
	}

	framePixels := tier.MaxFramePixels() // 长边²，> 0

	// baseTokens = ceil(duration × framePixels × fps / 1024)，逐步 checkedMul 防溢出。
	frames, ok := checkedMul(duration, fps)
	if !ok {
		return 0, 0, fmt.Errorf("video.EstimateReserveMinor: duration×fps 溢出（duration=%d fps=%d）", duration, fps)
	}
	totalPixels, ok := checkedMul(frames, framePixels)
	if !ok {
		return 0, 0, fmt.Errorf("video.EstimateReserveMinor: duration×fps×framePixels 溢出（framePixels=%d）", framePixels)
	}
	baseTokens := ceilDivInt64(totalPixels, framePixelTokenDivisor)

	// safeTokens = ceil(baseTokens × safetyBP / 10000)
	scaled, ok := checkedMul(baseTokens, safetyBP)
	if !ok {
		return 0, 0, fmt.Errorf("video.EstimateReserveMinor: baseTokens×safetyBP 溢出（baseTokens=%d）", baseTokens)
	}
	reserveTokens = ceilDivInt64(scaled, bpScale)
	if reserveTokens < floor {
		reserveTokens = floor
	}

	// reserveMinor = BilledMinorCeil(reserveTokens, 单价, 倍率)，先 checkedMul 两段乘积防溢出。
	price := tier.PricePerMillionTokensMinor
	mult := pricing.BillingMultiplierBP
	priced, ok := checkedMul(reserveTokens, price)
	if !ok {
		return 0, 0, fmt.Errorf("video.EstimateReserveMinor: reserveTokens×单价 溢出（reserveTokens=%d 单价=%d）", reserveTokens, price)
	}
	base := ceilDivInt64(priced, perMillionTokens)
	if _, ok := checkedMul(base, mult); !ok {
		return 0, 0, fmt.Errorf("video.EstimateReserveMinor: base×倍率 溢出（base=%d 倍率BP=%d）", base, mult)
	}
	reserveMinor = BilledMinorCeil(reserveTokens, price, mult)
	return reserveTokens, reserveMinor, nil
}

// BilledMinorCeil 是 token → minor 的**权威换算公式**（reserve 与 settle 的单一真相源）。
//
//	base   = ceil(tokens × pricePerMillionMinor / 1_000_000)
//	billed = ceil(base × multiplierBP / 10_000)
//
// 整数双段 ceil（绝不浮点算钱）。**与 internal/task/snapshot.go SettleMinor 的末段算法在非溢出区
// 逐字一致**：settle 侧用冻结快照价对真实 usage 调本口径（外加 floor/cap），reserve 侧
// EstimateReserveMinor 对 token 上界调本函数——两侧同一公式使「可证 settle ≤ reserve」结构性成立
// （BilledMinorCeil 对 token 单调非减）。drift 由测试守卫（billing_test 对拍 + task 包 SettleMinor 等值断言）。
//
// **本函数导出**（供 task 包跨包不变量测试与 Unit 10 调用），故内部对两段乘积都做 checkedMul：
// 溢出 → **饱和到 math.MaxInt64**（fail-closed，CLAUDE.md 原则 5）。绝不静默 wrap 成偏低/负值金额；
// 饱和高值在唯一动钱处（SettleMinor 末段 min(settle, ReserveMinor) cap）被收敛回 reserve，故对账户
// 不会少收、不会击穿账本硬约束。真实输入永不触及溢出（catalog 已硬顶 duration/fps，价表量级有限）。
// tokens ≤ 0 时返回 0。
func BilledMinorCeil(tokens, pricePerMillionMinor, multiplierBP int64) int64 {
	priced, ok := checkedMul(tokens, pricePerMillionMinor)
	if !ok {
		return math.MaxInt64
	}
	base := ceilDivInt64(priced, perMillionTokens)
	scaled, ok := checkedMul(base, multiplierBP)
	if !ok {
		return math.MaxInt64
	}
	return ceilDivInt64(scaled, bpScale)
}

// ceilDivInt64 正整数向上取整除法 ceil(a/b)；a≤0 返 0，要求 b>0。
//
// 与 internal/task/snapshot.go 的 ceilDiv 同语义（两包各自持有，避免 task↔video 反向依赖）。
func ceilDivInt64(a, b int64) int64 {
	if a <= 0 {
		return 0
	}
	return (a + b - 1) / b
}

// checkedMul 返回 a×b 及「未溢出 int64」标志；仅供本包 reserve 估算的非负输入用。
//
// 实现：任一因子为 0 返 (0,true)；否则乘后用 p/b==a 反查（非负输入下可靠判溢出）。
func checkedMul(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	p := a * b
	if p/b != a {
		return 0, false
	}
	return p, true
}

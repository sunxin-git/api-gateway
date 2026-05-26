package ledger

import "errors"

// Sentinel errors —— 服务层契约（计划 R27 / Unit 3 §三）。
//
// 共 11 个：6 核心 CAS / 状态 + 5 语义化业务错误。
// 调用方使用 errors.Is 判断，**不**用类型断言；error 链可用 fmt.Errorf("...: %w", Err...) 包装。
//
// 被显式砍掉（不实现）：
//   - ErrInvariantViolation —— DB CHECK 触发即 panic，应用层无 caller 分支
//   - ErrInvalidActor       —— 用 fmt.Errorf("invalid actor: %w", err) 直接包装 Validate 返的 error
var (
	// === 核心 CAS / 状态错误 ===

	// ErrAccountNotFound 业务账户不存在（或处于非 active 状态如 suspended/deleted，
	// 由 D-min 后续细分）。
	ErrAccountNotFound = errors.New("business account not found")

	// ErrAccountFrozen 账户已被 freeze（drift 检测命中 / 手工冻结）；
	// Recharge/Reserve 拒绝；Commit/Release/Refund 允许继续完成 inflight。
	ErrAccountFrozen = errors.New("business account is frozen")

	// ErrInsufficientBalance available 余额不足；Reserve 失败。
	ErrInsufficientBalance = errors.New("insufficient available balance")

	// ErrInsufficientReserved reserved 余额不足；Commit/Release 失败（理论上调用方应已
	// 用 FindActiveReserveByCorrelation 校验过，这是兜底）。
	ErrInsufficientReserved = errors.New("insufficient reserved balance")

	// ErrInsufficientUsed used_total 不足；Refund 失败。
	ErrInsufficientUsed = errors.New("insufficient used_total")

	// ErrVersionConflict 乐观锁版本号冲突；调用方一般应重试。
	ErrVersionConflict = errors.New("balance version conflict, please retry")

	// === 语义化业务错误 ===

	// ErrInvalidAmount amount <= 0；入口校验阶段返回，不进 tx。
	ErrInvalidAmount = errors.New("amount must be positive")

	// ErrCommitExceedsReserved Commit 入口前置校验：actualCost > 原 reserve 金额。
	// 设计原则：不允许 Commit 跨账 available 补差额；超额必须由调用方先 Recharge 再 Reserve。
	ErrCommitExceedsReserved = errors.New("actual cost exceeds reserved amount")

	// ErrReserveNotFound Commit/Release 找不到同 correlation_id 的 active reserve entry。
	ErrReserveNotFound = errors.New("active reserve entry not found for correlation_id")

	// ErrAlreadySettled Commit/Release 找到的 reserve 已被 commit 或 release 过。
	ErrAlreadySettled = errors.New("reserve has already been settled (committed/released)")

	// ErrIdempotencyConflict 充值幂等命中但 canonical_body_sha256 不一致；
	// 通常意味着调用方 bug 或恶意覆盖；service 必须 critical log。
	ErrIdempotencyConflict = errors.New("idempotency_key reused with different body")

	// ErrRebuildContention RebuildBalance 在 TX2 读快照与 TX3 替换之间，
	// 多次（默认 3 次）被新 ledger entry 写入打断，重试耗尽；账户保持 frozen 等运营再触发。
	ErrRebuildContention = errors.New("rebuild balance: contention, last_ledger_id changed across retries")

	// === 临时占位（未实现的方法返回） ===

	// ErrNotImplemented 接口方法已声明但本 Unit 未实现（保留供未来扩展占位用）。
	// 当前 P0 阶段所有 Service 接口方法均已实装。
	ErrNotImplemented = errors.New("not implemented in this unit")
)

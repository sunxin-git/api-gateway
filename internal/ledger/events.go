package ledger

import "time"

// 事件治理（计划 R22/R23/R24）：
//
// 1. 事件常量集中在本文件，所有 payload 用 typed struct，**禁止**任意 jsonb 透传。
// 2. payload 字段白名单原则：
//      - 禁止透传 metadata jsonb / snapshot jsonb 原值
//      - 禁止透传 reference_id 自由字符串（如需关联，hash 后再带）
//      - 禁止透传 drift 检测的 expected/actual 数值（落 ledger metadata，不进 outbox payload）
//      - 不携带 amount 之外的金额明细字段
//      - frozen 事件携带 reason_code enum，**不**含 diff 详情（防 PII / 内部状态泄漏）
//
// 3. 当前 5 个事件类型；未来命名空间预留（D-min 落地）：
//      - account.activated      —— business_account.status 从 pending 切到 active
//      - account.suspended      —— business_account.status 从 active 切到 suspended
//      - account.deleted        —— business_account.status 切到 deleted（软删）
//      - account.token.created  —— Admin Token 生命周期事件（在 gateway_admin_token 工作流）
//
// 4. 详细字段定义见 docs/events/v1.md（本工作流的交付物之一）。

// EventType 事件类型常量。
type EventType string

const (
	// EventTypeAccountCreated 业务账户创建成功。
	EventTypeAccountCreated EventType = "account.created"
	// EventTypeAccountRecharged 账户充值成功（含幂等命中第一次成功的情形）。
	EventTypeAccountRecharged EventType = "account.recharged"
	// EventTypeAccountRefunded 账户退款成功（used_total 减 + available 增）。
	EventTypeAccountRefunded EventType = "account.refunded"
	// EventTypeAccountFrozen 账户被冻结（drift_detected / manual_freeze / rebuild_in_progress）。
	EventTypeAccountFrozen EventType = "account.frozen"
	// EventTypeAccountUnfrozen 账户解冻（manual_unfreeze / rebuild_completed）。
	EventTypeAccountUnfrozen EventType = "account.unfrozen"
)

// ReasonCode 冻结/解冻的标准化原因码（计划 R23）。
// 调用方根据 reason_code 决定下游处理动作（业务系统侧）。
type ReasonCode string

const (
	// ReasonCodeDriftDetected reconciler 检测到 ledger SUM 与 balance 不一致，二次确认后仍不一致。
	ReasonCodeDriftDetected ReasonCode = "drift_detected"
	// ReasonCodeManualFreeze 运维通过程序 / 未来管理后台主动冻结（P0 只通过程序内 Freeze 调用）。
	ReasonCodeManualFreeze ReasonCode = "manual_freeze"
	// ReasonCodeManualUnfreeze 运维主动解冻（drift 确认非真实后）。
	ReasonCodeManualUnfreeze ReasonCode = "manual_unfreeze"
	// ReasonCodeRebuildInProgress RebuildBalance 第一阶段：freeze 账户准备重算。
	ReasonCodeRebuildInProgress ReasonCode = "rebuild_in_progress"
	// ReasonCodeRebuildCompleted RebuildBalance 第三阶段：写回新 balance + 解冻。
	ReasonCodeRebuildCompleted ReasonCode = "rebuild_completed"
)

// AccountCreatedPayload account.created 事件 payload。
type AccountCreatedPayload struct {
	BusinessAccountID string    `json:"business_account_id"`
	IsolationRequired bool      `json:"isolation_required"`
	OccurredAt        time.Time `json:"occurred_at"`
}

// AccountRechargedPayload account.recharged 事件 payload。
//
// 金额 (Amount) 是这次充值的金额；NewAvailable / NewRechargeTotal 是充值后的快照值。
// 不包含 idempotency_key / metadata / snapshot 原值。
type AccountRechargedPayload struct {
	BusinessAccountID string    `json:"business_account_id"`
	Amount            int64     `json:"amount"`
	NewAvailable      int64     `json:"new_available"`
	NewRechargeTotal  int64     `json:"new_recharge_total"`
	LedgerEntryID     int64     `json:"ledger_entry_id"`
	OccurredAt        time.Time `json:"occurred_at"`
}

// AccountRefundedPayload account.refunded 事件 payload。
//
// ReferenceType / ReferenceID 仅作业务系统反查路径，**不**透传任意自由字符串；
// 写 outbox 前由 service 做长度/字符校验。
type AccountRefundedPayload struct {
	BusinessAccountID string    `json:"business_account_id"`
	Amount            int64     `json:"amount"`
	NewAvailable      int64     `json:"new_available"`
	NewUsedTotal      int64     `json:"new_used_total"`
	NewRefundTotal    int64     `json:"new_refund_total"`
	LedgerEntryID     int64     `json:"ledger_entry_id"`
	ReferenceType     string    `json:"reference_type,omitempty"`
	ReferenceID       string    `json:"reference_id,omitempty"`
	OccurredAt        time.Time `json:"occurred_at"`
}

// AccountFrozenPayload account.frozen 事件 payload。
//
// 仅携带 reason_code；diff 详情（expected vs actual）走 ledger.metadata 落地，**不**进 outbox payload。
type AccountFrozenPayload struct {
	BusinessAccountID string     `json:"business_account_id"`
	ReasonCode        ReasonCode `json:"reason_code"`
	OccurredAt        time.Time  `json:"occurred_at"`
}

// AccountUnfrozenPayload account.unfrozen 事件 payload。
type AccountUnfrozenPayload struct {
	BusinessAccountID string     `json:"business_account_id"`
	ReasonCode        ReasonCode `json:"reason_code"`
	OccurredAt        time.Time  `json:"occurred_at"`
}

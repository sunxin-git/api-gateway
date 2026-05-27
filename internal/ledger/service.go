package ledger

import (
	"context"
	"time"
)

// WriteOutcome 账本写操作结果类型（计划 R11+D11，D-min document-review 添加）。
//
// Recharge / Refund 同时支持"首次新写"和"幂等命中"两条路径；调用方（D-min handler）
// 需区分二者：
//   - FreshlyWritten：本次调用产生了新 ledger entry（INSERT 成功）；handler 应累加 daily 配额
//   - IdempotentReplay：本次调用命中既有 entry（idempotency_key 或 correlation_id UNIQUE）；
//     handler **不**累加配额（已在首次写时累过），但返回的 *LedgerEntry 仍是有效结果
//
// 没有这个区分时，业务系统重试场景下 handler 会双重累加 daily 配额 → 配额被人为膨胀
// → 合法请求被误拒 429。
type WriteOutcome int

const (
	// WriteOutcomeFreshlyWritten 本次调用产生了新 ledger entry。
	WriteOutcomeFreshlyWritten WriteOutcome = iota + 1
	// WriteOutcomeIdempotentReplay 本次调用命中既有 entry（幂等命中，无新 entry 写入）。
	WriteOutcomeIdempotentReplay
)

// String 返回可读形式，便于日志输出。
func (o WriteOutcome) String() string {
	switch o {
	case WriteOutcomeFreshlyWritten:
		return "freshly_written"
	case WriteOutcomeIdempotentReplay:
		return "idempotent_replay"
	default:
		return "unknown"
	}
}

// Service 账本服务接口（计划 R6 / Unit 3）。
//
// 实现：PostgresService（internal/ledger/postgres.go）。
//
// 设计原则（CLAUDE.md §三）：
//   - 显式优于隐式：每个写操作必带 Actor + 必要参数；不依赖 service 持有的隐式 state
//   - 失败优先：所有错误返回 sentinel（11 个）；fail-closed
//   - 输出说重点：返回 *LedgerEntry / *Balance 仅含 service 层语义需要的字段
//   - reimplement 纪律：不复用 third-party/new-api/ 任何代码
//
// 并发与原子性：
//   - 每个写操作内部用 PG CTE 单语句 + tx 级 ROLLBACK 双保险
//   - service 不内部重试；返 ErrVersionConflict 让调用方决定（避免重试风暴）
//
// 幂等：
//   - Recharge：(entry_type, idempotency_key) 复合 UNIQUE；body sha256 一致返原 entry，不一致 → ErrIdempotencyConflict
//   - Reserve/Commit/Release/Refund：(business_account_id, correlation_id, entry_type) 复合 UNIQUE；同 correlation 直接返原 entry
type Service interface {
	// CreateAccount 创建业务账户（同事务建 balance(zeros) + outbox account.created）。
	// 错误：账户已存在 → ErrAccountAlreadyExists（决策 D12，D-min document-review 修订）。
	CreateAccount(ctx context.Context, actor Actor, params CreateAccountParams) (*Account, error)

	// Recharge 充值入账。
	// 入口校验 amount > 0；CAS 含 frozen=false。
	// 错误：ErrInvalidAmount / ErrAccountNotFound / ErrAccountFrozen / ErrVersionConflict / ErrIdempotencyConflict。
	//
	// 返回 WriteOutcome 区分"首次新写" vs "幂等命中"（D11 + D-min document-review）：
	//   - FreshlyWritten：新 entry 写入；调用方累加 daily 配额
	//   - IdempotentReplay：idempotency_key 命中且 body 一致，返原 entry；调用方**不**累加配额
	Recharge(ctx context.Context, actor Actor, params RechargeParams) (*LedgerEntry, WriteOutcome, error)

	// Reserve 预占额度。
	// 入口校验 amount > 0；CAS 含 frozen=false AND available >= amount。
	// 错误：ErrInvalidAmount / ErrAccountNotFound / ErrAccountFrozen / ErrInsufficientBalance / ErrVersionConflict。
	Reserve(ctx context.Context, actor Actor, params ReserveParams) (*LedgerEntry, error)

	// Commit 结算预占。
	// 入口校验 amount > 0 AND actualCost <= reserve.amount；可能产 2 条 entry（commit + 可选 release）。
	// 不查 frozen（允许 inflight 账户完成）。
	// 错误：ErrInvalidAmount / ErrCommitExceedsReserved / ErrReserveNotFound / ErrAlreadySettled
	//      / ErrInsufficientReserved / ErrVersionConflict。
	Commit(ctx context.Context, actor Actor, params CommitParams) ([]LedgerEntry, error)

	// Release 释放预占（不结算，全额回 available）。
	// 不查 frozen（允许管理动作）。
	// 错误：ErrInvalidAmount / ErrReserveNotFound / ErrAlreadySettled / ErrInsufficientReserved / ErrVersionConflict。
	Release(ctx context.Context, actor Actor, params ReleaseParams) (*LedgerEntry, error)

	// Refund 退款（used_total 减 + available 增 + refund_total 增）。
	// 不查 frozen（管理动作允许）；refund_total 不进不变量等式。
	// 错误：ErrInvalidAmount / ErrAccountNotFound / ErrInsufficientUsed / ErrVersionConflict。
	//
	// 返回 WriteOutcome 区分"首次新写" vs "幂等命中"（D11 + D-min document-review）。
	Refund(ctx context.Context, actor Actor, params RefundParams) (*LedgerEntry, WriteOutcome, error)

	// GetBalance 读取账户当前余额（含 frozen 状态）。
	// 错误：ErrAccountNotFound。
	GetBalance(ctx context.Context, accountID string) (*Balance, error)

	// Freeze 冻结账户余额（drift / 管理）。
	// 自带 tx；reasonCode 须为 ReasonCode* 常量之一。已 frozen 视为幂等成功，不发 outbox。
	// 错误：ErrAccountNotFound / ErrVersionConflict（无 ErrAccountFrozen —— 已 frozen 不视为错误）。
	Freeze(ctx context.Context, actor Actor, accountID string, reasonCode ReasonCode) error

	// Unfreeze 解冻账户余额。
	// 已 unfrozen 视为幂等成功，不发 outbox。
	// 错误：ErrAccountNotFound / ErrVersionConflict。
	Unfreeze(ctx context.Context, actor Actor, accountID string, reasonCode ReasonCode) error

	// RebuildBalance 余额重建（U8 落地，本 Unit 占位返回 ErrNotImplemented）。
	// 拆 3 个独立 tx：freeze / read snapshot REPEATABLE READ / replace + unfreeze。
	RebuildBalance(ctx context.Context, actor Actor, accountID string) (*Balance, error)
}

// === 入参 DTO ===

// CreateAccountParams CreateAccount 入参。
type CreateAccountParams struct {
	// ID 业务账户外部 ID（text PK）。
	ID string
	// IsolationRequired 企业隔离硬开关；默认 false。
	IsolationRequired bool
	// Metadata 业务侧附加标签 json（已 marshal 的 []byte）；可为 nil（service 转 '{}'::jsonb）。
	Metadata []byte
}

// RechargeParams Recharge 入参。
type RechargeParams struct {
	// AccountID 业务账户 ID。
	AccountID string
	// Amount 充值金额（minor unit，必须 > 0）。
	Amount int64
	// CorrelationID 业务关联 ID（如 topup_order 编号）；非空但允许重复（充值不靠它幂等）。
	CorrelationID string
	// IdempotencyKey 充值幂等键；非空时走 (entry_type='recharge', idempotency_key) 复合 UNIQUE。
	IdempotencyKey string
	// CanonicalBody 用于计算 sha256 的明确 struct（防 sha256 实现漂移）。
	// 当前仅 RechargeBody 实现；P0 admin-cli 路径写死 amount+account_id+external_ref。
	CanonicalBody *RechargeBody
	// ReferenceType / ReferenceID 反查路径（如 "topup_order" / order_id）；可空。
	ReferenceType string
	ReferenceID   string
	// Metadata 业务侧附加标签；可为 nil。
	Metadata []byte
}

// RechargeBody 充值幂等键 canonical body 的明确 struct（避免任意 interface{} 嵌套）。
//
// 字段顺序与计算 sha256 时的字段排序无关 —— canonicalizeBody 用 reflect 按字段名 lexicographic
// 排序后 json.Marshal，故字段名为唯一决定因素；勿轻易改字段名。
type RechargeBody struct {
	AccountID   string `json:"account_id"`
	Amount      int64  `json:"amount"`
	ExternalRef string `json:"external_ref,omitempty"`
}

// ReserveParams Reserve 入参。
type ReserveParams struct {
	AccountID     string
	Amount        int64
	CorrelationID string // 必填；(account, correlation, entry_type='reserve') 复合 UNIQUE
	// ReferenceType / ReferenceID 一般为 "task" / task_id（reserve 关联 task 路径）。
	ReferenceType string
	ReferenceID   string
	Metadata      []byte
}

// CommitParams Commit 入参。
type CommitParams struct {
	AccountID     string
	CorrelationID string // 必填；指向原 reserve entry
	ActualCost    int64  // 实际消耗；必须 > 0 且 ≤ reserve.amount
	ReferenceType string
	ReferenceID   string
	Metadata      []byte
}

// ReleaseParams Release 入参（不结算，全额释放）。
type ReleaseParams struct {
	AccountID     string
	CorrelationID string // 必填；指向原 reserve entry
	Amount        int64  // 期望释放金额；与 reserve.amount 一致（service 入口校验）
	ReferenceType string
	ReferenceID   string
	Metadata      []byte
}

// RefundParams Refund 入参。
type RefundParams struct {
	AccountID     string
	Amount        int64
	CorrelationID string // 必填；refund 自己的关联 ID（通常 manual_refund 编号）
	ReferenceType string
	ReferenceID   string
	Metadata      []byte
}

// === 返回 DTO ===

// Account 业务账户视图。
type Account struct {
	ID                string
	Status            string // active / suspended / frozen / deleted
	IsolationRequired bool
	BreakGlassUntil   *time.Time
	Metadata          []byte
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// Balance 账户余额视图。
type Balance struct {
	BusinessAccountID string
	Available         int64
	Reserved          int64
	UsedTotal         int64
	RechargeTotal     int64
	RefundTotal       int64
	Version           int64
	Frozen            bool
	FrozenReason      string
	FrozenAt          *time.Time
	UpdatedAt         time.Time
	LastLedgerID      int64
}

// LedgerEntry 账本流水视图。
type LedgerEntry struct {
	ID                int64
	BusinessAccountID string
	EntryType         string // recharge / reserve / commit / release / refund / ...
	Amount            int64
	AvailableDelta    int64
	ReservedDelta     int64
	UsedDelta         int64
	CorrelationID     string
	IdempotencyKey    string
	ReferenceType     string
	ReferenceID       string
	Metadata          []byte
	Snapshot          []byte
	ActorType         string
	ActorID           string
	CreatedAt         time.Time
}

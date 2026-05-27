// Package admin 提供 Admin API 的 HTTP handler 层。
//
// 计划：docs/plans/2026-05-27-003-feat-workflow-d-min-admin-api-plan.md Unit 5
// 设计文档：docs/multimedia-gateway-design.md §9bis.6
//
// 5 个 endpoint：
//
//	POST /admin/v1/business-accounts            → CreateAccount
//	POST /admin/v1/business-accounts/:id/recharge → Recharge（幂等 = external_ref）
//	POST /admin/v1/business-accounts/:id/refund   → Refund（correlation_id 复合 UNIQUE）
//	GET  /admin/v1/business-accounts/:id/balance  → GetBalance
//	GET  /admin/v1/whoami                          → Whoami（任何已鉴权 token 自检）
//
// Handler 与 LedgerService / admintoken.Service / admintoken.Throttle / audit.Logger 解耦：
// 通过 Constructor 注入接口；handler 内部不直接 import 框架细节。
package admin

import (
	"encoding/json"
	"regexp"
	"strings"
)

// =============================================================================
// 共享字段
// =============================================================================

// MaxAccountIDLen / MaxRefLen / MaxIdempotencyKeyLen 入参长度上限。
//
// 与 PG schema CHECK 约束对齐：
//   - business_account.id text PK 不限长但实际靠 application 层防御
//   - business_account_ledger.idempotency_key text；canonical_body_sha256 64 char hex
const (
	MaxAccountIDLen      = 64
	MaxRefLen            = 64
	MaxIdempotencyKeyLen = 128
	MaxCorrelationIDLen  = 128
)

// accountIDPattern business account ID 字符集白名单。
// 允许字母 / 数字 / 下划线 / 短横线（与 docs/multimedia-gateway-design.md §术语表对齐）。
var accountIDPattern = regexp.MustCompile(`^[A-Za-z0-9_\-]+$`)

// MaxRechargeAmount / MaxRefundAmount 单笔金额上限。
//
// 防御性 cap：单笔超过 math.MaxInt64/2 会让账本累加溢出风险（recharge_total 等 SUM 字段）；
// 实际业务由 token.SingleRechargeMax / single_refund_max 阀门更严格控制。
const MaxRechargeAmount int64 = 1 << 62 // ~4.6 × 10^18，远大于现实需要

// =============================================================================
// CreateAccount
// =============================================================================

// CreateAccountRequest POST /business-accounts 入参。
type CreateAccountRequest struct {
	// ID 业务账户外部 ID；1 ≤ len ≤ 64；字符集 [A-Za-z0-9_-]
	ID string `json:"id" binding:"required"`
	// IsolationRequired 企业隔离硬开关；默认 false
	IsolationRequired bool `json:"isolation_required"`
	// Metadata 业务侧附加标签；json.RawMessage 透传，service 层校验合法 JSON
	Metadata json.RawMessage `json:"metadata,omitempty"`
}

// validate 入参校验；返回错误 message（中文 + 显式字段）。
func (r *CreateAccountRequest) validate() string {
	id := strings.TrimSpace(r.ID)
	if id == "" {
		return "id 不能为空"
	}
	if len(id) > MaxAccountIDLen {
		return "id 长度超过上限（64 字节）"
	}
	if !accountIDPattern.MatchString(id) {
		return "id 仅允许字母 / 数字 / 下划线 / 短横线"
	}
	return ""
}

// AccountResponse 账户视图（CreateAccount 成功返回 + GetBalance 嵌入）。
type AccountResponse struct {
	ID                string `json:"id"`
	Status            string `json:"status"`
	IsolationRequired bool   `json:"isolation_required"`
	CreatedAt         string `json:"created_at"` // RFC3339
}

// =============================================================================
// Recharge
// =============================================================================

// RechargeRequest POST /business-accounts/:id/recharge 入参。
type RechargeRequest struct {
	// Amount 充值金额（minor unit）；> 0
	Amount int64 `json:"amount" binding:"required"`
	// ExternalRef 业务系统提供的幂等键；同 (entry_type='recharge', idempotency_key) UNIQUE 防重复扣款
	ExternalRef string `json:"external_ref" binding:"required"`
	// ReferenceType / ReferenceID 反查路径（如 topup_order / order-id）；可空
	ReferenceType string          `json:"reference_type,omitempty"`
	ReferenceID   string          `json:"reference_id,omitempty"`
	Metadata      json.RawMessage `json:"metadata,omitempty"`
}

func (r *RechargeRequest) validate() string {
	if r.Amount <= 0 {
		return "amount 必须大于 0"
	}
	if r.Amount > MaxRechargeAmount {
		return "amount 超出最大允许金额"
	}
	ref := strings.TrimSpace(r.ExternalRef)
	if ref == "" {
		return "external_ref 不能为空"
	}
	if len(ref) > MaxIdempotencyKeyLen {
		return "external_ref 长度超过上限（128 字节）"
	}
	if len(r.ReferenceType) > MaxRefLen {
		return "reference_type 长度超过上限（64 字节）"
	}
	if len(r.ReferenceID) > MaxRefLen {
		return "reference_id 长度超过上限（64 字节）"
	}
	return ""
}

// LedgerEntryResponse 充值 / 退款返回的 ledger entry 视图。
type LedgerEntryResponse struct {
	ID             int64  `json:"id"`
	EntryType      string `json:"entry_type"`
	Amount         int64  `json:"amount"`
	AvailableDelta int64  `json:"available_delta"`
	UsedDelta      int64  `json:"used_delta"`
	CorrelationID  string `json:"correlation_id"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	CreatedAt      string `json:"created_at"`
	// Idempotent 是否本次为幂等重放命中（true = LedgerService 返 IdempotentReplay）。
	// 业务系统可据此判断"是不是首次入账"做下游处理。
	Idempotent bool `json:"idempotent"`
}

// =============================================================================
// Refund
// =============================================================================

// RefundRequest POST /business-accounts/:id/refund 入参。
type RefundRequest struct {
	// Amount 退款金额；> 0；不能超过当前 used_total
	Amount int64 `json:"amount" binding:"required"`
	// CorrelationID 业务关联 ID（manual_refund 编号）；与 (entry_type='refund') 复合 UNIQUE
	CorrelationID string          `json:"correlation_id" binding:"required"`
	ReferenceType string          `json:"reference_type,omitempty"`
	ReferenceID   string          `json:"reference_id,omitempty"`
	Metadata      json.RawMessage `json:"metadata,omitempty"`
}

func (r *RefundRequest) validate() string {
	if r.Amount <= 0 {
		return "amount 必须大于 0"
	}
	if r.Amount > MaxRechargeAmount {
		return "amount 超出最大允许金额"
	}
	cid := strings.TrimSpace(r.CorrelationID)
	if cid == "" {
		return "correlation_id 不能为空"
	}
	if len(cid) > MaxCorrelationIDLen {
		return "correlation_id 长度超过上限（128 字节）"
	}
	if len(r.ReferenceType) > MaxRefLen {
		return "reference_type 长度超过上限（64 字节）"
	}
	if len(r.ReferenceID) > MaxRefLen {
		return "reference_id 长度超过上限（64 字节）"
	}
	return ""
}

// =============================================================================
// GetBalance
// =============================================================================

// BalanceResponse GET /business-accounts/:id/balance 返回。
type BalanceResponse struct {
	AccountID     string `json:"account_id"`
	Available     int64  `json:"available"`
	Reserved      int64  `json:"reserved"`
	UsedTotal     int64  `json:"used_total"`
	RechargeTotal int64  `json:"recharge_total"`
	RefundTotal   int64  `json:"refund_total"`
	Frozen        bool   `json:"frozen"`
	FrozenReason  string `json:"frozen_reason,omitempty"`
	Version       int64  `json:"version"`
	UpdatedAt     string `json:"updated_at"`
}

// =============================================================================
// Whoami
// =============================================================================

// WhoamiResponse GET /whoami 返回 token 自检信息。
//
// 不返回：token_hash / ip_allowlist 具体 CIDR 列表（防 token 泄露后嗅探精确网段配置）。
type WhoamiResponse struct {
	TokenID              int64           `json:"token_id"`
	Description          string          `json:"description"`
	Scopes               []string        `json:"scopes"`
	IPAllowlistCIDRCount int             `json:"ip_allowlist_cidr_count"`
	ExpiresAt            *string         `json:"expires_at,omitempty"`
	ThrottleLimits       ThrottleLimits  `json:"throttle_limits"`
	TodayUsageUTC        UsageBlock      `json:"today_usage_utc"`
	CircuitState         CircuitStateDTO `json:"circuit_state"`
	ServerTimeUTC        string          `json:"server_time_utc"`
}

// ThrottleLimits whoami 中的阀门快照（nil pointer = 无限制）。
type ThrottleLimits struct {
	SingleRechargeMax       *int64 `json:"single_recharge_max"`
	DailyRechargeQuotaLimit *int64 `json:"daily_recharge_quota_limit"`
	SingleRefundMax         *int64 `json:"single_refund_max"`
	DailyRefundQuotaLimit   *int64 `json:"daily_refund_quota_limit"`
	DailyAccountCreateLimit *int32 `json:"daily_account_create_limit"`
	RequestsPerMinute       *int32 `json:"requests_per_minute"`
	CircuitBreakerEnabled   bool   `json:"circuit_breaker_enabled"`
}

// UsageBlock 当日累计用量。
type UsageBlock struct {
	RechargeTotalMinor int64 `json:"recharge_total_minor"`
	RefundTotalMinor   int64 `json:"refund_total_minor"`
	AccountCreateCount int32 `json:"account_create_count"`
}

// CircuitStateDTO 熔断器状态视图。
type CircuitStateDTO struct {
	Open               bool    `json:"open"`
	TrippedUntil       *string `json:"tripped_until"`
	ErrorCountInWindow int32   `json:"error_count_in_window"`
}

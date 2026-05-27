package admintoken

import (
	"database/sql"
	"net/netip"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// Token Admin Token 完整视图（Service 内部使用，不直接返给 HTTP）。
//
// 字段映射 schema gateway_admin_token + 解析后的 ip_allowlist。
// TokenHash 仅在 Postgres 路径内可见；上层（middleware / handler）只接触 ValidationResult / List 返回值。
type Token struct {
	// ID 自增主键。
	ID int64
	// TokenHash hex 字符串（64 字符）；HMAC-SHA-256(pepper, plaintext)。
	// **不要**暴露给 HTTP；list 接口需手工置空。
	TokenHash string
	// Description 运营标签，如 "creator-platform-prod"。
	Description string
	// Scopes 细粒度权限范围，如 ["business_account:read", "business_account:recharge"]。
	Scopes []string
	// AllowedCIDRs 解析后的 CIDR 列表；空切片 = fail-closed 拒绝全部请求。
	AllowedCIDRs []netip.Prefix

	// === 7 阀门字段（nil pointer = 无限制；非 nil = 该上限值，单位 minor unit / 次数 / 次/分钟） ===

	// SingleRechargeMax 单笔充值上限（minor unit）。
	SingleRechargeMax *int64
	// DailyRechargeQuotaLimit 当日累计充值上限（minor unit）。
	DailyRechargeQuotaLimit *int64
	// SingleRefundMax 单笔退款上限（document-review 添加）。
	SingleRefundMax *int64
	// DailyRefundQuotaLimit 当日累计退款上限（document-review 添加）。
	DailyRefundQuotaLimit *int64
	// DailyAccountCreateLimit 当日创建账户数上限。
	DailyAccountCreateLimit *int32
	// RequestsPerMinute RPM 限速。
	RequestsPerMinute *int32
	// CircuitBreakerEnabled 是否启用熔断器（bool NOT NULL，false = 不启用）。
	CircuitBreakerEnabled bool

	// === 审计字段 ===

	// CreatedBy 创建者标识，如 "cli:bootstrap"（admin-cli 路径写死）。
	CreatedBy string
	// CreatedAt token 创建时刻。
	CreatedAt time.Time
	// ExpiresAt 过期时间；nil = 永不过期。
	ExpiresAt *time.Time
	// RevokedAt 吊销时间；nil = 未吊销。
	RevokedAt *time.Time
}

// CreateParams Create 入参；admin-cli token create 子命令直接映射。
type CreateParams struct {
	// Description 必填；token 运营标签。
	Description string
	// Scopes 必填且 ≥ 1；细粒度权限。Create 不去重 / 不排序，原样入库。
	Scopes []string
	// AllowedCIDRs 必填且 ≥ 1；fail-closed 拒绝空 allowlist。
	AllowedCIDRs []netip.Prefix

	// 7 阀门字段（nil 指针 = 不设上限）。
	SingleRechargeMax       *int64
	DailyRechargeQuotaLimit *int64
	SingleRefundMax         *int64
	DailyRefundQuotaLimit   *int64
	DailyAccountCreateLimit *int32
	RequestsPerMinute       *int32
	CircuitBreakerEnabled   bool

	// CreatedBy 必填；admin-cli 路径写 "cli:bootstrap"。
	CreatedBy string

	// ExpiresAt 可空；nil = 永不过期。
	ExpiresAt *time.Time
}

// ValidationResult ValidateByPlaintext 返回值；middleware 拿它注入 ctx 供 handler / scope check 使用。
//
// 不含 TokenHash（防上层把 hash 写入 audit）。
type ValidationResult struct {
	// Token 完整 token 视图（hash 置空）；middleware 与 handler 路径都从此读 scopes / 阀门。
	Token *Token
}

// =============================================================================
// 类型转换 helpers（*int64 ↔ pgtype.Int8 / *int32 ↔ pgtype.Int4 / *time.Time ↔ sql.NullTime）
//
// sqlc 生成的 param / row 用 pgtype.Int8 / pgtype.Int4；本包对外用 *int64 / *int32 表达"可空"
// 更符合 Go 习惯。helpers 集中在此，避免散落各处。
// =============================================================================

func pgInt8(p *int64) pgtype.Int8 {
	if p == nil {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: *p, Valid: true}
}

func pgInt4(p *int32) pgtype.Int4 {
	if p == nil {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: *p, Valid: true}
}

func nullTime(p *time.Time) sql.NullTime {
	if p == nil {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: *p, Valid: true}
}

func ptrInt64(n pgtype.Int8) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}

func ptrInt32(n pgtype.Int4) *int32 {
	if !n.Valid {
		return nil
	}
	v := n.Int32
	return &v
}

func ptrTime(n sql.NullTime) *time.Time {
	if !n.Valid {
		return nil
	}
	v := n.Time
	return &v
}

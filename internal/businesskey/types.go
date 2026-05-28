package businesskey

import (
	"database/sql"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// Key 业务系统 API Key 完整视图（Service 内部使用，不直接返给 HTTP）。
//
// 字段映射 schema business_account_api_key。
// KeyHash 仅在 Postgres 路径内可见；上层（middleware / handler）只接触 ValidationResult / List 返回值（其中 hash 字段已置空）。
type Key struct {
	// ID 自增主键。
	ID int64
	// BusinessAccountID 关联的业务账户 ID（FK CASCADE）。
	BusinessAccountID string
	// Description 运营标签，如 "creator-platform-prod-key-1"。
	Description string
	// KeyHash hex 字符串（64 字符）；HMAC-SHA-256(GATEWAY_TOKEN_PEPPER, plaintext)。
	// **不要**暴露给 HTTP；list / GetByID 返回时已置空。
	KeyHash string

	// RequestsPerMinute RPM 限速；nil = 不限速；按 key.id 维度计数（InProcessRPM）。
	RequestsPerMinute *int32

	// CreatedBy 创建者标识，如 "cli:bootstrap"（admin-cli 硬编码）。
	CreatedBy string
	// CreatedAt key 创建时刻。
	CreatedAt time.Time
	// RevokedAt 吊销时间；nil = 未吊销。
	RevokedAt *time.Time
	// LastUsedAt 最近鉴权命中时刻（best-effort 异步批量更新）；nil = 从未鉴权命中。
	LastUsedAt *time.Time
	// UpdatedAt 最近更新时刻（含 last_used_at flush）。
	UpdatedAt time.Time
}

// CreateParams Create 入参；admin-cli business-key create 子命令直接映射。
type CreateParams struct {
	// BusinessAccountID 必填；指向已存在 business_account 行，FK CASCADE。
	BusinessAccountID string
	// Description 必填；运营标签。
	Description string
	// RequestsPerMinute 可选；nil = 不限速；正数 = RPM 阀门。
	RequestsPerMinute *int32
	// CreatedBy 必填；admin-cli 路径写 "cli:bootstrap"。
	CreatedBy string
}

// ValidationResult ValidateByPlaintext 返回值；middleware 拿它注入 ctx 供 handler 使用。
//
// 不含 KeyHash（防上层把 hash 写入 audit / log）。
type ValidationResult struct {
	// Key 完整 key 视图（hash 置空）；middleware 与 handler 路径都从此读 business_account_id / RPM。
	Key *Key
}

// =============================================================================
// 类型转换 helpers（*int32 ↔ pgtype.Int4 / *time.Time ↔ sql.NullTime）
//
// sqlc 生成的 param / row 用 pgtype.Int4 / sql.NullTime；本包对外用 *int32 / *time.Time
// 表达"可空"更符合 Go 习惯。helpers 集中在此，避免散落各处。
// =============================================================================

func pgInt4(p *int32) pgtype.Int4 {
	if p == nil {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: *p, Valid: true}
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

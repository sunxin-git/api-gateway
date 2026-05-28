package businesskey

import (
	"github.com/sunxin-git/api-gateway/internal/db"
)

// rowmap.go 把 sqlc 生成的 Row / 模型类型转为本包对外的 Key struct。
//
// 设计动机：sqlc 给 ListActive*Row 类型剔除了 key_hash 字段（query 不 SELECT），
// 与全字段 BusinessAccountApiKey 类型不同；统一在此处映射，避免散落各 query 调用点。

// modelToKey BusinessAccountApiKey（含 key_hash）→ Key；调用方按需把 KeyHash 置空。
func modelToKey(m db.BusinessAccountApiKey) *Key {
	return &Key{
		ID:                m.ID,
		BusinessAccountID: m.BusinessAccountID,
		Description:       m.Description,
		KeyHash:           m.KeyHash,
		RequestsPerMinute: ptrInt32(m.RequestsPerMinute),
		CreatedBy:         m.CreatedBy,
		CreatedAt:         m.CreatedAt,
		RevokedAt:         ptrTime(m.RevokedAt),
		LastUsedAt:        ptrTime(m.LastUsedAt),
		UpdatedAt:         m.UpdatedAt,
	}
}

// listByAccountRowToKey list query 不返 key_hash；统一映射为 KeyHash="" 的 Key。
func listByAccountRowToKey(r db.ListActiveBusinessKeysByAccountRow) *Key {
	return &Key{
		ID:                r.ID,
		BusinessAccountID: r.BusinessAccountID,
		Description:       r.Description,
		KeyHash:           "", // list query 不 SELECT hash
		RequestsPerMinute: ptrInt32(r.RequestsPerMinute),
		CreatedBy:         r.CreatedBy,
		CreatedAt:         r.CreatedAt,
		RevokedAt:         ptrTime(r.RevokedAt),
		LastUsedAt:        ptrTime(r.LastUsedAt),
		UpdatedAt:         r.UpdatedAt,
	}
}

func listAllRowToKey(r db.ListAllActiveBusinessKeysRow) *Key {
	return &Key{
		ID:                r.ID,
		BusinessAccountID: r.BusinessAccountID,
		Description:       r.Description,
		KeyHash:           "",
		RequestsPerMinute: ptrInt32(r.RequestsPerMinute),
		CreatedBy:         r.CreatedBy,
		CreatedAt:         r.CreatedAt,
		RevokedAt:         ptrTime(r.RevokedAt),
		LastUsedAt:        ptrTime(r.LastUsedAt),
		UpdatedAt:         r.UpdatedAt,
	}
}

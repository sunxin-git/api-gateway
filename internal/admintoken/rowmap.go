package admintoken

import (
	"github.com/sunxin-git/api-gateway/internal/db"
)

// rowmap.go 把 sqlc 生成的 Row 类型转为本包对外的 Token struct。
//
// 设计动机：sqlc 给每个 query 生成独立 Row struct（即便字段相同），导致 5 个 query 有 5 个
// Row 类型；本包内 Token 是单一统一模型，集中在此处转换，避免散落各 query 调用点。

func insertRowToToken(r db.InsertAdminTokenRow) *Token {
	return &Token{
		ID:                      r.ID,
		TokenHash:               r.TokenHash,
		Description:             r.Description,
		Scopes:                  r.Scopes,
		AllowedCIDRs:            r.IpAllowlist,
		SingleRechargeMax:       ptrInt64(r.SingleRechargeMax),
		DailyRechargeQuotaLimit: ptrInt64(r.DailyRechargeQuotaLimit),
		SingleRefundMax:         ptrInt64(r.SingleRefundMax),
		DailyRefundQuotaLimit:   ptrInt64(r.DailyRefundQuotaLimit),
		DailyAccountCreateLimit: ptrInt32(r.DailyAccountCreateLimit),
		RequestsPerMinute:       ptrInt32(r.RequestsPerMinute),
		CircuitBreakerEnabled:   r.CircuitBreakerEnabled,
		CreatedBy:               r.CreatedBy,
		CreatedAt:               r.CreatedAt,
		ExpiresAt:               ptrTime(r.ExpiresAt),
		RevokedAt:               ptrTime(r.RevokedAt),
	}
}

func findRowToToken(r db.FindActiveAdminTokenByHashRow) *Token {
	return &Token{
		ID:                      r.ID,
		TokenHash:               r.TokenHash,
		Description:             r.Description,
		Scopes:                  r.Scopes,
		AllowedCIDRs:            r.IpAllowlist,
		SingleRechargeMax:       ptrInt64(r.SingleRechargeMax),
		DailyRechargeQuotaLimit: ptrInt64(r.DailyRechargeQuotaLimit),
		SingleRefundMax:         ptrInt64(r.SingleRefundMax),
		DailyRefundQuotaLimit:   ptrInt64(r.DailyRefundQuotaLimit),
		DailyAccountCreateLimit: ptrInt32(r.DailyAccountCreateLimit),
		RequestsPerMinute:       ptrInt32(r.RequestsPerMinute),
		CircuitBreakerEnabled:   r.CircuitBreakerEnabled,
		CreatedBy:               r.CreatedBy,
		CreatedAt:               r.CreatedAt,
		ExpiresAt:               ptrTime(r.ExpiresAt),
		RevokedAt:               ptrTime(r.RevokedAt),
	}
}

func byIDRowToToken(r db.FindAdminTokenByIDRow) *Token {
	return &Token{
		ID:                      r.ID,
		TokenHash:               r.TokenHash,
		Description:             r.Description,
		Scopes:                  r.Scopes,
		AllowedCIDRs:            r.IpAllowlist,
		SingleRechargeMax:       ptrInt64(r.SingleRechargeMax),
		DailyRechargeQuotaLimit: ptrInt64(r.DailyRechargeQuotaLimit),
		SingleRefundMax:         ptrInt64(r.SingleRefundMax),
		DailyRefundQuotaLimit:   ptrInt64(r.DailyRefundQuotaLimit),
		DailyAccountCreateLimit: ptrInt32(r.DailyAccountCreateLimit),
		RequestsPerMinute:       ptrInt32(r.RequestsPerMinute),
		CircuitBreakerEnabled:   r.CircuitBreakerEnabled,
		CreatedBy:               r.CreatedBy,
		CreatedAt:               r.CreatedAt,
		ExpiresAt:               ptrTime(r.ExpiresAt),
		RevokedAt:               ptrTime(r.RevokedAt),
	}
}

func listRowToToken(r db.ListActiveAdminTokensRow) *Token {
	return &Token{
		ID:                      r.ID,
		TokenHash:               "", // List query 不 SELECT hash
		Description:             r.Description,
		Scopes:                  r.Scopes,
		AllowedCIDRs:            r.IpAllowlist,
		SingleRechargeMax:       ptrInt64(r.SingleRechargeMax),
		DailyRechargeQuotaLimit: ptrInt64(r.DailyRechargeQuotaLimit),
		SingleRefundMax:         ptrInt64(r.SingleRefundMax),
		DailyRefundQuotaLimit:   ptrInt64(r.DailyRefundQuotaLimit),
		DailyAccountCreateLimit: ptrInt32(r.DailyAccountCreateLimit),
		RequestsPerMinute:       ptrInt32(r.RequestsPerMinute),
		CircuitBreakerEnabled:   r.CircuitBreakerEnabled,
		CreatedBy:               r.CreatedBy,
		CreatedAt:               r.CreatedAt,
		ExpiresAt:               ptrTime(r.ExpiresAt),
		RevokedAt:               ptrTime(r.RevokedAt),
	}
}

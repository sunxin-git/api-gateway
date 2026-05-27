package admintoken

import (
	"context"
	"net/netip"
)

// Service Admin Token 服务接口（计划 R2 / R3 / R4 / R10 / Unit 2）。
//
// 实现：PostgresService（internal/admintoken/postgres.go）。
//
// 设计原则（CLAUDE.md §四）：
//   - 显式优于隐式：ValidateByPlaintext 同时校验 hash + revoked/expired + IP，单 query 拿到 token 后逐项判
//   - 失败优先：空 AllowedCIDRs = 拒全部；revoked/expired token 不进鉴权路径
//   - DIP：上层 middleware 只依赖此接口，便于测试注入 fake Service
type Service interface {
	// Create 创建新 token；返回 token 视图 + 一次性 plaintext 字符串。
	// 调用方（admin-cli token create）负责把 plaintext 安全交付业务系统；本服务**永不**再吐 plaintext。
	//
	// 内部流程：
	//   1. 校验 params（description / scopes ≥ 1 / cidrs ≥ 1 / createdBy 非空）
	//   2. 32 字节 CSPRNG → base64url 编码（plaintext）
	//   3. HMAC-SHA-256(pepper, plaintext) → hex（token_hash）
	//   4. INSERT gateway_admin_token
	//
	// 错误：ErrInvalidParam（入参非法）/ wrapped DB error。
	Create(ctx context.Context, params CreateParams) (*Token, string, error)

	// ValidateByPlaintext 鉴权热路径：验证 plaintext + 源 IP，返回完整 token 视图供后续 scope check 用。
	//
	// 内部流程：
	//   1. HMAC-SHA-256(pepper, plaintext) → hex
	//   2. SELECT ... WHERE token_hash = ? AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > NOW())
	//   3. 解析 ip_allowlist 为 []netip.Prefix；逐个 prefix.Contains(clientIP) 判 IP 命中
	//
	// 错误（按优先级）：
	//   - ErrTokenNotFound：hash 无匹配 / token 已 revoked / 已 expired（单 query 内已过滤）
	//   - ErrIPNotAllowed：CIDR 不命中 / CIDR 列表为空（fail-closed）
	//
	// scope 校验**不**在此处做（middleware AdminScope 单独调 CheckScope）。
	ValidateByPlaintext(ctx context.Context, plaintext string, clientIP netip.Addr) (*ValidationResult, error)

	// CheckScope 检查 token 是否持有指定 scope。
	// O(n) 线性扫描；token.Scopes 通常 ≤ 10 个，足够快。
	// nil token / 空 scopes / 空 requiredScope 一律返 false（fail-closed）。
	CheckScope(token *Token, requiredScope string) bool

	// Revoke 吊销指定 id 的 token。
	// 用 COALESCE(revoked_at, NOW()) 保留首次 revoke 时间戳；多次 Revoke 同 id 返 alreadyRevoked=true。
	//
	// 返回：
	//   - alreadyRevoked=false, err=nil：本次首次 revoke 成功
	//   - alreadyRevoked=true,  err=nil：之前已 revoke（幂等成功，DB 未变更）
	//   - any, ErrTokenNotFound：id 不存在
	Revoke(ctx context.Context, id int64) (alreadyRevoked bool, err error)

	// List 列出所有未 revoke 的 token；按 created_at DESC 排序。
	// 返回的 Token.TokenHash 为空字符串（不暴露 hash）。
	List(ctx context.Context) ([]*Token, error)

	// GetByID 按 id 查 token（不限制 revoked / expired，运维 / audit 用）。
	// 返回的 Token.TokenHash 为空字符串。
	// 错误：ErrTokenNotFound。
	GetByID(ctx context.Context, id int64) (*Token, error)
}

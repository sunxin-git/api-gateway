package businesskey

import "context"

// Service 业务 API Key 服务接口（计划 R2 / R10 / Unit 2）。
//
// 实现：PostgresService（internal/businesskey/postgres.go）。
//
// 设计原则（CLAUDE.md §四）：
//   - 显式优于隐式：ValidateByPlaintext 单 query 拿 key 含 business_account_id；调用方拿 ValidationResult 后自行注入 ctx
//   - 失败优先：所有错误返 sentinel；revoked / not found 在鉴权路径都映射为 ErrKeyNotFound（避免泄露"key 存在但已 revoked"语义给攻击者）
//   - DIP：上层 middleware 只依赖此接口，便于测试注入 fake Service
//
// 与 admintoken.Service 的差异（F-min plan §决策 D4 / D14）：
//   - 无 IP allowlist 校验（业务系统通常多 region 接入；MVP 简化）
//   - 无 scope 概念（一个 key 全权限；与 OpenAI / DeepSeek 风格一致）
//   - 额外提供 TouchLastUsed 异步路径（best-effort 更新 last_used_at）
//   - List 拆为 ListByAccount / ListAll（业务账户维度更常用）
type Service interface {
	// Create 创建新 business api key；返回 key 视图 + 一次性 plaintext。
	// 调用方（admin-cli business-key create）负责把 plaintext 安全交付业务系统；本服务**永不**再吐 plaintext。
	//
	// 内部流程：
	//  1. 校验 params（business_account_id / description 非空；RPM 若非 nil 则必须 > 0）
	//  2. 32 字节 CSPRNG → base64url 编码（plaintext）
	//  3. HMAC-SHA-256(pepper, plaintext) → hex（key_hash）
	//  4. INSERT business_account_api_key（外键 business_account_id 不存在 → 返 PG FK error，包装后返）
	//
	// 错误：ErrInvalidParam / wrapped DB error（如 FK 违反）。
	Create(ctx context.Context, params CreateParams) (*Key, string, error)

	// ValidateByPlaintext 鉴权热路径：验证 plaintext，返回完整 key 视图供后续路径使用。
	//
	// 内部流程：
	//  1. HMAC-SHA-256(pepper, plaintext) → hex
	//  2. SELECT ... WHERE key_hash = ? AND revoked_at IS NULL
	//  3. 命中成功 → 异步 markTouched(key.ID)（写 sync.Map，不阻塞返回）
	//
	// 错误：
	//   - ErrKeyNotFound：hash 无匹配 / key 已 revoked（单 query 内已过滤）
	ValidateByPlaintext(ctx context.Context, plaintext string) (*ValidationResult, error)

	// Revoke 吊销指定 id 的 key。
	// 用 COALESCE(revoked_at, NOW()) 保留首次 revoke 时间戳；多次 Revoke 同 id 返 alreadyRevoked=true。
	//
	// 返回：
	//   - alreadyRevoked=false, err=nil：本次首次 revoke 成功
	//   - alreadyRevoked=true,  err=nil：之前已 revoke（幂等成功，DB 未变更）
	//   - any, ErrKeyNotFound：id 不存在
	Revoke(ctx context.Context, id int64) (alreadyRevoked bool, err error)

	// ListByAccount 列出指定账户的所有未 revoke key；按 created_at DESC 排序。
	// 返回的 Key.KeyHash 为空字符串（不暴露 hash）。
	ListByAccount(ctx context.Context, businessAccountID string) ([]*Key, error)

	// ListAll 列出所有账户的未 revoke key；按 created_at DESC 排序。
	// admin-cli business-key list（不带 filter）用；运维全局审计用。
	// 返回的 Key.KeyHash 为空字符串。
	ListAll(ctx context.Context) ([]*Key, error)

	// GetByID 按 id 查 key（不限制 revoked 状态，运维 / audit 用）。
	// 返回的 Key.KeyHash 为空字符串。
	// 错误：ErrKeyNotFound。
	GetByID(ctx context.Context, id int64) (*Key, error)

	// TouchLastUsed 单 key 的 last_used_at = NOW() 同步更新（best-effort）。
	//
	// 内部使用：异步 flush goroutine 调用本方法 batch update；
	// 失败仅 log，不向调用方返错（last_used_at 不影响鉴权主路径）。
	TouchLastUsed(ctx context.Context, id int64) error

	// Close 触发 last_used_at 最终 flush + 停异步 goroutine；幂等。
	// main.go 进程退出时调用（defer）；测试 cleanup 也调用。
	Close() error
}

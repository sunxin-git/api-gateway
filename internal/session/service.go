package session

import (
	"context"
	"errors"
	"time"
)

// Sentinel errors —— 包对外契约。
var (
	// ErrSessionInvalid 会话不存在 / 已过期 / 归属账户已禁用（鉴权路径统一 fail-closed）。
	ErrSessionInvalid = errors.New("session: 会话无效（不存在 / 已过期 / 账户禁用）")
)

// Service 管理后台会话服务接口（计划 Unit 4；DIP：中间件 / login handler 依赖接口）。
type Service interface {
	// Create 为运维账户建会话；返回**明文** session token（写 Cookie）+ csrf token（下发前端）+ 过期时刻。
	// 明文 token 仅本次返回，库内只存其 HMAC。
	Create(ctx context.Context, operatorID int64) (token string, csrf string, expiresAt time.Time, err error)

	// Lookup 鉴权热路径：按明文 token 查活跃会话（未过期 + 账户启用）。
	// 失败（不存在 / 过期 / 禁用）→ ErrSessionInvalid。
	Lookup(ctx context.Context, token string) (*SessionContext, error)

	// Delete 登出：按明文 token 删会话；幂等（会话已不存在不报错）。
	Delete(ctx context.Context, token string) error

	// DeleteByOperator 删某运维全部会话（禁用账户 / 强制下线）；返回删除行数。
	DeleteByOperator(ctx context.Context, operatorID int64) (int64, error)

	// DeleteExpired sweep 清理过期会话；返回删除行数。
	DeleteExpired(ctx context.Context) (int64, error)
}

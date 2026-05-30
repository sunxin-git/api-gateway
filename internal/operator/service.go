package operator

import (
	"context"
	"errors"
)

// Sentinel errors —— 包对外契约。
//
// 调用方用 errors.Is 判断，不用类型断言；可用 fmt.Errorf("...: %w", Err...) 包装。
var (
	// ErrNotFound 按 id 查无账户。
	ErrNotFound = errors.New("operator: 账户不存在")

	// ErrInvalidParam 创建 / 改口令入参非法（用户名空 / 口令过短 / 过长等）。
	ErrInvalidParam = errors.New("operator: 参数非法")

	// ErrUsernameExists 用户名唯一冲突（SQLSTATE 23505）。
	ErrUsernameExists = errors.New("operator: 用户名已存在")

	// ErrAuthFailed 认证失败。**统一**覆盖「用户名不存在 / 口令错误 / 账户已禁用」三种情形，
	// 不向调用方区分（防账户枚举；ADR-0008 决策 1）。
	ErrAuthFailed = errors.New("operator: 认证失败（用户名或口令错误，或账户已禁用）")
)

// Service 运维账户服务接口（计划 Unit 3；DIP：上层 handler / bootstrap 依赖接口）。
//
// 安全契约：
//   - 返回的 OperatorAccount **绝不**含 password_hash（类型本身无该字段）；
//   - Authenticate 失败一律 ErrAuthFailed，不区分原因（防枚举）；
//   - 明文口令仅 Create / SetPassword / Authenticate 入参出现，bcrypt 后即弃，绝不入日志。
type Service interface {
	// Create 创建运维账户（bcrypt 口令入库）；返回不含哈希的视图。
	// 错误：ErrInvalidParam（入参非法）/ ErrUsernameExists（用户名冲突）/ wrapped DB error。
	Create(ctx context.Context, params CreateParams) (*OperatorAccount, error)

	// Authenticate 校验用户名 + 口令；成功返回账户视图。
	// 任一失败（不存在 / 口令错 / 禁用）→ ErrAuthFailed（不区分，防枚举 + 近似常量时间）。
	Authenticate(ctx context.Context, username, password string) (*OperatorAccount, error)

	// GetByID 按 id 查账户视图。错误：ErrNotFound。
	GetByID(ctx context.Context, id int64) (*OperatorAccount, error)

	// List 列出全部运维账户视图（不含哈希）；按 created_at DESC。
	List(ctx context.Context) ([]*OperatorAccount, error)

	// SetEnabled 启用 / 禁用账户（禁用即时阻断后续登录）。错误：ErrNotFound。
	SetEnabled(ctx context.Context, id int64, enabled bool) (*OperatorAccount, error)

	// SetPassword 重置指定账户口令（bcrypt 入库）。错误：ErrInvalidParam / ErrNotFound。
	SetPassword(ctx context.Context, id int64, newPassword string) error

	// Count 运维账户总数（初始管理员种子幂等判定用）。
	Count(ctx context.Context) (int64, error)
}

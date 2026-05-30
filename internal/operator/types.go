// Package operator 提供管理后台运维登录账户的生成、认证、生命周期管理。
//
// 计划：docs/plans/2026-05-31-001-feat-admin-console-config-plan.md Unit 3
// 决策：ADR-0008（管理后台会话认证：用户名/密码 + bcrypt + 初始管理员种子）
//
// 包内职责：
//   - operator_account CRUD（创建 / 列表 / 按 id 查 / 启停 / 改口令）
//   - 口令认证（bcrypt 比对 + 禁用账户拒登 + 防枚举）
//   - 初始管理员 env 种子（bootstrap.go；表空时幂等）
//
// 不在本包：
//   - 会话（admin_session）/ login·logout HTTP（推 internal/httpapi，Unit 4）
//   - 运维账户 Admin API handler（推 internal/admin，Unit 12）
//
// 安全红线（ADR-0008）：
//   - 口令低熵 → bcrypt 慢哈希（**非** admintoken/businesskey 的 HMAC）；
//   - password_hash 与明文**绝不**回显 / 不入日志 / 不进对外视图（OperatorAccount 无该字段）；
//   - 认证失败一律 ErrAuthFailed，**不**区分「用户名不存在 / 口令错 / 账户禁用」（防枚举）。
package operator

import "time"

// OperatorAccount 运维账户**对外视图**：**不含 password_hash**（结构上杜绝泄露）。
//
// 字段映射 schema operator_account（除 password_hash 外）。
type OperatorAccount struct {
	// ID 自增主键。
	ID int64
	// Username 登录用户名（唯一）。
	Username string
	// Enabled 软禁用；false 时拒绝登录。
	Enabled bool
	// CreatedBy 创建者：初始管理员种子为 "seed"，后台开通为 "operator:<id>"。
	CreatedBy string
	// CreatedAt 创建时刻。
	CreatedAt time.Time
	// UpdatedAt 最近更新时刻。
	UpdatedAt time.Time
}

// CreateParams Create 入参；Password 为明文，service 内 bcrypt 后入库（明文用后即弃）。
type CreateParams struct {
	// Username 必填；唯一；长度 / 字符集见 validateUsername。
	Username string
	// Password 必填明文；service 内 bcrypt；长度下限见 validatePassword。
	Password string
	// CreatedBy 必填；种子写 "seed"，后台开通写 "operator:<开通者 id>"。
	CreatedBy string
}

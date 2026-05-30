// Package session 提供管理后台会话的创建、查找、销毁（PG 存储，ADR-0008 决策 3）。
//
// 计划：docs/plans/2026-05-31-001-feat-admin-console-config-plan.md Unit 4
//
// 安全红线（ADR-0008）：
//   - 会话明文 token 仅写入 HttpOnly Cookie；库内只存 HMAC(pepper, token)（与 admintoken 同思路）；
//   - 查找按 token_hash 单 row + 校验 expires_at > NOW() + 账户 enabled（JOIN 内完成，fail-closed）；
//   - csrf_token 随会话生成，登录时下发前端，状态变更请求带 header 校验（中间件，Unit 4b）。
//
// 不在本包：
//   - login / logout HTTP（推 internal/httpapi/session_handler.go）；
//   - cookie 解析 / CSRF 校验中间件（推 internal/httpapi/middleware/admin_session_auth.go）。
package session

import "time"

// SessionContext 是一次有效会话查找的结果：会话 + 其归属运维身份（已过 enabled/未过期过滤）。
type SessionContext struct {
	// SessionID admin_session.id。
	SessionID int64
	// OperatorID 归属运维账户 id。
	OperatorID int64
	// Username 运维用户名（来自 JOIN operator_account）。
	Username string
	// CSRFToken 本会话的 CSRF token（状态变更请求须匹配）。
	CSRFToken string
	// ExpiresAt 会话过期时刻。
	ExpiresAt time.Time
}

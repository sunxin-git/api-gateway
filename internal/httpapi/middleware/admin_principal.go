package middleware

import (
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/sunxin-git/api-gateway/internal/admintoken"
)

// CtxKeyAdminPrincipal gin.Context 中归一化管理身份的 key（*AdminPrincipal）。
const CtxKeyAdminPrincipal = "admin_principal"

// PrincipalKind 管理身份来源（会话 operator vs Bearer admin_token）。
type PrincipalKind int

const (
	// PrincipalKindAdminToken Bearer Admin Token 身份（scope 受限）。
	PrincipalKindAdminToken PrincipalKind = iota
	// PrincipalKindOperator 会话登录的运维身份（单一角色，拥有全部配置能力）。
	PrincipalKindOperator
)

// AdminPrincipal 是 /admin/v1 链的**归一化管理身份**（ADR-0008 决策 4）。
//
// 由前置鉴权中间件（AdminAuth）按通道注入：
//   - 会话 Cookie → operator（OperatorID/OperatorUsername，Token 为 nil）；
//   - Bearer Token → admin_token（Token 非 nil，按 token.Scopes 校验）。
//
// 下游 Throttle / Scope / Audit 统一读本类型，而非各自读 token 验证结果。
type AdminPrincipal struct {
	Kind PrincipalKind
	// Token 仅 Kind==AdminToken 时非 nil。
	Token *admintoken.Token
	// OperatorID / OperatorUsername 仅 Kind==Operator 时有效。
	OperatorID       int64
	OperatorUsername string
}

// IsOperator 是否会话运维身份（拥有全部配置能力）。
func (p *AdminPrincipal) IsOperator() bool {
	return p != nil && p.Kind == PrincipalKindOperator
}

// AuditActor 返回审计 actor 字符串：`operator:<id>` 或 `admin_token:<id>`，nil → `anonymous`。
func (p *AdminPrincipal) AuditActor() string {
	if p == nil {
		return "anonymous"
	}
	switch p.Kind {
	case PrincipalKindOperator:
		return "operator:" + strconv.FormatInt(p.OperatorID, 10)
	case PrincipalKindAdminToken:
		if p.Token != nil {
			return "admin_token:" + strconv.FormatInt(p.Token.ID, 10)
		}
	}
	return "anonymous"
}

// SetAdminPrincipal 注入归一化身份（鉴权中间件用）。
func SetAdminPrincipal(c *gin.Context, p *AdminPrincipal) {
	c.Set(CtxKeyAdminPrincipal, p)
}

// GetAdminPrincipal 取归一化身份；不存在返 nil。
func GetAdminPrincipal(c *gin.Context) *AdminPrincipal {
	v, ok := c.Get(CtxKeyAdminPrincipal)
	if !ok {
		return nil
	}
	p, ok := v.(*AdminPrincipal)
	if !ok {
		return nil
	}
	return p
}

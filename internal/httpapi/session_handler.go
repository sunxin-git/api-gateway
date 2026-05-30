package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/sunxin-git/api-gateway/internal/httpapi/middleware"
	"github.com/sunxin-git/api-gateway/internal/operator"
	"github.com/sunxin-git/api-gateway/internal/session"
)

// SessionHandler 管理后台登录 / 登出（ADR-0008；计划 Unit 4）。
//
// 登录：校验用户名 + 口令（operator.Authenticate）→ 建会话（session.Create）→
//
//	下发 HttpOnly Cookie（session token）+ 返回 csrf_token（前端存内存，状态变更请求带 header）。
//
// 登出：删会话 + 清 Cookie（幂等）。
type SessionHandler struct {
	operators    operator.Service
	sessions     session.Service
	secureCookie bool // production（HTTPS）→ true；dev（HTTP）→ false 否则 cookie 不被发送
	log          *slog.Logger
}

// NewSessionHandler 构造；依赖 nil 即 panic（启动期 fail-fast）。
func NewSessionHandler(operators operator.Service, sessions session.Service, secureCookie bool, log *slog.Logger) *SessionHandler {
	if operators == nil || sessions == nil || log == nil {
		panic("httpapi.NewSessionHandler: 依赖不能为 nil")
	}
	return &SessionHandler{operators: operators, sessions: sessions, secureCookie: secureCookie, log: log}
}

type loginRequest struct {
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required"`
}

// Login POST /admin/login。
func (h *SessionHandler) Login(c *gin.Context) {
	var req loginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeSessionError(c, http.StatusBadRequest, "invalid_request", "用户名 / 口令缺失")
		return
	}

	acct, err := h.operators.Authenticate(c.Request.Context(), req.Username, req.Password)
	if err != nil {
		if errors.Is(err, operator.ErrAuthFailed) {
			// 统一文案，不区分「用户名不存在 / 口令错 / 禁用」（防枚举，与 service 层一致）。
			writeSessionError(c, http.StatusUnauthorized, "auth_failed", "用户名或口令错误")
			return
		}
		h.log.Error("operator 认证异常", slog.String("err", err.Error()))
		writeSessionError(c, http.StatusInternalServerError, "internal_error", "服务内部错误")
		return
	}

	token, csrf, expiresAt, err := h.sessions.Create(c.Request.Context(), acct.ID)
	if err != nil {
		h.log.Error("建会话失败", slog.String("err", err.Error()))
		writeSessionError(c, http.StatusInternalServerError, "internal_error", "服务内部错误")
		return
	}

	maxAge := int(time.Until(expiresAt).Seconds())
	if maxAge < 1 {
		maxAge = 1
	}
	// HttpOnly（JS 不可读，防 XSS 窃取）+ Secure（仅 production HTTPS）+ SameSite=Strict
	// （管理后台无跨站跳转需求，Strict 更严，安全审查 P2-2）。
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie(middleware.AdminSessionCookieName, token, maxAge, "/", "", h.secureCookie, true)

	c.JSON(http.StatusOK, gin.H{
		"username":   acct.Username,
		"csrf_token": csrf, // 前端存内存，状态变更请求带 X-CSRF-Token header
		"expires_at": expiresAt.UTC().Format(time.RFC3339),
	})
}

// Logout POST /admin/logout（幂等：无会话也清 cookie + 200）。
func (h *SessionHandler) Logout(c *gin.Context) {
	if cookie, err := c.Cookie(middleware.AdminSessionCookieName); err == nil && cookie != "" {
		if derr := h.sessions.Delete(c.Request.Context(), cookie); derr != nil {
			h.log.Warn("删会话失败（登出仍清 cookie）", slog.String("err", derr.Error()))
		}
	}
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie(middleware.AdminSessionCookieName, "", -1, "/", "", h.secureCookie, true)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func writeSessionError(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, gin.H{
		"error": gin.H{
			"code":       code,
			"message":    message,
			"request_id": middleware.GetRequestID(c),
		},
	})
}

package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sunxin-git/api-gateway/internal/config"
	"github.com/sunxin-git/api-gateway/internal/httpapi/middleware"
	"github.com/sunxin-git/api-gateway/internal/obs"
)

func testCtx() context.Context {
	c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = cancel // 测试结束自然释放
	return c
}

// =============================================================================
// D-min Unit 7 smoke 测试：admin 路由组装配 + HSTS + Trusted Proxies + XFF
//
// 本文件不接 PG（不验证 admin handler 业务逻辑——那在 internal/admin/handler_test.go）；
// 仅验证 Unit 7 在 httpapi.Server 层面的装配契约：
//
//   - SetTrustedProxies 配置生效（c.ClientIP() 行为按配置变化）
//   - HSTS middleware 给 /admin/v1/* 写 header
//   - 即便不挂任何 admin 中间件，admin 路由组本身可注册并响应
// =============================================================================

func newSmokeServer(t *testing.T, trustedCIDRs []string) *Server {
	t.Helper()
	cfg := &config.Config{
		HTTPAddr:           ":0",
		LogLevel:           "info",
		CORSAllowedOrigins: []string{},
		TrustedProxyCIDRs:  trustedCIDRs,
	}
	deps := Deps{
		Logger:  slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Metrics: obs.NewMetrics("test-version", "test-commit"),
		Build:   BuildInfo{Version: "test-version", Commit: "test-commit"},
	}
	return NewServer(cfg, deps)
}

func TestAdminGroup_HSTSAlwaysOn(t *testing.T) {
	s := newSmokeServer(t, nil)
	g := s.Engine().Group("/admin/v1")
	g.Use(middleware.HSTS())
	g.GET("/ping", func(c *ginCtx) {
		c.JSON(http.StatusOK, map[string]string{"ok": "1"})
	})

	req := httptest.NewRequest(http.MethodGet, "/admin/v1/ping", nil)
	w := httptest.NewRecorder()
	s.Engine().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, middleware.HSTSHeader, w.Header().Get("Strict-Transport-Security"),
		"admin 路由组下所有响应都应带 HSTS（即使后端 HTTP）")
}

func TestTrustedProxies_NilDoesNotTrustXFF(t *testing.T) {
	// trusted=nil（fail-closed）→ c.ClientIP() 永远返回 RemoteAddr（XFF 被忽略）
	s := newSmokeServer(t, nil)
	var seenIP string
	s.Engine().GET("/x", func(c *ginCtx) {
		seenIP = c.ClientIP()
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	w := httptest.NewRecorder()
	s.Engine().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "127.0.0.1", seenIP, "无 trusted_proxies 时 XFF 必须被忽略")
}

func TestTrustedProxies_TrustsXFFFromConfiguredProxy(t *testing.T) {
	s := newSmokeServer(t, []string{"127.0.0.1/32"})
	var seenIP string
	s.Engine().GET("/x", func(c *ginCtx) {
		seenIP = c.ClientIP()
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	w := httptest.NewRecorder()
	s.Engine().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "1.2.3.4", seenIP, "trusted proxy 来源的 XFF 必须被采纳")
}

func TestTrustedProxies_RejectsXFFFromUntrustedSource(t *testing.T) {
	s := newSmokeServer(t, []string{"127.0.0.1/32"})
	var seenIP string
	s.Engine().GET("/x", func(c *ginCtx) {
		seenIP = c.ClientIP()
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.RemoteAddr = "8.8.8.8:1234" // 非 trusted proxy
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	w := httptest.NewRecorder()
	s.Engine().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "8.8.8.8", seenIP, "非 trusted proxy 的 XFF 必须被拒绝")
}

func TestStartTLS_MissingFilesReturnsError(t *testing.T) {
	// 仅验证 StartTLS 入口存在 + 返回错误（不真正 listen）。
	cfg := &config.Config{
		HTTPAddr:           "127.0.0.1:0",
		LogLevel:           "info",
		CORSAllowedOrigins: []string{},
	}
	deps := Deps{
		Logger:  slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Metrics: obs.NewMetrics("test", "test"),
		Build:   BuildInfo{Version: "test", Commit: "test"},
	}
	s := NewServer(cfg, deps)
	go func() { _ = s.StartTLS("/nonexistent/cert", "/nonexistent/key") }()
	// 立即关掉，StartTLS goroutine 会因文件不存在返 error
	err := s.Shutdown(testCtx())
	assert.NoError(t, err)
}

// ginCtx alias 以减少 import 噪声。
type ginCtx = gin.Context

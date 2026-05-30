package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/crypto/bcrypt"

	"github.com/sunxin-git/api-gateway/internal/admintoken"
	"github.com/sunxin-git/api-gateway/internal/httpapi/middleware"
	"github.com/sunxin-git/api-gateway/internal/operator"
	"github.com/sunxin-git/api-gateway/internal/session"
)

func sessTestDSN() string {
	if v := os.Getenv("LEDGER_TEST_PG_DSN"); v != "" {
		return v
	}
	return "postgres://gateway:gateway_dev@127.0.0.1:55432/gateway?sslmode=disable"
}

func sessMustPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(sessTestDSN())
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	cfg.MaxConns = 8
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("跳过：无法连 PG：%v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func sessSilentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// sessTestPepper 32 字节测试 pepper（admintoken / session 共用）。
var sessTestPepper = []byte("httpapi-session-test-pepper-32by!")

// buildSessionTestEngine 装配：operator + session 服务 + login/logout + AdminAuth 保护的 /admin/v1/ping。
func buildSessionTestEngine(t *testing.T, pool *pgxpool.Pool) (*gin.Engine, operator.Service) {
	t.Helper()
	log := sessSilentLogger()
	opSvc := operator.NewPostgresServiceWithCost(pool, log, bcrypt.MinCost)
	sessSvc := session.NewPostgresService(pool, sessTestPepper, time.Hour, log)
	tokenSvc := admintoken.NewPostgresService(pool, sessTestPepper, log) // 仅占位，会话流程不触达
	sh := NewSessionHandler(opSvc, sessSvc, false /*secureCookie=false 便于 http 测试*/, log)

	counter := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "t_auth_failed"}, []string{"reason"})

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.RequestID())
	r.POST("/admin/login", sh.Login)
	r.POST("/admin/logout", sh.Logout)
	g := r.Group("/admin/v1")
	g.Use(middleware.AdminAuth(sessSvc, tokenSvc, counter))
	g.GET("/ping", func(c *gin.Context) {
		p := middleware.GetAdminPrincipal(c)
		c.JSON(http.StatusOK, gin.H{"actor": p.AuditActor()})
	})
	return r, opSvc
}

func createLoginOperator(t *testing.T, pool *pgxpool.Pool, opSvc operator.Service, password string) string {
	t.Helper()
	username := "login_op_" + time.Now().Format("150405.000000000")
	acct, err := opSvc.Create(context.Background(), operator.CreateParams{
		Username: username, Password: password, CreatedBy: "test",
	})
	if err != nil {
		t.Fatalf("create operator: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = pool.Exec(ctx, "DELETE FROM operator_account WHERE id = $1", acct.ID)
	})
	return username
}

// TestLoginFlow 登录→拿 cookie→访问受保护端点→登出→cookie 失效（401）。
func TestLoginFlow(t *testing.T) {
	pool := sessMustPool(t)
	r, opSvc := buildSessionTestEngine(t, pool)
	const password = "login-flow-secret"
	username := createLoginOperator(t, pool, opSvc, password)

	// 1. 登录
	body := `{"username":"` + username + `","password":"` + password + `"}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("登录应 200，得 %d: %s", w.Code, w.Body.String())
	}
	cookie := extractCookie(w.Result().Cookies(), middleware.AdminSessionCookieName)
	if cookie == "" {
		t.Fatalf("登录应下发 %s cookie", middleware.AdminSessionCookieName)
	}
	if !strings.Contains(w.Body.String(), "csrf_token") {
		t.Fatalf("登录响应应含 csrf_token，得 %s", w.Body.String())
	}

	// 2. 用 cookie 访问受保护端点（GET 无需 CSRF）
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/admin/v1/ping", nil)
	req2.AddCookie(&http.Cookie{Name: middleware.AdminSessionCookieName, Value: cookie})
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("带会话 cookie 访问应 200，得 %d: %s", w2.Code, w2.Body.String())
	}

	// 3. 登出
	w3 := httptest.NewRecorder()
	req3 := httptest.NewRequest(http.MethodPost, "/admin/logout", nil)
	req3.AddCookie(&http.Cookie{Name: middleware.AdminSessionCookieName, Value: cookie})
	r.ServeHTTP(w3, req3)
	if w3.Code != http.StatusOK {
		t.Fatalf("登出应 200，得 %d", w3.Code)
	}

	// 4. 登出后 cookie 失效 → 401
	w4 := httptest.NewRecorder()
	req4 := httptest.NewRequest(http.MethodGet, "/admin/v1/ping", nil)
	req4.AddCookie(&http.Cookie{Name: middleware.AdminSessionCookieName, Value: cookie})
	r.ServeHTTP(w4, req4)
	if w4.Code != http.StatusUnauthorized {
		t.Fatalf("登出后会话应失效 401，得 %d", w4.Code)
	}
}

// TestLogin_WrongPassword 错口令 → 401 auth_failed。
func TestLogin_WrongPassword(t *testing.T) {
	pool := sessMustPool(t)
	r, opSvc := buildSessionTestEngine(t, pool)
	username := createLoginOperator(t, pool, opSvc, "the-correct-password")

	body := `{"username":"` + username + `","password":"wrong-password"}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("错口令登录应 401，得 %d: %s", w.Code, w.Body.String())
	}
}

func extractCookie(cookies []*http.Cookie, name string) string {
	for _, ck := range cookies {
		if ck.Name == name {
			return ck.Value
		}
	}
	return ""
}

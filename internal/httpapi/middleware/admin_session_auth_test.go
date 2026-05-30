package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/sunxin-git/api-gateway/internal/admintoken"
	"github.com/sunxin-git/api-gateway/internal/session"
)

// fakeSessionService 实现 session.Service，仅 Lookup 可注入（AdminAuth 只用 Lookup）。
type fakeSessionService struct {
	lookupFn func(token string) (*session.SessionContext, error)
}

func (f *fakeSessionService) Create(_ context.Context, _ int64) (string, string, time.Time, error) {
	return "", "", time.Time{}, session.ErrSessionInvalid
}
func (f *fakeSessionService) Lookup(_ context.Context, token string) (*session.SessionContext, error) {
	if f.lookupFn != nil {
		return f.lookupFn(token)
	}
	return nil, session.ErrSessionInvalid
}
func (f *fakeSessionService) Delete(_ context.Context, _ string) error { return nil }
func (f *fakeSessionService) DeleteByOperator(_ context.Context, _ int64) (int64, error) {
	return 0, nil
}
func (f *fakeSessionService) DeleteExpired(_ context.Context) (int64, error) { return 0, nil }

const testSessionCookie = "good-session-token"
const testCSRF = "csrf-token-xyz"

// validSessionSvc 返回固定有效会话（token == testSessionCookie）。
func validSessionSvc() *fakeSessionService {
	return &fakeSessionService{lookupFn: func(token string) (*session.SessionContext, error) {
		if token == testSessionCookie {
			return &session.SessionContext{
				OperatorID: 7,
				Username:   "op-test",
				CSRFToken:  testCSRF,
				ExpiresAt:  time.Now().Add(time.Hour),
			}, nil
		}
		return nil, session.ErrSessionInvalid
	}}
}

func newAdminAuthEngine(sess session.Service, tok admintoken.Service) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(RequestID())
	g := r.Group("/admin/v1")
	g.Use(AdminAuth(sess, tok, newAuthFailedCounter()))
	g.GET("/ping", func(c *gin.Context) {
		p := GetAdminPrincipal(c)
		c.JSON(http.StatusOK, gin.H{"actor": p.AuditActor()})
	})
	g.POST("/do", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	return r
}

func doReq(r *gin.Engine, method, path string, cookie, csrf, bearer string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: AdminSessionCookieName, Value: cookie})
	}
	if csrf != "" {
		req.Header.Set(CSRFHeaderName, csrf)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestAdminAuth_SessionGet 有效会话 GET（无需 CSRF）→ 200 + operator principal。
func TestAdminAuth_SessionGet(t *testing.T) {
	r := newAdminAuthEngine(validSessionSvc(), &fakeAdminService{})
	w := doReq(r, http.MethodGet, "/admin/v1/ping", testSessionCookie, "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("有效会话 GET 应 200，得 %d: %s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); !strings.Contains(got, "operator:7") {
		t.Fatalf("actor 应为 operator:7，得 %s", got)
	}
}

// TestAdminAuth_SessionPostCSRF 状态变更：无 CSRF→403；错 CSRF→403；对 CSRF→200。
func TestAdminAuth_SessionPostCSRF(t *testing.T) {
	r := newAdminAuthEngine(validSessionSvc(), &fakeAdminService{})

	if w := doReq(r, http.MethodPost, "/admin/v1/do", testSessionCookie, "", ""); w.Code != http.StatusForbidden {
		t.Fatalf("POST 无 CSRF 应 403，得 %d", w.Code)
	}
	if w := doReq(r, http.MethodPost, "/admin/v1/do", testSessionCookie, "wrong-csrf", ""); w.Code != http.StatusForbidden {
		t.Fatalf("POST 错 CSRF 应 403，得 %d", w.Code)
	}
	if w := doReq(r, http.MethodPost, "/admin/v1/do", testSessionCookie, testCSRF, ""); w.Code != http.StatusOK {
		t.Fatalf("POST 对 CSRF 应 200，得 %d: %s", w.Code, w.Body.String())
	}
}

// TestAdminAuth_InvalidSession_NoBearer 无效会话且无 Bearer → 401。
func TestAdminAuth_InvalidSession_NoBearer(t *testing.T) {
	r := newAdminAuthEngine(validSessionSvc(), &fakeAdminService{})
	w := doReq(r, http.MethodGet, "/admin/v1/ping", "expired-or-bad", "", "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("无效会话无 Bearer 应 401，得 %d", w.Code)
	}
}

// TestAdminAuth_BearerFallback 无效/无会话 + 有效 Bearer → 200（admin_token principal）。
func TestAdminAuth_BearerFallback(t *testing.T) {
	tokenSvc := &fakeAdminService{validateF: func(plaintext string, _ netip.Addr) (*admintoken.ValidationResult, error) {
		if plaintext == "sk-valid" {
			return &admintoken.ValidationResult{Token: &admintoken.Token{ID: 42, Scopes: []string{"x"}}}, nil
		}
		return nil, admintoken.ErrTokenNotFound
	}}
	r := newAdminAuthEngine(validSessionSvc(), tokenSvc)

	// 无效会话 cookie + 有效 Bearer → 落到 Bearer
	w := doReq(r, http.MethodGet, "/admin/v1/ping", "bad-cookie", "", "sk-valid")
	if w.Code != http.StatusOK {
		t.Fatalf("无效会话 + 有效 Bearer 应 200，得 %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "admin_token:42") {
		t.Fatalf("actor 应为 admin_token:42，得 %s", w.Body.String())
	}

	// 无 cookie + 有效 Bearer → 200
	if w := doReq(r, http.MethodGet, "/admin/v1/ping", "", "", "sk-valid"); w.Code != http.StatusOK {
		t.Fatalf("无 cookie + 有效 Bearer 应 200，得 %d", w.Code)
	}
	// 无 cookie + 无 Bearer → 401
	if w := doReq(r, http.MethodGet, "/admin/v1/ping", "", "", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("无 cookie 无 Bearer 应 401，得 %d", w.Code)
	}
}

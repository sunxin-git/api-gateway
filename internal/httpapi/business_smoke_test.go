package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sunxin-git/api-gateway/internal/businesskey"
	"github.com/sunxin-git/api-gateway/internal/httpapi/middleware"
	"github.com/sunxin-git/api-gateway/internal/relay"
)

// =============================================================================
// F-min Unit 7 smoke 测试：业务 relay 路由组 /v1 装配契约
//
// 本文件不接 PG（不验证 relay handler 的 Reserve/Settle 业务逻辑——那在
// internal/relay/handler_test.go）；仅验证 Unit 7 在 httpapi.Server 层面的装配契约：
//
//   - 业务中间件链顺序：HSTS → BodyLimit → KeyAuth → RPM → Audit → handler
//   - 缺 Authorization / 无效 key → 401 OpenAI 兼容错误（业务 SDK 可解析）
//   - 鉴权通过后请求可达终端 handler
//   - /v1 路由组下所有响应都带 HSTS header
//
// body 超 1 MiB → 413 由 BusinessBodyLimit 中间件单测覆盖
// （middleware/business_middleware_test.go TestBusinessBodyLimit_Over1MB_Rejects413）；
// 此处不重复（真实 relay handler 的 body 读取语义与 stub 不同，重复测会给误导性信心）。
// =============================================================================

// fakeBizKeyService 实现 businesskey.Service；ValidateByPlaintext 按 plaintext 决定命中。
//
// "valid-biz-key" → 返回 ValidationResult（含 key.ID=1）；其余 → ErrKeyNotFound。
// 其他方法非本 smoke 关心，返回零值 / nil。
type fakeBizKeyService struct{}

func (fakeBizKeyService) Create(_ context.Context, _ businesskey.CreateParams) (*businesskey.Key, string, error) {
	return nil, "", nil
}

func (fakeBizKeyService) ValidateByPlaintext(_ context.Context, plaintext string) (*businesskey.ValidationResult, error) {
	if plaintext == "valid-biz-key" {
		return &businesskey.ValidationResult{
			Key: &businesskey.Key{ID: 1, BusinessAccountID: "acct-smoke"},
		}, nil
	}
	return nil, businesskey.ErrKeyNotFound
}

func (fakeBizKeyService) Revoke(_ context.Context, _ int64) (bool, error)              { return false, nil }
func (fakeBizKeyService) ListByAccount(_ context.Context, _ string) ([]*businesskey.Key, error) {
	return nil, nil
}
func (fakeBizKeyService) ListAll(_ context.Context) ([]*businesskey.Key, error) { return nil, nil }
func (fakeBizKeyService) GetByID(_ context.Context, _ int64) (*businesskey.Key, error) {
	return nil, businesskey.ErrKeyNotFound
}
func (fakeBizKeyService) TouchLastUsed(_ context.Context, _ int64) error { return nil }
func (fakeBizKeyService) Close() error                                   { return nil }

// newBusinessSmokeServer 组装 server + /v1 业务链（与 main.registerBusinessRoutes 同顺序），
// 终端 handler 用 stub（返 200），避免引 ledger / catalog / adapter（保持 no-PG）。
func newBusinessSmokeServer(t *testing.T) (*Server, func()) {
	t.Helper()
	s := newSmokeServer(t, nil)
	m := s.deps.Metrics // NewServer 注入的 obs.Metrics

	rpm := businesskey.NewInProcessRPM(slog.New(slog.NewJSONHandler(io.Discard, nil)), nil)

	g := s.Engine().Group("/v1")
	g.Use(
		middleware.HSTS(),
		middleware.BusinessBodyLimit(m.BusinessAPIBodyTooLargeTotal),
		middleware.BusinessKeyAuth(fakeBizKeyService{}, m.BusinessAPIAuthFailedTotal),
		middleware.BusinessRPM(rpm, m.BusinessAPIRateLimitedTotal),
	)
	g.POST("/chat/completions", func(c *ginCtx) {
		// stub 终端：鉴权 + RPM 通过才会到这里
		c.JSON(http.StatusOK, map[string]string{"reached": "handler"})
	})

	return s, func() { _ = rpm.Close() }
}

func doBizPost(s *Server, auth, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	w := httptest.NewRecorder()
	s.Engine().ServeHTTP(w, req)
	return w
}

// decodeErrType 取 OpenAI 兼容错误响应的 error.type。
func decodeErrType(t *testing.T, body []byte) (errType, code string) {
	t.Helper()
	var resp relay.ErrorResponse
	require.NoError(t, json.Unmarshal(body, &resp), "响应体应是 OpenAI 兼容错误 JSON：%s", body)
	return resp.Error.Type, resp.Error.Code
}

func TestBusinessGroup_MissingAuth_Returns401(t *testing.T) {
	s, cleanup := newBusinessSmokeServer(t)
	defer cleanup()

	w := doBizPost(s, "", `{"model":"gw-default","messages":[{"role":"user","content":"hi"}]}`)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	errType, _ := decodeErrType(t, w.Body.Bytes())
	assert.Equal(t, relay.ErrTypeInvalidAPIKey, errType)
	assert.Equal(t, middleware.HSTSHeader, w.Header().Get("Strict-Transport-Security"),
		"/v1 路由组下错误响应也应带 HSTS")
}

func TestBusinessGroup_InvalidKey_Returns401(t *testing.T) {
	s, cleanup := newBusinessSmokeServer(t)
	defer cleanup()

	w := doBizPost(s, "Bearer wrong-key", `{"model":"gw-default","messages":[]}`)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	errType, code := decodeErrType(t, w.Body.Bytes())
	assert.Equal(t, relay.ErrTypeInvalidAPIKey, errType)
	assert.Equal(t, "invalid_api_key", code)
}

func TestBusinessGroup_ValidKey_ReachesHandler(t *testing.T) {
	s, cleanup := newBusinessSmokeServer(t)
	defer cleanup()

	w := doBizPost(s, "Bearer valid-biz-key", `{"model":"gw-default","messages":[{"role":"user","content":"hi"}]}`)
	require.Equal(t, http.StatusOK, w.Code, "鉴权 + RPM 通过应到达终端 handler；body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), "handler")
	assert.Equal(t, middleware.HSTSHeader, w.Header().Get("Strict-Transport-Security"))
}

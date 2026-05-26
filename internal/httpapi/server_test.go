package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sunxin-git/api-gateway/internal/config"
	"github.com/sunxin-git/api-gateway/internal/obs"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	cfg := &config.Config{
		HTTPAddr:           ":0",
		LogLevel:           "info",
		CORSAllowedOrigins: []string{},
	}
	deps := Deps{
		Logger:  slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Metrics: obs.NewMetrics("test-version", "test-commit"),
		Build:   BuildInfo{Version: "test-version", Commit: "test-commit"},
	}
	return NewServer(cfg, deps)
}

func TestHealthz返回JSON包含版本信息(t *testing.T) {
	s := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	s.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "ok", body["status"])
	assert.Equal(t, "test-version", body["version"])
	assert.Equal(t, "test-commit", body["commit"])
}

func TestReadyz无check时返回200(t *testing.T) {
	s := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	s.engine.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "ready", body["status"])
}

func TestReadyz_check失败时返回503(t *testing.T) {
	s := newTestServer(t)
	s.AddReadinessCheck("db", func(_ context.Context) error {
		return errors.New("DB 不可达")
	})
	s.AddReadinessCheck("redis", func(_ context.Context) error {
		return nil
	})

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	s.engine.ServeHTTP(w, req)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "not_ready", body["status"])
	checks := body["checks"].(map[string]any)
	assert.Contains(t, checks["db"], "DB 不可达")
	assert.Equal(t, "ok", checks["redis"])
}

func TestMetrics端点返回prometheus格式(t *testing.T) {
	s := newTestServer(t)
	// 触发一次请求让 histogram 至少有一个观测值。
	req0 := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w0 := httptest.NewRecorder()
	s.engine.ServeHTTP(w0, req0)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	s.engine.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	// Prometheus exposition format 关键 marker
	assert.True(t, strings.Contains(body, "gateway_http_request_duration_seconds"),
		"应包含项目自有 metric")
	assert.True(t, strings.Contains(body, "gateway_build_info"))
	// 不应包含 go_ / process_ 等默认 metric（自有 registry）
	assert.False(t, strings.Contains(body, "go_goroutines"),
		"自有 registry 不应携带 client_golang 默认 metric")
}

func TestMiddleware链顺序_RequestID头被回写(t *testing.T) {
	s := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	s.engine.ServeHTTP(w, req)
	assert.NotEmpty(t, w.Header().Get("X-Request-Id"))
}

func TestGracefulShutdown在SIGTERM后退出(t *testing.T) {
	// 用 :0 让 OS 分配端口，避免与并发测试冲突
	cfg := &config.Config{
		HTTPAddr:           "127.0.0.1:0",
		LogLevel:           "info",
		CORSAllowedOrigins: []string{},
	}
	deps := Deps{
		Logger:  slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Metrics: obs.NewMetrics("test-version", "test-commit"),
		Build:   BuildInfo{Version: "test-version", Commit: "test-commit"},
	}
	s := NewServer(cfg, deps)

	// 替换 listener 让我们能拿到实际端口
	ln, err := net.Listen("tcp", cfg.HTTPAddr)
	require.NoError(t, err)
	s.httpSrv.Addr = ln.Addr().String()

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.httpSrv.Serve(ln)
	}()

	// 等 server 进入 Serve 状态
	time.Sleep(50 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	shutdownErr := s.Shutdown(ctx)
	require.NoError(t, shutdownErr)

	select {
	case err := <-errCh:
		// Serve 在 Shutdown 后返回 http.ErrServerClosed
		assert.ErrorIs(t, err, http.ErrServerClosed)
	case <-time.After(5 * time.Second):
		t.Fatal("Server 未在 5s 内退出")
	}
}

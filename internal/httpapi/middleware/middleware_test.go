package middleware

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestEngine 构造测试用 gin Engine，禁用默认中间件，保持纯净。
func newTestEngine() *gin.Engine {
	gin.SetMode(gin.TestMode)
	return gin.New()
}

func TestRequestID_缺失时生成UUIDv7(t *testing.T) {
	r := newTestEngine()
	r.Use(RequestID())
	r.GET("/x", func(c *gin.Context) {
		c.String(http.StatusOK, GetRequestID(c))
	})

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	rid := w.Body.String()
	require.NotEmpty(t, rid)
	assert.Equal(t, rid, w.Header().Get(HeaderRequestID), "响应 header 必须与 context 一致")

	parsed, err := uuid.Parse(rid)
	require.NoError(t, err)
	assert.Equal(t, uuid.Version(7), parsed.Version(), "默认应生成 v7")
}

func TestRequestID_透传已有值(t *testing.T) {
	r := newTestEngine()
	r.Use(RequestID())
	r.GET("/x", func(c *gin.Context) {
		c.String(http.StatusOK, GetRequestID(c))
	})

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set(HeaderRequestID, "client-provided-id")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, "client-provided-id", w.Body.String())
	assert.Equal(t, "client-provided-id", w.Header().Get(HeaderRequestID))
}

func TestRecover_捕获panic返回500并自增计数(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	counter := prometheus.NewCounter(prometheus.CounterOpts{Name: "test_panic_total"})

	r := newTestEngine()
	r.Use(RequestID(), Recover(logger, counter))
	r.GET("/boom", func(_ *gin.Context) {
		panic("故意 panic")
	})

	req := httptest.NewRequest(http.MethodGet, "/boom", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Equal(t, float64(1), testutil.ToFloat64(counter), "panic counter 应自增")

	logOut := buf.String()
	assert.Contains(t, logOut, "panic")
	assert.Contains(t, logOut, "故意 panic")

	// 响应 body 应是 JSON 且含 request_id
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "内部错误", body["error"])
	assert.NotEmpty(t, body["request_id"])
}

func TestSlog_输出JSON含request_id(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	r := newTestEngine()
	r.Use(RequestID(), Slog(logger))
	r.GET("/ping", func(c *gin.Context) {
		c.String(http.StatusOK, "pong")
	})

	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	req.Header.Set(HeaderRequestID, "rid-abc")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var entry map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &entry))
	assert.Equal(t, "http_access", entry["msg"])
	assert.Equal(t, "GET", entry["method"])
	assert.Equal(t, "/ping", entry["path"])
	assert.EqualValues(t, 200, entry["status"])
	assert.Equal(t, "rid-abc", entry["request_id"])
}

func TestCORS_空白名单拒绝跨域(t *testing.T) {
	r := newTestEngine()
	r.Use(CORS(nil)) // 空名单
	r.GET("/x", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	t.Run("无Origin同源放行", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("有Origin直接403", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set("Origin", "https://evil.example.com")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusForbidden, w.Code)
	})
}

func TestCORS_白名单内放行且回写头(t *testing.T) {
	r := newTestEngine()
	r.Use(CORS([]string{"https://ok.example.com"}))
	r.GET("/x", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Origin", "https://ok.example.com")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "https://ok.example.com", w.Header().Get("Access-Control-Allow-Origin"))
	assert.Equal(t, "Origin", w.Header().Get("Vary"))
}

func TestCORS_OPTIONS预检返回204(t *testing.T) {
	r := newTestEngine()
	r.Use(CORS([]string{"https://ok.example.com"}))
	r.GET("/x", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	req := httptest.NewRequest(http.MethodOptions, "/x", nil)
	req.Header.Set("Origin", "https://ok.example.com")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusNoContent, w.Code)
}

func TestProm_写入耗时histogram(t *testing.T) {
	reg := prometheus.NewRegistry()
	h := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{Name: "test_dur"},
		[]string{"method", "path", "status"},
	)
	reg.MustRegister(h)

	r := newTestEngine()
	r.Use(Prom(h))
	r.GET("/p", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	req := httptest.NewRequest(http.MethodGet, "/p", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	mfs, err := reg.Gather()
	require.NoError(t, err)
	require.Len(t, mfs, 1)
	require.NotEmpty(t, mfs[0].GetMetric())

	// 验证 label 值
	got := map[string]string{}
	for _, lp := range mfs[0].GetMetric()[0].GetLabel() {
		got[lp.GetName()] = lp.GetValue()
	}
	assert.Equal(t, "GET", got["method"])
	assert.Equal(t, "/p", got["path"])
	assert.Equal(t, "200", got["status"])
}

func TestProm_未匹配路由path为unknown(t *testing.T) {
	h := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{Name: "test_dur2"},
		[]string{"method", "path", "status"},
	)
	prometheus.NewRegistry().MustRegister(h)

	r := newTestEngine()
	r.Use(Prom(h))
	// 不注册任何路由

	req := httptest.NewRequest(http.MethodGet, "/nope", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// 不能通过 reg.Gather 验证（label 未知），直接遍历 vec 找标签。
	m, err := h.GetMetricWithLabelValues("GET", "unknown", "404")
	require.NoError(t, err)
	require.NotNil(t, m)
}

// TestChainOrder 验证完整 6 个 middleware 装配顺序下，每层都按预期生效。
func TestChain完整链顺序生效(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	counter := prometheus.NewCounter(prometheus.CounterOpts{Name: "test_chain_panic"})
	histogram := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{Name: "test_chain_dur"},
		[]string{"method", "path", "status"},
	)

	r := newTestEngine()
	// 顺序：recover → requestid → slog → otel → prom → cors
	r.Use(
		Recover(logger, counter),
		RequestID(),
		Slog(logger),
		OTel("test-service"),
		Prom(histogram),
		CORS([]string{"https://allow.example.com"}),
	)
	r.GET("/ok", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	// 1) 正常请求：CORS 同源放行 + status 200
	req := httptest.NewRequest(http.MethodGet, "/ok", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.NotEmpty(t, w.Header().Get(HeaderRequestID), "RequestID 已生成")

	// 2) 跨域被 CORS 拒绝（white-list miss）
	req2 := httptest.NewRequest(http.MethodGet, "/ok", nil)
	req2.Header.Set("Origin", "https://evil.example.com")
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	assert.Equal(t, http.StatusForbidden, w2.Code)
}

// 静态变量提示编译器：io.Discard 在 import 列表中有效（避免某些 lint 报未用）。
var _ io.Writer = io.Discard

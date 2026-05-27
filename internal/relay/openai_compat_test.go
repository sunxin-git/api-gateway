package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// 测试 helpers
// =============================================================================

// newTestAdapter 构造 adapter + mock 上游 server；返回 adapter + entry 指向 mock + cleanup。
func newTestAdapter(t *testing.T, handler http.HandlerFunc) (*OpenAICompatAdapter, *ModelEntry, func()) {
	t.Helper()
	srv := httptest.NewServer(handler)
	entry := &ModelEntry{
		GatewayModelName:      "gw-default",
		UpstreamProviderType:  "openai_compat",
		UpstreamBaseURL:       srv.URL,
		UpstreamAPIKey:        "test-key",
		UpstreamModelName:     "upstream-real-model",
		PriceInputPer1MMinor:  800,
		PriceOutputPer1MMinor: 2000,
		MaxContextTokens:      32768,
	}
	client := &http.Client{Timeout: 5 * time.Second}
	adapter := NewOpenAICompatAdapter(client)
	cleanup := func() { srv.Close() }
	return adapter, entry, cleanup
}

func sampleRequestBody() map[string]any {
	return map[string]any{
		"model": "gw-default",
		"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
		},
		"temperature": 0.7,
	}
}

// =============================================================================
// Happy path
// =============================================================================

func TestChatCompletion_Happy_WithUsage(t *testing.T) {
	// mock 上游验证：(1) Authorization 用 upstream key (2) Content-Type JSON
	// (3) body 的 model 被改写为 upstream 真实名 (4) 其他字段透传
	var seenAuth string
	var seenBody map[string]any
	adapter, entry, cleanup := newTestAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&seenBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
  "id": "chatcmpl-xxx",
  "object": "chat.completion",
  "model": "upstream-real-model",
  "choices": [{"message": {"role": "assistant", "content": "hi"}}],
  "usage": {"prompt_tokens": 10, "completion_tokens": 20, "total_tokens": 30}
}`))
	})
	defer cleanup()

	resp, err := adapter.ChatCompletion(context.Background(), entry, sampleRequestBody())
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	require.NotNil(t, resp.Usage)
	assert.Equal(t, 10, resp.Usage.PromptTokens)
	assert.Equal(t, 20, resp.Usage.CompletionTokens)
	assert.Equal(t, 30, resp.Usage.TotalTokens)

	// 透传断言：上游响应 body 整体保留
	assert.Contains(t, string(resp.Body), "chatcmpl-xxx")
	assert.Contains(t, string(resp.Body), `"role": "assistant"`)

	// 请求改写断言
	assert.Equal(t, "Bearer test-key", seenAuth, "上游 Authorization 应用 upstream key")
	assert.Equal(t, "upstream-real-model", seenBody["model"], "model 字段应改写为 UpstreamModelName")
	assert.Equal(t, float64(0.7), seenBody["temperature"], "其他字段（temperature）应透传")
	require.NotNil(t, seenBody["messages"])
}

func TestChatCompletion_FieldsPassthrough(t *testing.T) {
	// 验证 tools / response_format / tool_choice 等高级字段透传
	var seenBody map[string]any
	adapter, entry, cleanup := newTestAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&seenBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"usage": {"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	})
	defer cleanup()

	req := map[string]any{
		"model":           "gw-default",
		"messages":        []any{map[string]any{"role": "user", "content": "x"}},
		"tools":           []any{map[string]any{"type": "function", "function": map[string]any{"name": "test"}}},
		"tool_choice":     "auto",
		"response_format": map[string]any{"type": "json_object"},
		"top_p":           0.9,
	}
	_, err := adapter.ChatCompletion(context.Background(), entry, req)
	require.NoError(t, err)

	assert.Equal(t, "auto", seenBody["tool_choice"])
	assert.Equal(t, float64(0.9), seenBody["top_p"])
	assert.NotNil(t, seenBody["tools"])
	assert.NotNil(t, seenBody["response_format"])
}

func TestChatCompletion_BusinessHeadersNotForwarded(t *testing.T) {
	// plan §决策 D3：业务侧 header 不转发上游（PII 防护）
	// 当前 adapter 直接构造新 request，不复制业务侧 header；本测试验证 mock 上游
	// 收到的请求**仅**含 Authorization + Content-Type，无业务侧 X-Forwarded-For 等
	var seenHeaders http.Header
	adapter, entry, cleanup := newTestAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		seenHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	})
	defer cleanup()

	_, err := adapter.ChatCompletion(context.Background(), entry, sampleRequestBody())
	require.NoError(t, err)

	// 上游必须收到 Authorization 和 Content-Type
	assert.NotEmpty(t, seenHeaders.Get("Authorization"))
	assert.Contains(t, seenHeaders.Get("Content-Type"), "application/json")
	// 业务侧常见 header 不应出现（本测试调用方就没传，但确认 adapter 不会添加）
	assert.Empty(t, seenHeaders.Get("X-Forwarded-For"))
	assert.Empty(t, seenHeaders.Get("Cookie"))
}

// =============================================================================
// 200 但 usage 缺失
// =============================================================================

func TestChatCompletion_200ButNoUsage_ReturnsNilUsage(t *testing.T) {
	adapter, entry, cleanup := newTestAdapter(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-no-usage","choices":[]}`))
	})
	defer cleanup()

	resp, err := adapter.ChatCompletion(context.Background(), entry, sampleRequestBody())
	require.NoError(t, err, "缺 usage 不应作为 error；handler 兜底处理")
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Nil(t, resp.Usage, "缺 usage 时返 nil；handler 决策（plan §决策 D2）")
}

// =============================================================================
// 200 但非 JSON
// =============================================================================

func TestChatCompletion_200ButNonJSON_ErrUpstreamMalformed(t *testing.T) {
	adapter, entry, cleanup := newTestAdapter(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<html>not json</html>`))
	})
	defer cleanup()

	_, err := adapter.ChatCompletion(context.Background(), entry, sampleRequestBody())
	require.Error(t, err)
	require.ErrorIs(t, err, ErrUpstreamMalformed)
}

// =============================================================================
// 4xx / 5xx 透传
// =============================================================================

func TestChatCompletion_400Passthrough(t *testing.T) {
	body := `{"error":{"message":"invalid model","type":"invalid_request_error"}}`
	adapter, entry, cleanup := newTestAdapter(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(body))
	})
	defer cleanup()

	resp, err := adapter.ChatCompletion(context.Background(), entry, sampleRequestBody())
	require.NoError(t, err, "4xx 不作为 error，由 handler 透传")
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Equal(t, body, string(resp.Body))
	assert.Nil(t, resp.Usage, "非 200 不解析 usage")
}

func TestChatCompletion_500Passthrough(t *testing.T) {
	adapter, entry, cleanup := newTestAdapter(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"internal"}}`))
	})
	defer cleanup()

	resp, err := adapter.ChatCompletion(context.Background(), entry, sampleRequestBody())
	require.NoError(t, err, "5xx 不作为 error；handler 决策（plan D2：→ 502 upstream_5xx）")
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
}

func TestChatCompletion_429Passthrough(t *testing.T) {
	// 上游限速 429 — 透传业务方
	adapter, entry, cleanup := newTestAdapter(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream rate limit"}}`))
	})
	defer cleanup()

	resp, err := adapter.ChatCompletion(context.Background(), entry, sampleRequestBody())
	require.NoError(t, err)
	assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
}

// =============================================================================
// Timeout
// =============================================================================

func TestChatCompletion_Timeout_ErrUpstreamTimeout(t *testing.T) {
	// 上游 sleep > client timeout
	adapter, entry, cleanup := newTestAdapter(t, func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(3 * time.Second)
		w.WriteHeader(http.StatusOK)
	})
	defer cleanup()

	// 覆盖 client 用更短 timeout
	adapter.client = &http.Client{Timeout: 100 * time.Millisecond}

	_, err := adapter.ChatCompletion(context.Background(), entry, sampleRequestBody())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUpstreamTimeout)
}

func TestChatCompletion_ContextCanceled_ErrUpstreamTimeout(t *testing.T) {
	// 业务方断开 → ctx 取消传播到 adapter
	adapter, entry, cleanup := newTestAdapter(t, func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(1 * time.Second)
		w.WriteHeader(http.StatusOK)
	})
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err := adapter.ChatCompletion(ctx, entry, sampleRequestBody())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUpstreamTimeout, "ctx canceled 归入 timeout 分支（plan §D2）")
}

// =============================================================================
// Unreachable
// =============================================================================

func TestChatCompletion_ConnectionRefused_ErrUpstreamUnreachable(t *testing.T) {
	// 用关闭端口模拟 connection refused
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	ln.Close() // 立即关；后续 connect 必失败

	entry := &ModelEntry{
		UpstreamBaseURL:       "http://" + addr,
		UpstreamAPIKey:        "x",
		UpstreamModelName:     "m",
		PriceInputPer1MMinor:  1,
		PriceOutputPer1MMinor: 1,
		MaxContextTokens:      100,
	}
	adapter := NewOpenAICompatAdapter(&http.Client{Timeout: 2 * time.Second})

	_, err = adapter.ChatCompletion(context.Background(), entry, sampleRequestBody())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUpstreamUnreachable)
}

// =============================================================================
// 上游 URL trailing slash 容错
// =============================================================================

func TestChatCompletion_BaseURLTrailingSlash(t *testing.T) {
	var seenPath string
	adapter, entry, cleanup := newTestAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	})
	defer cleanup()
	entry.UpstreamBaseURL += "/" // 加 trailing slash

	_, err := adapter.ChatCompletion(context.Background(), entry, sampleRequestBody())
	require.NoError(t, err)
	assert.Equal(t, "/chat/completions", seenPath, "trailing slash 应被规范化")
}

// =============================================================================
// 工厂
// =============================================================================

func TestNewAdapter_OpenAICompat(t *testing.T) {
	a, err := NewAdapter("openai_compat", &http.Client{})
	require.NoError(t, err)
	require.NotNil(t, a)
}

func TestNewAdapter_UnknownProvider(t *testing.T) {
	_, err := NewAdapter("ollama", &http.Client{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "openai_compat")
}

func TestNewAdapter_NilClient(t *testing.T) {
	_, err := NewAdapter("openai_compat", nil)
	require.Error(t, err)
}

func TestNewOpenAICompatAdapter_PanicOnNilClient(t *testing.T) {
	defer func() {
		require.NotNil(t, recover(), "nil client 必须 panic")
	}()
	_ = NewOpenAICompatAdapter(nil)
}

// =============================================================================
// NewUpstreamClient 配置 sanity
// =============================================================================

func TestNewUpstreamClient_Configured(t *testing.T) {
	c := NewUpstreamClient()
	require.NotNil(t, c)
	assert.Equal(t, UpstreamClientTimeout, c.Timeout)
}

// =============================================================================
// 并发安全
// =============================================================================

func TestChatCompletion_ConcurrentSafe(t *testing.T) {
	var hits int64
	adapter, entry, cleanup := newTestAdapter(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	})
	defer cleanup()

	const N = 50
	errCh := make(chan error, N)
	for i := 0; i < N; i++ {
		go func(idx int) {
			req := sampleRequestBody()
			req["__idx"] = idx
			_, err := adapter.ChatCompletion(context.Background(), entry, req)
			errCh <- err
		}(i)
	}
	for i := 0; i < N; i++ {
		require.NoError(t, <-errCh, "并发 ChatCompletion 应全部成功")
	}
	assert.Equal(t, int64(N), atomic.LoadInt64(&hits))
}

// =============================================================================
// 内部函数测试（buildUpstreamBody / parseUsageFromBody / classifyClientErr）
// =============================================================================

func TestBuildUpstreamBody_NilInput(t *testing.T) {
	body, err := buildUpstreamBody(nil, "upstream-model-x")
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(body, &m))
	assert.Equal(t, "upstream-model-x", m["model"])
}

func TestBuildUpstreamBody_OverwritesModel(t *testing.T) {
	body, err := buildUpstreamBody(map[string]any{
		"model":       "business-model",
		"temperature": 0.5,
	}, "upstream-real")
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(body, &m))
	assert.Equal(t, "upstream-real", m["model"])
	assert.Equal(t, float64(0.5), m["temperature"])
}

func TestBuildUpstreamBody_DoesNotMutateInput(t *testing.T) {
	original := map[string]any{
		"model":       "business-model",
		"temperature": 0.5,
	}
	_, err := buildUpstreamBody(original, "upstream-real")
	require.NoError(t, err)
	assert.Equal(t, "business-model", original["model"], "原 map 不应被修改（adapter 浅克隆）")
}

func TestParseUsageFromBody_Happy(t *testing.T) {
	usage, err := parseUsageFromBody([]byte(`{"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`))
	require.NoError(t, err)
	require.NotNil(t, usage)
	assert.Equal(t, 10, usage.PromptTokens)
}

func TestParseUsageFromBody_NoUsageField(t *testing.T) {
	usage, err := parseUsageFromBody([]byte(`{"id":"x"}`))
	require.NoError(t, err)
	assert.Nil(t, usage)
}

func TestParseUsageFromBody_NonJSON(t *testing.T) {
	_, err := parseUsageFromBody([]byte("<html>"))
	require.Error(t, err)
}

func TestClassifyClientErr_DeadlineExceeded(t *testing.T) {
	wrapped := fmt.Errorf("dial: %w", context.DeadlineExceeded)
	got := classifyClientErr(wrapped)
	assert.ErrorIs(t, got, ErrUpstreamTimeout)
}

func TestClassifyClientErr_Canceled(t *testing.T) {
	wrapped := fmt.Errorf("dial: %w", context.Canceled)
	got := classifyClientErr(wrapped)
	assert.ErrorIs(t, got, ErrUpstreamTimeout)
}

func TestClassifyClientErr_Generic(t *testing.T) {
	got := classifyClientErr(errors.New("connection refused"))
	assert.ErrorIs(t, got, ErrUpstreamUnreachable)
}

// =============================================================================
// 16 MiB body 限制
// =============================================================================

func TestChatCompletion_HugeBodyCapped(t *testing.T) {
	// 上游返巨型 body（17 MiB）；adapter 应只读 16 MiB
	// 此场景实际不应发生，但防御性测试 io.LimitReader 生效
	adapter, entry, cleanup := newTestAdapter(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, io.LimitReader(zeroReader{}, 17*1024*1024))
	})
	defer cleanup()

	resp, err := adapter.ChatCompletion(context.Background(), entry, sampleRequestBody())
	// 因为不是合法 JSON（全 0 字节），ErrUpstreamMalformed
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUpstreamMalformed)
	_ = resp
}

// zeroReader 返全 0 字节的 reader，用于 huge body 测试。
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

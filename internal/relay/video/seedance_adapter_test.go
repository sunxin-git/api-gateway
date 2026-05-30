package video

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newMockSeedance 起一个 httptest 上游 + 绑定的 adapter/entry。
func newMockSeedance(t *testing.T, handler http.HandlerFunc) (*SeedanceAdapter, *VideoModelEntry) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	adapter := NewSeedanceAdapter(srv.Client())
	entry := &VideoModelEntry{
		GatewayModelName:  "gw-video",
		UpstreamModelName: "doubao-seedance-2-0-t2v",
		UpstreamBaseURL:   srv.URL,
	}
	return adapter, entry
}

func testReq() *ValidatedRequest {
	return &ValidatedRequest{
		TaskType:   TaskTypeTextToVideo,
		Prompt:     "一只在草地奔跑的狗",
		Duration:   5,
		Resolution: "720p",
		Ratio:      "16:9",
		Fps:        24,
	}
}

var testCreds = UpstreamCredentials{APIKey: "test-ark-key"}

// ---------------------------------------------------------------------------
// Submit
// ---------------------------------------------------------------------------

func TestSeedanceSubmit_Happy(t *testing.T) {
	var gotBody map[string]any
	var gotAuth, gotMethod, gotPath string
	adapter, entry := newMockSeedance(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"cgt-abc123"}`))
	})

	id, err := adapter.Submit(context.Background(), entry, testCreds, testReq(), "https://gw/cb/tok123")
	require.NoError(t, err)
	assert.Equal(t, "cgt-abc123", id)

	// 请求形态
	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, "/contents/generations/tasks", gotPath)
	assert.Equal(t, "Bearer test-ark-key", gotAuth)

	// body 改写：model→上游真实名，prompt→content[text]，各参数透传
	assert.Equal(t, "doubao-seedance-2-0-t2v", gotBody["model"])
	content, ok := gotBody["content"].([]any)
	require.True(t, ok)
	require.Len(t, content, 1)
	item := content[0].(map[string]any)
	assert.Equal(t, "text", item["type"])
	assert.Equal(t, "一只在草地奔跑的狗", item["text"])
	assert.Equal(t, "16:9", gotBody["ratio"])
	assert.Equal(t, float64(5), gotBody["duration"])
	assert.Equal(t, "720p", gotBody["resolution"])
	assert.Equal(t, float64(24), gotBody["fps"])
	assert.Equal(t, false, gotBody["watermark"])
	assert.Equal(t, true, gotBody["generate_audio"])
	assert.Equal(t, "https://gw/cb/tok123", gotBody["callback_url"])
}

func TestSeedanceSubmit_NoCallbackOmitsField(t *testing.T) {
	var gotBody map[string]any
	adapter, entry := newMockSeedance(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		_, _ = w.Write([]byte(`{"id":"cgt-x"}`))
	})
	_, err := adapter.Submit(context.Background(), entry, testCreds, testReq(), "")
	require.NoError(t, err)
	_, has := gotBody["callback_url"]
	assert.False(t, has, "空 callbackURL 不应写入 body")
}

func TestSeedanceSubmit_TaskIDAlias(t *testing.T) {
	adapter, entry := newMockSeedance(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"task_id":"cgt-from-alias"}`))
	})
	id, err := adapter.Submit(context.Background(), entry, testCreds, testReq(), "")
	require.NoError(t, err)
	assert.Equal(t, "cgt-from-alias", id)
}

func TestSeedanceSubmit_ErrorPaths(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantErr error
	}{
		{"rejected_400", http.StatusBadRequest, `{"error":{"message":"bad ratio"}}`, ErrUpstreamRejected},
		{"rejected_401", http.StatusUnauthorized, `{"error":"unauthorized"}`, ErrUpstreamRejected},
		{"server_500", http.StatusInternalServerError, `oops`, ErrUpstreamServer},
		{"server_503", http.StatusServiceUnavailable, `busy`, ErrUpstreamServer},
		{"malformed_no_id", http.StatusOK, `{"foo":"bar"}`, ErrUpstreamMalformed},
		{"malformed_not_json", http.StatusOK, `<html>nope`, ErrUpstreamMalformed},
		{"malformed_empty_id", http.StatusOK, `{"id":"   "}`, ErrUpstreamMalformed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			adapter, entry := newMockSeedance(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			_, err := adapter.Submit(context.Background(), entry, testCreds, testReq(), "")
			require.Error(t, err)
			assert.ErrorIs(t, err, tc.wantErr)
		})
	}
}

func TestSeedanceSubmit_NilGuards(t *testing.T) {
	a := NewSeedanceAdapter(http.DefaultClient)
	_, err := a.Submit(context.Background(), nil, testCreds, testReq(), "")
	require.Error(t, err)
	_, err = a.Submit(context.Background(), &VideoModelEntry{}, testCreds, nil, "")
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// Poll
// ---------------------------------------------------------------------------

func TestSeedancePoll_SucceededWithUsage(t *testing.T) {
	var gotPath string
	adapter, entry := newMockSeedance(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "Bearer test-ark-key", r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{
			"id":"cgt-1","status":"succeeded",
			"content":{"video_url":"https://tos.example/v.mp4"},
			"usage":{"completion_tokens":1638400,"total_tokens":1638400}
		}`))
	})
	res, err := adapter.Poll(context.Background(), entry, testCreds, "cgt-1")
	require.NoError(t, err)
	assert.Equal(t, "/contents/generations/tasks/cgt-1", gotPath)
	assert.Equal(t, UpstreamSucceeded, res.Status)
	require.NotNil(t, res.Usage)
	assert.Equal(t, int64(1638400), res.Usage.CompletionTokens)
	assert.Equal(t, "https://tos.example/v.mp4", res.ResultURL)
}

func TestSeedancePoll_SucceededMissingUsage(t *testing.T) {
	cases := map[string]string{
		"no_usage":   `{"status":"succeeded","content":{"video_url":"https://x/v.mp4"}}`,
		"zero_usage": `{"status":"succeeded","content":{"video_url":"https://x/v.mp4"},"usage":{"completion_tokens":0}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			adapter, entry := newMockSeedance(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(body))
			})
			res, err := adapter.Poll(context.Background(), entry, testCreds, "cgt-1")
			require.NoError(t, err)
			assert.Equal(t, UpstreamSucceeded, res.Status)
			assert.Nil(t, res.Usage, "缺 usage / completion_tokens≤0 → nil（交 settle 兜底）")
			assert.Equal(t, "https://x/v.mp4", res.ResultURL)
		})
	}
}

func TestSeedancePoll_StatusAliases(t *testing.T) {
	cases := []struct {
		raw  string
		want UpstreamStatus
	}{
		{"queued", UpstreamRunning},
		{"running", UpstreamRunning},
		{"pending", UpstreamRunning},
		{"processing", UpstreamRunning},
		{"in_progress", UpstreamRunning},
		{"SUCCEEDED", UpstreamSucceeded}, // 大小写不敏感
		{"done", UpstreamSucceeded},
		{"failed", UpstreamFailed},
		{"error", UpstreamFailed},
		{"cancelled", UpstreamCancelled},
		{"canceled", UpstreamCancelled},
		{"expired", UpstreamExpired},
		{"weird_unknown", UpstreamRunning}, // 未知 → Running（不提前终态）
		{"", UpstreamRunning},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			adapter, entry := newMockSeedance(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"status":"` + tc.raw + `"}`))
			})
			res, err := adapter.Poll(context.Background(), entry, testCreds, "cgt-1")
			require.NoError(t, err)
			assert.Equal(t, tc.want, res.Status)
		})
	}
}

func TestSeedancePoll_FailedWithMessage(t *testing.T) {
	adapter, entry := newMockSeedance(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"failed","error":{"code":"content_violation","message":"prompt 违规"}}`))
	})
	res, err := adapter.Poll(context.Background(), entry, testCreds, "cgt-1")
	require.NoError(t, err)
	assert.Equal(t, UpstreamFailed, res.Status)
	assert.Equal(t, "prompt 违规", res.FailureMessage)
	assert.Equal(t, "content_violation", res.FailureCode)
	assert.Nil(t, res.Usage)
	assert.Empty(t, res.ResultURL)
}

// TestSeedancePoll_TerminalFailureVariants 覆盖 Failed/Cancelled/Expired × 有无 error（防三态复制粘贴漏）。
func TestSeedancePoll_TerminalFailureVariants(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		status   UpstreamStatus
		wantMsg  string
		wantCode string
	}{
		{"failed_no_error", `{"status":"failed"}`, UpstreamFailed, "", ""},
		{"cancelled_with_error", `{"status":"cancelled","error":{"code":"user_cancel","message":"用户取消"}}`, UpstreamCancelled, "用户取消", "user_cancel"},
		{"cancelled_no_error", `{"status":"cancelled"}`, UpstreamCancelled, "", ""},
		{"expired_with_error", `{"status":"expired","error":{"code":"exec_timeout","message":"执行超期"}}`, UpstreamExpired, "执行超期", "exec_timeout"},
		{"expired_no_error", `{"status":"expired"}`, UpstreamExpired, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			adapter, entry := newMockSeedance(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			})
			res, err := adapter.Poll(context.Background(), entry, testCreds, "cgt-1")
			require.NoError(t, err)
			assert.Equal(t, tc.status, res.Status)
			assert.Equal(t, tc.wantMsg, res.FailureMessage)
			assert.Equal(t, tc.wantCode, res.FailureCode)
			assert.Nil(t, res.Usage)
		})
	}
}

// TestSeedancePoll_NegativeUsageTreatedMissing 负 completion_tokens → 视为缺 usage（不猜扣）。
func TestSeedancePoll_NegativeUsageTreatedMissing(t *testing.T) {
	adapter, entry := newMockSeedance(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"succeeded","content":{"video_url":"https://x/v.mp4"},"usage":{"completion_tokens":-5000000}}`))
	})
	res, err := adapter.Poll(context.Background(), entry, testCreds, "cgt-1")
	require.NoError(t, err)
	assert.Equal(t, UpstreamSucceeded, res.Status)
	assert.Nil(t, res.Usage, "负 token 不可信 → nil（交 settle_failed 兜底）")
}

// TestSeedancePoll_TaskIDPathEscaped task_id 含特殊字符须转义入路径（防路径穿越）。
func TestSeedancePoll_TaskIDPathEscaped(t *testing.T) {
	var gotRawPath string
	adapter, entry := newMockSeedance(t, func(w http.ResponseWriter, r *http.Request) {
		gotRawPath = r.URL.EscapedPath()
		_, _ = w.Write([]byte(`{"status":"running"}`))
	})
	_, err := adapter.Poll(context.Background(), entry, testCreds, "cgt/../evil")
	require.NoError(t, err)
	assert.Contains(t, gotRawPath, "cgt%2F..%2Fevil", "task_id 的 / 须被 PathEscape 转义")
}

func TestSeedancePoll_NilEntry(t *testing.T) {
	a := NewSeedanceAdapter(http.DefaultClient)
	_, err := a.Poll(context.Background(), nil, testCreds, "cgt-1")
	require.Error(t, err)
}

// TestSeedance_EmptyAPIKey 空凭据 fail-closed：不发空 Bearer 给上游（Submit/Poll 均拒）。
func TestSeedance_EmptyAPIKey(t *testing.T) {
	called := false
	adapter, entry := newMockSeedance(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		_, _ = w.Write([]byte(`{"id":"x"}`))
	})
	_, err := adapter.Submit(context.Background(), entry, UpstreamCredentials{APIKey: "  "}, testReq(), "")
	require.Error(t, err)
	_, err = adapter.Poll(context.Background(), entry, UpstreamCredentials{}, "cgt-1")
	require.Error(t, err)
	assert.False(t, called, "空凭据应在发请求前 fail-closed，不触达上游")
}

func TestSeedanceSubmit_404IsRejected(t *testing.T) {
	adapter, entry := newMockSeedance(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`no route`))
	})
	_, err := adapter.Submit(context.Background(), entry, testCreds, testReq(), "")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUpstreamRejected, "Submit 的 404 归 4xx Rejected，非 TaskNotFound")
}

func TestSeedance_ContextCanceled(t *testing.T) {
	adapter, entry := newMockSeedance(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte(`{"status":"running"}`))
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	_, err := adapter.Poll(ctx, entry, testCreds, "cgt-1")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUpstreamTimeout, "context.Canceled 归 Timeout")
}

func TestSeedancePoll_ErrorPaths(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantErr error
	}{
		{"not_found_404", http.StatusNotFound, `{"error":"no such task"}`, ErrUpstreamTaskNotFound},
		{"rejected_403", http.StatusForbidden, `forbidden`, ErrUpstreamRejected},
		{"server_500", http.StatusInternalServerError, `oops`, ErrUpstreamServer},
		{"malformed", http.StatusOK, `not-json`, ErrUpstreamMalformed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			adapter, entry := newMockSeedance(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			_, err := adapter.Poll(context.Background(), entry, testCreds, "cgt-1")
			require.Error(t, err)
			assert.ErrorIs(t, err, tc.wantErr)
		})
	}
}

func TestSeedancePoll_EmptyTaskID(t *testing.T) {
	a := NewSeedanceAdapter(http.DefaultClient)
	_, err := a.Poll(context.Background(), &VideoModelEntry{UpstreamBaseURL: "https://x"}, testCreds, "  ")
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// 传输层错误：timeout / unreachable
// ---------------------------------------------------------------------------

func TestSeedance_Timeout(t *testing.T) {
	adapter, entry := newMockSeedance(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte(`{"id":"x"}`))
	})
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	_, err := adapter.Submit(ctx, entry, testCreds, testReq(), "")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUpstreamTimeout)
}

func TestSeedance_Unreachable(t *testing.T) {
	// 起后立即关，复用其 URL → 连接拒绝。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := srv.URL
	srv.Close()

	adapter := NewSeedanceAdapter(&http.Client{Timeout: 2 * time.Second})
	entry := &VideoModelEntry{UpstreamModelName: "m", UpstreamBaseURL: deadURL}
	_, err := adapter.Poll(context.Background(), entry, testCreds, "cgt-1")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUpstreamUnreachable)
}

// ---------------------------------------------------------------------------
// normalizeUpstreamStatus 直测 + IsTerminal
// ---------------------------------------------------------------------------

func TestNormalizeUpstreamStatus(t *testing.T) {
	assert.Equal(t, UpstreamSucceeded, normalizeUpstreamStatus("  Succeeded "))
	assert.Equal(t, UpstreamFailed, normalizeUpstreamStatus("ERROR"))
	assert.Equal(t, UpstreamCancelled, normalizeUpstreamStatus("canceled"))
	assert.Equal(t, UpstreamExpired, normalizeUpstreamStatus("expired"))
	assert.Equal(t, UpstreamRunning, normalizeUpstreamStatus("queued"))
	assert.Equal(t, UpstreamRunning, normalizeUpstreamStatus(""))
}

func TestUpstreamStatus_IsTerminal(t *testing.T) {
	assert.False(t, UpstreamRunning.IsTerminal())
	for _, s := range []UpstreamStatus{UpstreamSucceeded, UpstreamFailed, UpstreamCancelled, UpstreamExpired} {
		assert.True(t, s.IsTerminal(), string(s))
	}
}

// 防御：上游错误 body 截断不 panic（长 body）。
func TestBodySnippet_Truncates(t *testing.T) {
	long := strings.Repeat("x", errBodySnippetMax+50)
	out := bodySnippet([]byte(long))
	assert.LessOrEqual(t, len(out), errBodySnippetMax+len("…"))
	assert.True(t, strings.HasSuffix(out, "…"))
}

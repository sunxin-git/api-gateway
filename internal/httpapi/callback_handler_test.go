package httpapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sunxin-git/api-gateway/internal/task"
)

type fakeCallbackProcessor struct {
	outcome   task.CallbackOutcome
	err       error
	gotTaskID string
	gotToken  string
	calls     int
}

func (f *fakeCallbackProcessor) HandleCallback(_ context.Context, taskID, token string) (task.CallbackOutcome, error) {
	f.calls++
	f.gotTaskID = taskID
	f.gotToken = token
	return f.outcome, f.err
}

func callbackTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func newCallbackTestEngine(p CallbackProcessor) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewVideoCallbackHandler(p, callbackTestLogger())
	r.POST(task.CallbackPathPrefix+"/:task_id/:token", h.Handle)
	return r
}

func postCallback(t *testing.T, r *gin.Engine, taskID, token string) *httptest.ResponseRecorder {
	t.Helper()
	url := task.CallbackPathPrefix + "/" + taskID + "/" + token
	req := httptest.NewRequest(http.MethodPost, url, strings.NewReader(`{"untrusted":"body"}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestVideoCallbackHandler_StatusMapping(t *testing.T) {
	cases := []struct {
		name       string
		outcome    task.CallbackOutcome
		err        error
		wantStatus int
	}{
		{"accepted", task.CallbackAccepted, nil, http.StatusOK},
		{"ignored_unknown", task.CallbackIgnoredUnknownTask, nil, http.StatusOK},
		{"ignored_state", task.CallbackIgnoredState, nil, http.StatusOK},
		{"unauthorized", task.CallbackUnauthorized, nil, http.StatusUnauthorized},
		{"transient_error_500", task.CallbackAccepted, errors.New("db blip"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &fakeCallbackProcessor{outcome: tc.outcome, err: tc.err}
			r := newCallbackTestEngine(p)
			w := postCallback(t, r, "vtask_abc", "tok_xyz")
			assert.Equal(t, tc.wantStatus, w.Code)
		})
	}
}

// TestVideoCallbackHandler_PassesPathParams 校验 task_id / token 从路径正确解析并透传给 processor。
func TestVideoCallbackHandler_PassesPathParams(t *testing.T) {
	p := &fakeCallbackProcessor{outcome: task.CallbackAccepted}
	r := newCallbackTestEngine(p)
	w := postCallback(t, r, "vtask_deadbeef", "tok_secret_segment")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "vtask_deadbeef", p.gotTaskID)
	assert.Equal(t, "tok_secret_segment", p.gotToken)
	assert.Equal(t, 1, p.calls)
}

// TestVideoCallbackHandler_MissingToken 缺 token 段 → 路由不匹配（404，processor 不被调用）。
func TestVideoCallbackHandler_MissingToken(t *testing.T) {
	p := &fakeCallbackProcessor{outcome: task.CallbackAccepted}
	r := newCallbackTestEngine(p)
	// 仅 task_id 段，无 token 段 → 路由 /:task_id/:token 不匹配。
	req := httptest.NewRequest(http.MethodPost, task.CallbackPathPrefix+"/vtask_only", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Equal(t, 0, p.calls, "路由不匹配，processor 不应被调用")
}

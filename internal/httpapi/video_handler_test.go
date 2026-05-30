package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sunxin-git/api-gateway/internal/businesskey"
	"github.com/sunxin-git/api-gateway/internal/channel"
	"github.com/sunxin-git/api-gateway/internal/db"
	"github.com/sunxin-git/api-gateway/internal/ledger"
	"github.com/sunxin-git/api-gateway/internal/relay/video"
	"github.com/sunxin-git/api-gateway/internal/task"
)

// ---------- 模拟 VideoTaskService ----------

type fakeVideoSvc struct {
	submitFn      func(ctx context.Context, p task.SubmitParams) (string, error)
	getFn         func(ctx context.Context, accountID, taskID string) (*task.TaskView, error)
	balanceFn     func(ctx context.Context, accountID string) (*ledger.Balance, error)
	entitlementFn func(ctx context.Context, accountID, gatewayModel string) (bool, error)
	presignFn     func(ctx context.Context, accountID, taskID string, ttl time.Duration) (string, error)

	submitCalled        bool
	submitGotAccountID  string
	getGotAccountID     string
	getGotTaskID        string
	presignGotAccountID string
}

func (f *fakeVideoSvc) Submit(ctx context.Context, p task.SubmitParams) (string, error) {
	f.submitCalled = true
	f.submitGotAccountID = p.BusinessAccountID
	return f.submitFn(ctx, p)
}

func (f *fakeVideoSvc) GetForAccount(ctx context.Context, accountID, taskID string) (*task.TaskView, error) {
	f.getGotAccountID = accountID
	f.getGotTaskID = taskID
	return f.getFn(ctx, accountID, taskID)
}

func (f *fakeVideoSvc) GetBalance(ctx context.Context, accountID string) (*ledger.Balance, error) {
	return f.balanceFn(ctx, accountID)
}

func (f *fakeVideoSvc) CheckEntitlement(ctx context.Context, accountID, gatewayModel string) (bool, error) {
	if f.entitlementFn == nil {
		return true, nil
	}
	return f.entitlementFn(ctx, accountID, gatewayModel)
}

func (f *fakeVideoSvc) PresignResult(ctx context.Context, accountID, taskID string, ttl time.Duration) (string, error) {
	f.presignGotAccountID = accountID
	if f.presignFn == nil {
		return "", nil
	}
	return f.presignFn(ctx, accountID, taskID, ttl)
}

var _ VideoTaskService = (*fakeVideoSvc)(nil)

// ---------- 测试装置 ----------

func testVideoCatalog(t *testing.T) video.VideoCatalog {
	t.Helper()
	cat, err := video.NewEnvVideoCatalog(video.CatalogConfig{
		GatewayModelName:       "gw-video",
		UpstreamProviderType:   video.ProviderTypeVolcSeedance,
		UpstreamBaseURL:        "https://ark.example/api/v3",
		UpstreamModelName:      "doubao-seedance-mock",
		ChannelName:            "test-ch",
		Price720pPer1MMinor:    6000,
		BillingMultiplierBP:    10_000,
		DurationMinSeconds:     4,
		DurationMaxSeconds:     15,
		DurationDefaultSeconds: 5,
		FpsDefault:             24,
		FpsMax:                 30,
		Ratios:                 []string{"16:9", "1:1"},
		RatioDefault:           "16:9",
		ResolutionDefault:      "720p",
	})
	require.NoError(t, err)
	return cat
}

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

const testAccountID = "biz-acct-A"

// newVideoTestEngine 装一个最小 gin engine：注入业务 key 上下文（模拟 BusinessKeyAuth）+ 注册视频路由。
func newVideoTestEngine(t *testing.T, svc VideoTaskService) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h := NewVideoHandler(svc, testVideoCatalog(t), 12_000, 0, 15*time.Minute, silentLogger())
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("business_key_validation", &businesskey.ValidationResult{
			Key: &businesskey.Key{ID: 7, BusinessAccountID: testAccountID},
		})
		c.Next()
	})
	r.POST("/v1/video/generations", h.Submit)
	r.GET("/v1/video/generations/:id", h.Get)
	r.GET("/v1/account/balance", h.GetBalance)
	return r
}

func doJSON(t *testing.T, r *gin.Engine, method, path, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = bytes.NewReader([]byte(body))
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var parsed map[string]any
	if w.Body.Len() > 0 {
		_ = json.Unmarshal(w.Body.Bytes(), &parsed)
	}
	return w, parsed
}

func errorCode(parsed map[string]any) string {
	e, ok := parsed["error"].(map[string]any)
	if !ok {
		return ""
	}
	code, _ := e["code"].(string)
	return code
}

// ---------- POST /v1/video/generations ----------

func TestVideoSubmit_Happy(t *testing.T) {
	svc := &fakeVideoSvc{
		submitFn: func(_ context.Context, p task.SubmitParams) (string, error) {
			assert.Equal(t, testAccountID, p.BusinessAccountID)
			assert.Greater(t, p.ReserveMinor, int64(0), "reserve 上界须 > 0")
			require.NotNil(t, p.ActorTokenID)
			assert.Equal(t, int64(7), *p.ActorTokenID)
			return "vtask_happy", nil
		},
	}
	r := newVideoTestEngine(t, svc)
	w, body := doJSON(t, r, http.MethodPost, "/v1/video/generations",
		`{"model":"gw-video","prompt":"a running dog","duration":5,"resolution":"720p"}`)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "vtask_happy", body["id"])
	assert.Equal(t, "queued", body["status"])
	assert.Equal(t, "text_to_video", body["task_type"])
}

func TestVideoSubmit_MissingModel(t *testing.T) {
	svc := &fakeVideoSvc{submitFn: func(context.Context, task.SubmitParams) (string, error) { return "x", nil }}
	r := newVideoTestEngine(t, svc)
	w, body := doJSON(t, r, http.MethodPost, "/v1/video/generations", `{"prompt":"a dog"}`)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "missing_model", errorCode(body))
	assert.False(t, svc.submitCalled, "缺 model 在 reserve/submit 前短路")
}

// TestVideoSubmit_AccountFrozen_402：账户冻结 → 402 account_frozen（与 relay handler 一致）。
func TestVideoSubmit_AccountFrozen_402(t *testing.T) {
	svc := &fakeVideoSvc{
		submitFn: func(context.Context, task.SubmitParams) (string, error) {
			return "", ledger.ErrAccountFrozen
		},
	}
	r := newVideoTestEngine(t, svc)
	w, body := doJSON(t, r, http.MethodPost, "/v1/video/generations",
		`{"model":"gw-video","prompt":"a dog"}`)
	assert.Equal(t, http.StatusPaymentRequired, w.Code)
	assert.Equal(t, "account_frozen", errorCode(body))
}

// TestVideoSubmit_AccountNotFound_401：账户不存在 → 401（鉴权语义，与 Submit/relay 一致）。
func TestVideoSubmit_AccountNotFound_401(t *testing.T) {
	svc := &fakeVideoSvc{
		submitFn: func(context.Context, task.SubmitParams) (string, error) {
			return "", ledger.ErrAccountNotFound
		},
	}
	r := newVideoTestEngine(t, svc)
	w, body := doJSON(t, r, http.MethodPost, "/v1/video/generations",
		`{"model":"gw-video","prompt":"a dog"}`)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Equal(t, "account_not_found", errorCode(body))
}

// TestVideoSubmit_VersionConflict_503：乐观锁冲突 → 503 temporarily_unavailable（业务重试）。
func TestVideoSubmit_VersionConflict_503(t *testing.T) {
	svc := &fakeVideoSvc{
		submitFn: func(context.Context, task.SubmitParams) (string, error) {
			return "", ledger.ErrVersionConflict
		},
	}
	r := newVideoTestEngine(t, svc)
	w, body := doJSON(t, r, http.MethodPost, "/v1/video/generations",
		`{"model":"gw-video","prompt":"a dog"}`)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Equal(t, "temporarily_unavailable", errorCode(body))
}

// TestVideoSubmit_DecryptFailed_503：渠道凭据解密失败 → 503 model_unavailable（fail-closed）。
func TestVideoSubmit_DecryptFailed_503(t *testing.T) {
	svc := &fakeVideoSvc{
		submitFn: func(context.Context, task.SubmitParams) (string, error) {
			return "", channel.ErrDecryptFailed
		},
	}
	r := newVideoTestEngine(t, svc)
	w, body := doJSON(t, r, http.MethodPost, "/v1/video/generations",
		`{"model":"gw-video","prompt":"a dog"}`)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Equal(t, "model_unavailable", errorCode(body))
}

func TestVideoSubmit_NotEntitled_403_NoReserve(t *testing.T) {
	svc := &fakeVideoSvc{
		entitlementFn: func(context.Context, string, string) (bool, error) { return false, nil },
		submitFn:      func(context.Context, task.SubmitParams) (string, error) { return "x", nil },
	}
	r := newVideoTestEngine(t, svc)
	w, body := doJSON(t, r, http.MethodPost, "/v1/video/generations",
		`{"model":"gw-video","prompt":"a dog"}`)

	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Equal(t, "model_not_entitled", errorCode(body))
	assert.False(t, svc.submitCalled, "entitlement 失败必须在 reserve 前短路（无 orphan reserve）")
}

func TestVideoSubmit_InvalidDuration_400_NoReserve(t *testing.T) {
	svc := &fakeVideoSvc{submitFn: func(context.Context, task.SubmitParams) (string, error) { return "x", nil }}
	r := newVideoTestEngine(t, svc)
	w, body := doJSON(t, r, http.MethodPost, "/v1/video/generations",
		`{"model":"gw-video","prompt":"a dog","duration":2}`) // < min 4

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, video.CodeParamOutOfRange, errorCode(body))
	assert.False(t, svc.submitCalled, "校验失败在 reserve 前短路")
}

func TestVideoSubmit_InsufficientBalance_402(t *testing.T) {
	svc := &fakeVideoSvc{
		submitFn: func(context.Context, task.SubmitParams) (string, error) {
			return "", ledger.ErrInsufficientBalance
		},
	}
	r := newVideoTestEngine(t, svc)
	w, body := doJSON(t, r, http.MethodPost, "/v1/video/generations",
		`{"model":"gw-video","prompt":"a dog"}`)

	assert.Equal(t, http.StatusPaymentRequired, w.Code)
	assert.Equal(t, "insufficient_quota", errorCode(body))
}

func TestVideoSubmit_ConcurrencyLimit_429(t *testing.T) {
	svc := &fakeVideoSvc{
		submitFn: func(context.Context, task.SubmitParams) (string, error) {
			return "", task.ErrConcurrencyLimit
		},
	}
	r := newVideoTestEngine(t, svc)
	w, body := doJSON(t, r, http.MethodPost, "/v1/video/generations",
		`{"model":"gw-video","prompt":"a dog"}`)

	assert.Equal(t, http.StatusTooManyRequests, w.Code)
	assert.Equal(t, "concurrency_limit", errorCode(body))
}

func TestVideoSubmit_ChannelUnavailable_503(t *testing.T) {
	svc := &fakeVideoSvc{
		submitFn: func(context.Context, task.SubmitParams) (string, error) {
			return "", channel.ErrChannelNotFound
		},
	}
	r := newVideoTestEngine(t, svc)
	w, body := doJSON(t, r, http.MethodPost, "/v1/video/generations",
		`{"model":"gw-video","prompt":"a dog"}`)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Equal(t, "model_unavailable", errorCode(body))
}

// ---------- GET /v1/video/generations/:id ----------

// TestVideoGet_CrossTenant_404：A 用自己 key 查不属于自己的 task → 404（svc 返 ErrTaskNotFound），
// 且 handler 必须用**鉴权账户**调 GetForAccount（不可被 body/path 越权）。
func TestVideoGet_CrossTenant_404(t *testing.T) {
	svc := &fakeVideoSvc{
		getFn: func(_ context.Context, accountID, taskID string) (*task.TaskView, error) {
			// 模拟 SQL 强制归属：账户不匹配 → 0 行 → ErrTaskNotFound。
			return nil, task.ErrTaskNotFound
		},
	}
	r := newVideoTestEngine(t, svc)
	w, body := doJSON(t, r, http.MethodGet, "/v1/video/generations/vtask_of_B", "")

	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Equal(t, "task_not_found", errorCode(body))
	assert.Equal(t, testAccountID, svc.getGotAccountID, "必须用鉴权账户做归属查询（防越权）")
	assert.Equal(t, "vtask_of_B", svc.getGotTaskID)
}

func TestVideoGet_Running(t *testing.T) {
	svc := &fakeVideoSvc{
		getFn: func(context.Context, string, string) (*task.TaskView, error) {
			return &task.TaskView{ID: "vtask_r", Status: db.TaskStatusUPSTREAMSUBMITTED, Model: "gw-video", TaskType: "text_to_video"}, nil
		},
	}
	r := newVideoTestEngine(t, svc)
	w, body := doJSON(t, r, http.MethodGet, "/v1/video/generations/vtask_r", "")

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "running", body["status"])
	_, hasResult := body["result"]
	assert.False(t, hasResult, "未完成不含 result")
}

func TestVideoGet_Succeeded_WithSignedURL(t *testing.T) {
	var presignTaskID string
	svc := &fakeVideoSvc{
		getFn: func(context.Context, string, string) (*task.TaskView, error) {
			return &task.TaskView{ID: "vtask_ok", Status: db.TaskStatusSETTLED, Model: "gw-video", TaskType: "text_to_video"}, nil
		},
		presignFn: func(_ context.Context, accountID, taskID string, _ time.Duration) (string, error) {
			presignTaskID = taskID
			return "https://tos.example/signed?sig=xyz", nil
		},
	}
	r := newVideoTestEngine(t, svc)
	w, body := doJSON(t, r, http.MethodGet, "/v1/video/generations/vtask_ok", "")

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "succeeded", body["status"])
	assert.Equal(t, "vtask_ok", presignTaskID)
	assert.Equal(t, testAccountID, svc.presignGotAccountID, "现签必须用鉴权账户做归属（结构性防越权）")
	result, ok := body["result"].(map[string]any)
	require.True(t, ok, "succeeded 应含 result")
	assert.Equal(t, "https://tos.example/signed?sig=xyz", result["video_url"])
}

// TestVideoGet_Succeeded_PresignError_Degrades：现签出错 → GET 仍 200 succeeded 但无 result（降级，不 500）。
func TestVideoGet_Succeeded_PresignError_Degrades(t *testing.T) {
	svc := &fakeVideoSvc{
		getFn: func(context.Context, string, string) (*task.TaskView, error) {
			return &task.TaskView{ID: "vtask_ok", Status: db.TaskStatusSETTLED, Model: "gw-video", TaskType: "text_to_video"}, nil
		},
		presignFn: func(context.Context, string, string, time.Duration) (string, error) {
			return "", assert.AnError // TOS 凭据缺失 / 解密失败等
		},
	}
	r := newVideoTestEngine(t, svc)
	w, body := doJSON(t, r, http.MethodGet, "/v1/video/generations/vtask_ok", "")

	require.Equal(t, http.StatusOK, w.Code, "现签失败不应 500，降级返 200")
	assert.Equal(t, "succeeded", body["status"])
	_, hasResult := body["result"]
	assert.False(t, hasResult, "现签失败 → 无 result（业务稍后重试，签名 URL 不入响应）")
}

// TestVideoGet_SettleFailed_MapsFailed：SETTLE_FAILED（error_code 空）→ failed + 默认对账码（不谎报 succeeded）。
func TestVideoGet_SettleFailed_MapsFailed(t *testing.T) {
	var presignCalled bool
	svc := &fakeVideoSvc{
		getFn: func(context.Context, string, string) (*task.TaskView, error) {
			return &task.TaskView{ID: "vtask_sf", Status: db.TaskStatusSETTLEFAILED, Model: "gw-video", TaskType: "text_to_video"}, nil
		},
		presignFn: func(context.Context, string, string, time.Duration) (string, error) {
			presignCalled = true
			return "https://tos.example/leak", nil
		},
	}
	r := newVideoTestEngine(t, svc)
	w, body := doJSON(t, r, http.MethodGet, "/v1/video/generations/vtask_sf", "")

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "failed", body["status"], "SETTLE_FAILED 不得谎报 succeeded（产物经网关不可取，否则业务无限轮询）")
	assert.Equal(t, "settlement_failed", body["error_code"], "应给稳定对账码")
	assert.False(t, presignCalled, "failed 终态不触发现签")
	_, hasResult := body["result"]
	assert.False(t, hasResult)
}

// TestVideoGet_CrossTenant_NotSigned：跨租户 GET → 404，绝不调用现签（PresignResult 不被触达）。
func TestVideoGet_CrossTenant_NotSigned(t *testing.T) {
	var presignCalled bool
	svc := &fakeVideoSvc{
		getFn: func(context.Context, string, string) (*task.TaskView, error) {
			return nil, task.ErrTaskNotFound
		},
		presignFn: func(context.Context, string, string, time.Duration) (string, error) {
			presignCalled = true
			return "x", nil
		},
	}
	r := newVideoTestEngine(t, svc)
	w, _ := doJSON(t, r, http.MethodGet, "/v1/video/generations/vtask_of_B", "")
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.False(t, presignCalled, "归属未通过绝不现签")
}

func TestVideoGet_Succeeded_NotYetStored_NoURL(t *testing.T) {
	svc := &fakeVideoSvc{
		getFn: func(context.Context, string, string) (*task.TaskView, error) {
			return &task.TaskView{ID: "vtask_ok", Status: db.TaskStatusSETTLED, Model: "gw-video", TaskType: "text_to_video"}, nil
		},
		presignFn: func(context.Context, string, string, time.Duration) (string, error) {
			return "", nil // 尚未转存
		},
	}
	r := newVideoTestEngine(t, svc)
	w, body := doJSON(t, r, http.MethodGet, "/v1/video/generations/vtask_ok", "")

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "succeeded", body["status"])
	_, hasResult := body["result"]
	assert.False(t, hasResult, "尚未转存 → 无 result（业务稍后重试）")
}

func TestVideoGet_FailedTerminal_ExposesError(t *testing.T) {
	svc := &fakeVideoSvc{
		getFn: func(context.Context, string, string) (*task.TaskView, error) {
			return &task.TaskView{
				ID: "vtask_f", Status: db.TaskStatusSETTLED, Model: "gw-video", TaskType: "text_to_video",
				ErrorCode: "upstream_rejected", ErrorMessage: "上游拒绝",
			}, nil
		},
	}
	r := newVideoTestEngine(t, svc)
	w, body := doJSON(t, r, http.MethodGet, "/v1/video/generations/vtask_f", "")

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "failed", body["status"])
	assert.Equal(t, "upstream_rejected", body["error_code"])
}

// ---------- GET /v1/account/balance ----------

func TestVideoBalance_Happy(t *testing.T) {
	svc := &fakeVideoSvc{
		balanceFn: func(_ context.Context, accountID string) (*ledger.Balance, error) {
			assert.Equal(t, testAccountID, accountID)
			return &ledger.Balance{Available: 8000, Reserved: 1000, UsedTotal: 1000, RechargeTotal: 10000}, nil
		},
	}
	r := newVideoTestEngine(t, svc)
	w, body := doJSON(t, r, http.MethodGet, "/v1/account/balance", "")

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, float64(8000), body["available_minor"])
	assert.Equal(t, float64(1000), body["reserved_minor"])
	assert.Equal(t, float64(1000), body["used_total_minor"])
}

// TestVideoBalance_AccountNotFound_401：账户不存在 → 401（与 Submit/relay 一致）。
func TestVideoBalance_AccountNotFound_401(t *testing.T) {
	svc := &fakeVideoSvc{
		balanceFn: func(context.Context, string) (*ledger.Balance, error) {
			return nil, ledger.ErrAccountNotFound
		},
	}
	r := newVideoTestEngine(t, svc)
	w, body := doJSON(t, r, http.MethodGet, "/v1/account/balance", "")
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Equal(t, "account_not_found", errorCode(body))
}

// ---------- apiStatus 单元 ----------

func TestAPIStatusMapping(t *testing.T) {
	cases := []struct {
		status    db.TaskStatus
		errorCode string
		want      string
	}{
		{db.TaskStatusSUBMITTED, "", "running"},
		{db.TaskStatusUPSTREAMSUBMITTING, "", "running"},
		{db.TaskStatusUPSTREAMSUBMITTED, "", "running"},
		{db.TaskStatusCOMPLETED, "", "running"},
		{db.TaskStatusSETTLING, "", "running"},
		{db.TaskStatusSETTLED, "", "succeeded"},
		{db.TaskStatusSETTLED, "upstream_rejected", "failed"},
		{db.TaskStatusSETTLEFAILED, "", "failed"},  // 结算失败：产物经网关不可取，不谎报 succeeded
		{db.TaskStatusSETTLEFAILED, "x", "failed"}, // 失败来源结算失败
		{db.TaskStatusFAILED, "x", "failed"},
		{db.TaskStatusCANCELLED, "", "cancelled"},
		{db.TaskStatusEXPIRED, "", "expired"},
	}
	for _, c := range cases {
		assert.Equalf(t, c.want, apiStatus(c.status, c.errorCode), "status=%s code=%q", c.status, c.errorCode)
	}
}

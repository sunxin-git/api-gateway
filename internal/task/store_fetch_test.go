package task

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cdnStatusServer 返回指定 HTTP 状态码（小 body）。
func cdnStatusServer(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte("x"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// cdnChunkedServer 200 但分块编码（无 Content-Length → resp.ContentLength = -1）。
func cdnChunkedServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.WriteHeader(http.StatusOK)
		if fl, ok := w.(http.Flusher); ok {
			_, _ = w.Write([]byte("part"))
			fl.Flush() // 触发分块（Content-Length 未知）
		}
		_, _ = w.Write([]byte("more"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestStoreResult_FetchStatusClassification 校验 fetchResult 的瞬时/永久分类：
//   - 4xx（404/403）永久 → storeResult 返 nil（不重试），无 meta；
//   - 5xx / 429 瞬时 → storeResult 返 error（asynq 重试）。
func TestStoreResult_FetchStatusClassification(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		wantRetry bool // true → storeResult 返 error
	}{
		{"404_permanent", http.StatusNotFound, false},
		{"403_permanent", http.StatusForbidden, false},
		{"500_retryable", http.StatusInternalServerError, true},
		{"502_retryable", http.StatusBadGateway, true},
		{"429_retryable", http.StatusTooManyRequests, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := setupTaskSuite(t)
			ctx := context.Background()
			taskID := s.settledCompletedTask(t)
			cdn := cdnStatusServer(t, tc.status)
			setPollResultURL(s, cdn.URL+"/r.mp4")
			fake := &fakeObjectStore{}
			svc := s.newServiceWithStore(t, fakeFactory(fake), httpClient())

			err := svc.storeResult(ctx, taskID)
			if tc.wantRetry {
				require.Error(t, err, "瞬时状态码 → 返 error 触发 asynq 重试")
			} else {
				require.NoError(t, err, "永久状态码 → 返 nil 不重试（转人工对账）")
			}
			assert.Equal(t, 0, fake.putCount(), "非 200 不上传")
			_, metaErr := s.q.GetOSSObjectMetaByTask(ctx, taskID)
			require.Error(t, metaErr, "未落 meta")
		})
	}
}

// TestStoreResult_MissingContentLength 上游 200 但无 Content-Length（分块）→ 永久失败（无法定大小）。
func TestStoreResult_MissingContentLength(t *testing.T) {
	s := setupTaskSuite(t)
	ctx := context.Background()
	taskID := s.settledCompletedTask(t)
	cdn := cdnChunkedServer(t)
	setPollResultURL(s, cdn.URL+"/r.mp4")
	fake := &fakeObjectStore{}
	svc := s.newServiceWithStore(t, fakeFactory(fake), httpClient())

	require.NoError(t, svc.storeResult(ctx, taskID), "缺 Content-Length → 永久失败转人工对账")
	assert.Equal(t, 0, fake.putCount())
}

// TestStoreResult_OversizeContentLength Content-Length 超 maxResultBytes → 永久失败（防 OOM）。
func TestStoreResult_OversizeContentLength(t *testing.T) {
	s := setupTaskSuite(t)
	ctx := context.Background()
	taskID := s.settledCompletedTask(t)
	cdn := cdnServer(t, make([]byte, 100)) // Content-Length=100
	setPollResultURL(s, cdn.URL+"/r.mp4")
	fake := &fakeObjectStore{}
	svc, err := NewService(Config{
		Pool: s.pool, Ledger: s.ledgerSvc, Adapter: s.adapter, Catalog: s.catalog,
		Creds: fullTOSCreds(), Enqueuer: s.enq, Logger: silentLog(),
		ObjectStoreFactory: fakeFactory(fake), ResultHTTPClient: httpClient(),
		AllowPrivateResultHost: true, MaxResultBytes: 10, // 上限 10 < 100
		SettleTimeout: 2 * time.Second, PollTimeout: 2 * time.Second, WorkerID: "test",
	})
	require.NoError(t, err)

	require.NoError(t, svc.storeResult(ctx, taskID), "产物超上限 → 永久失败转人工对账")
	assert.Equal(t, 0, fake.putCount())
}

// TestStoreResult_SSRFBlocked 产物 URL 解析到内网/环回（生产 allowPrivateResultHost=false）→ 拒绝转存。
func TestStoreResult_SSRFBlocked(t *testing.T) {
	s := setupTaskSuite(t)
	ctx := context.Background()
	taskID := s.settledCompletedTask(t)
	setPollResultURL(s, "http://169.254.169.254/latest/meta-data/") // 元数据端点
	fake := &fakeObjectStore{}
	// 生产配置：不放行私网（AllowPrivateResultHost 默认 false）。
	svc, err := NewService(Config{
		Pool: s.pool, Ledger: s.ledgerSvc, Adapter: s.adapter, Catalog: s.catalog,
		Creds: fullTOSCreds(), Enqueuer: s.enq, Logger: silentLog(),
		ObjectStoreFactory: fakeFactory(fake), ResultHTTPClient: httpClient(),
		SettleTimeout: 2 * time.Second, PollTimeout: 2 * time.Second, WorkerID: "test",
	})
	require.NoError(t, err)

	require.NoError(t, svc.storeResult(ctx, taskID), "SSRF 拒绝是永久失败转人工对账")
	assert.Equal(t, 0, fake.putCount(), "内网 URL 不被 fetch")
}

func TestValidateResultURL(t *testing.T) {
	block := &Service{} // allowPrivateResultHost=false（生产）
	for _, bad := range []string{
		"ftp://host/x",             // 非 http(s)
		"https://127.0.0.1/x",      // 环回
		"http://10.1.2.3/x",        // 私网
		"http://169.254.169.254/x", // 链路本地（元数据）
		"http://[::1]/x",           // IPv6 环回
	} {
		assert.Error(t, block.validateResultURL(bad), "应拒绝: %s", bad)
	}
	assert.NoError(t, block.validateResultURL("https://8.8.8.8/x"), "公网 IP 放行")

	allow := &Service{allowPrivateResultHost: true} // 测试放行
	assert.NoError(t, allow.validateResultURL("http://127.0.0.1:8080/x"), "放行环回（测试）")
	assert.Error(t, allow.validateResultURL("ftp://127.0.0.1/x"), "scheme 仍校验")
}

func TestBuildResultObjectKey_Sanitize(t *testing.T) {
	cases := []struct{ projectID, taskID, want string }{
		{"proj-1", "vtask_abc", "proj-1/video/vtask_abc.mp4"},
		{"../evil", "vtask_x", "evil/video/vtask_x.mp4"},     // 剔除 ../
		{"a/b/c", "vtask_x", "abc/video/vtask_x.mp4"},        // 剔除 /
		{"....", "vtask_x", "video/vtask_x.mp4"},             // 全剔除 → 无前缀
		{"", "vtask_x", "video/vtask_x.mp4"},                 // 空 projectID
		{"  p r o j  ", "vtask_x", "proj/video/vtask_x.mp4"}, // 剔除空白
	}
	for _, tc := range cases {
		got := buildResultObjectKey(tc.projectID, tc.taskID)
		assert.Equal(t, tc.want, got)
		assert.NotContains(t, got, "..", "无路径穿越")
	}
}

// TestRecoverMissingStore_NegativeFilters 校验扫描的排除谓词：超 24h 窗 / 新鲜 updated_at / 失败来源
// 均不被重投。
func TestRecoverMissingStore_NegativeFilters(t *testing.T) {
	cases := []struct {
		name string
		sql  string // 摆任务状态的 UPDATE 片段（WHERE id=$1 由调用补）
	}{
		{"url_window_expired", `status='SETTLED', error_code=NULL, upstream_task_id='c',
			terminal_at=NOW()-interval '25 hours', updated_at=NOW()-interval '10 minutes'`},
		{"fresh_updated_at", `status='SETTLED', error_code=NULL, upstream_task_id='c',
			terminal_at=NOW()-interval '1 hour', updated_at=NOW()`},
		{"failure_origin", `status='SETTLED', error_code='upstream_failed', upstream_task_id='c',
			terminal_at=NOW()-interval '1 hour', updated_at=NOW()-interval '10 minutes'`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := setupTaskSuite(t)
			ctx := context.Background()
			s.seedAccount(t, 100_000)
			taskID, err := s.svc.Submit(ctx, s.submitParams(1000))
			require.NoError(t, err)
			_, err = s.pool.Exec(ctx, `UPDATE task SET `+tc.sql+` WHERE id=$1`, taskID)
			require.NoError(t, err)

			fake := &fakeObjectStore{}
			svc := s.newServiceWithStore(t, fakeFactory(fake), httpClient())
			require.NoError(t, svc.recoverMissingStore(ctx))
			assert.Equal(t, 0, s.enq.storeCount(), "被排除谓词过滤，不重投 store")
		})
	}
}

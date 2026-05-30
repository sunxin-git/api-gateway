package task

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sunxin-git/api-gateway/internal/channel"
	"github.com/sunxin-git/api-gateway/internal/db"
	"github.com/sunxin-git/api-gateway/internal/relay/video"
	"github.com/sunxin-git/api-gateway/internal/storage"
)

// ---------- fake ObjectStore + creds ----------

type fakePut struct {
	key         string
	contentType string
	data        []byte
}

type fakeObjectStore struct {
	mu     sync.Mutex
	puts   []fakePut
	putErr error // 注入 Put 错误（如 storage.ErrObjectExists）
}

func (f *fakeObjectStore) Put(_ context.Context, key string, body io.Reader, _ int64, contentType string) error {
	data, _ := io.ReadAll(body)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.putErr != nil {
		return f.putErr
	}
	f.puts = append(f.puts, fakePut{key: key, contentType: contentType, data: data})
	return nil
}

func (f *fakeObjectStore) PresignGet(key string, _ time.Duration) (string, error) {
	return "https://signed.example/" + key, nil
}
func (f *fakeObjectStore) Bucket() string   { return "gw-results" }
func (f *fakeObjectStore) Region() string   { return "cn-beijing" }
func (f *fakeObjectStore) Endpoint() string { return "https://tos-cn-beijing.volces.com" }

func (f *fakeObjectStore) putCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.puts)
}

func fakeFactory(fake storage.ObjectStore) func(storage.TOSConfig) (storage.ObjectStore, error) {
	return func(storage.TOSConfig) (storage.ObjectStore, error) { return fake, nil }
}

// storeTestCreds 返回带 TOS 字段的凭据（store 需要 TOS AK/SK/bucket/endpoint/region）。
type storeTestCreds struct{ cc channel.ChannelCredentials }

func (c storeTestCreds) GetCredentialsForUpstream(_ context.Context, _ int64) (*channel.ChannelCredentials, error) {
	cp := c.cc
	return &cp, nil
}

func fullTOSCreds() storeTestCreds {
	return storeTestCreds{cc: channel.ChannelCredentials{
		APIKey:       "ark-key",
		TOSAccessKey: "tos-ak",
		TOSSecretKey: "tos-sk",
		TOSBucket:    "gw-results",
		TOSEndpoint:  "https://tos-cn-beijing.volces.com",
		TOSRegion:    "cn-beijing",
		ProjectID:    "proj-1",
	}}
}

// pollSucceeded 构造一个 Succeeded + 指定产物 URL 的 PollResult。
func pollSucceeded(resultURL string) *video.PollResult {
	return &video.PollResult{
		Status:    video.UpstreamSucceeded,
		Usage:     &video.UpstreamUsage{CompletionTokens: 100_000},
		ResultURL: resultURL,
	}
}

func setPollResultURL(s *taskSuite, resultURL string) {
	s.adapter.pollFn = func(_ context.Context, _ *video.VideoModelEntry, _ video.UpstreamCredentials, _ string) (*video.PollResult, error) {
		return pollSucceeded(resultURL), nil
	}
}

// newServiceWithStore 复用 suite 依赖构造一个启用结果转存的 Service（注入 fake 工厂 + 凭据 + http 客户端）。
func (s *taskSuite) newServiceWithStore(
	t *testing.T,
	factory func(storage.TOSConfig) (storage.ObjectStore, error),
	client *http.Client,
) *Service {
	t.Helper()
	svc, err := NewService(Config{
		Pool:                   s.pool,
		Ledger:                 s.ledgerSvc,
		Adapter:                s.adapter,
		Catalog:                s.catalog,
		Creds:                  fullTOSCreds(),
		Enqueuer:               s.enq,
		Logger:                 silentLog(),
		ObjectStoreFactory:     factory,
		ResultHTTPClient:       client,
		AllowPrivateResultHost: true, // httptest CDN 走 127.0.0.1，放行环回
		SettleTimeout:          2 * time.Second,
		PollTimeout:            2 * time.Second,
		StoreNeedingStoreAge:   time.Minute,
		WorkerID:               "test-store",
	})
	require.NoError(t, err)
	return svc
}

// cdnServer httptest 模拟上游产物 CDN，返回固定 mp4 字节 + Content-Length。
func cdnServer(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// settledCompletedTask 提交并摆到 SETTLED（COMPLETED 来源：error_code 空）+ upstream_task_id。
func (s *taskSuite) settledCompletedTask(t *testing.T) string {
	t.Helper()
	s.seedAccount(t, 100_000)
	taskID, err := s.svc.Submit(context.Background(), s.submitParams(1000))
	require.NoError(t, err)
	s.directSetStatus(t, taskID, db.TaskStatusSETTLED, "cgt-store-1")
	return taskID
}

func httpClient() *http.Client { return &http.Client{Timeout: 5 * time.Second} }

// ---------- 测试 ----------

func TestStoreResult_HappyPath(t *testing.T) {
	s := setupTaskSuite(t)
	ctx := context.Background()
	taskID := s.settledCompletedTask(t)

	videoBytes := []byte("FAKE-MP4-DATA-0123456789-abcdef")
	cdn := cdnServer(t, videoBytes)
	setPollResultURL(s, cdn.URL+"/result.mp4")

	var capturedCfg storage.TOSConfig
	fake := &fakeObjectStore{}
	factory := func(cfg storage.TOSConfig) (storage.ObjectStore, error) {
		capturedCfg = cfg
		return fake, nil
	}
	svc := s.newServiceWithStore(t, factory, httpClient())

	require.NoError(t, svc.storeResult(ctx, taskID))

	// 产物上传 + 凭据流转正确。
	require.Equal(t, 1, fake.putCount())
	assert.Equal(t, "tos-ak", capturedCfg.AccessKey, "工厂收到 channel TOS 凭据")
	assert.Equal(t, videoBytes, fake.puts[0].data, "上传字节 = 上游产物字节")
	assert.Equal(t, "video/mp4", fake.puts[0].contentType)
	assert.Contains(t, fake.puts[0].key, taskID, "对象 key 含 task_id（不可枚举随机段）")
	assert.Contains(t, fake.puts[0].key, "proj-1", "对象 key 含 project_id 隔离前缀")

	// oss_object_meta 落库。
	meta, err := s.q.GetOSSObjectMetaByTask(ctx, taskID)
	require.NoError(t, err)
	assert.Equal(t, "gw-results", meta.Bucket)
	assert.Equal(t, fake.puts[0].key, meta.ObjectKey)
	assert.Equal(t, int64(len(videoBytes)), meta.SizeBytes)
	assert.Equal(t, s.accountID, meta.BusinessAccountID)
}

func TestStoreResult_Idempotent(t *testing.T) {
	s := setupTaskSuite(t)
	ctx := context.Background()
	taskID := s.settledCompletedTask(t)
	cdn := cdnServer(t, []byte("vvvv"))
	setPollResultURL(s, cdn.URL+"/r.mp4")
	fake := &fakeObjectStore{}
	svc := s.newServiceWithStore(t, fakeFactory(fake), httpClient())

	require.NoError(t, svc.storeResult(ctx, taskID))
	require.NoError(t, svc.storeResult(ctx, taskID)) // 重投
	assert.Equal(t, 1, fake.putCount(), "已有 meta → 第二次跳过，不重复上传")
}

func TestStoreResult_ObjectExistsContinuesToMeta(t *testing.T) {
	s := setupTaskSuite(t)
	ctx := context.Background()
	taskID := s.settledCompletedTask(t)
	cdn := cdnServer(t, []byte("vvvv"))
	setPollResultURL(s, cdn.URL+"/r.mp4")
	// Put 返回 ErrObjectExists（上次传成功但写 meta 前失败 → 重投命中）。
	fake := &fakeObjectStore{putErr: storage.ErrObjectExists}
	svc := s.newServiceWithStore(t, fakeFactory(fake), httpClient())

	require.NoError(t, svc.storeResult(ctx, taskID), "ErrObjectExists 视为幂等成功，续写 meta")
	_, err := s.q.GetOSSObjectMetaByTask(ctx, taskID)
	require.NoError(t, err, "meta 已续写")
}

func TestStoreResult_NoResultURL_ManualReconcile(t *testing.T) {
	s := setupTaskSuite(t)
	ctx := context.Background()
	taskID := s.settledCompletedTask(t)
	setPollResultURL(s, "") // 上游无产物 URL
	fake := &fakeObjectStore{}
	svc := s.newServiceWithStore(t, fakeFactory(fake), httpClient())

	require.NoError(t, svc.storeResult(ctx, taskID), "无产物 URL → 永久失败转人工对账（不重试）")
	assert.Equal(t, 0, fake.putCount())
	_, err := s.q.GetOSSObjectMetaByTask(ctx, taskID)
	require.Error(t, err, "无 meta")
}

func TestStoreResult_FailureOriginSkipped(t *testing.T) {
	s := setupTaskSuite(t)
	ctx := context.Background()
	s.seedAccount(t, 100_000)
	taskID, err := s.svc.Submit(ctx, s.submitParams(1000))
	require.NoError(t, err)
	// 失败来源 SETTLED：error_code 非空 → 无产物。
	_, err = s.pool.Exec(ctx,
		`UPDATE task SET status='SETTLED', error_code='upstream_failed', upstream_task_id='cgt-x' WHERE id=$1`, taskID)
	require.NoError(t, err)

	fake := &fakeObjectStore{}
	svc := s.newServiceWithStore(t, fakeFactory(fake), httpClient())
	require.NoError(t, svc.storeResult(ctx, taskID))
	assert.Equal(t, 0, fake.putCount(), "失败来源无产物，不转存")
}

// TestSettle_EnqueuesStore 成功结算（COMPLETED→SETTLED）后自动入队 store（settle→store 接线）。
func TestSettle_EnqueuesStore(t *testing.T) {
	s := setupTaskSuite(t)
	ctx := context.Background()
	s.seedAccount(t, 100_000)
	taskID, err := s.svc.Submit(ctx, s.submitParams(1000))
	require.NoError(t, err)
	s.directSetStatus(t, taskID, db.TaskStatusCOMPLETED, "cgt-settle-store")
	setPollResultURL(s, "https://cdn.example/r.mp4") // settle 的 pollUsage 取 usage

	fake := &fakeObjectStore{}
	svc := s.newServiceWithStore(t, fakeFactory(fake), httpClient())

	require.NoError(t, svc.settleTask(ctx, taskID))
	assert.Equal(t, db.TaskStatusSETTLED, s.getTask(t, taskID).Status)
	assert.Equal(t, 1, s.enq.storeCount(), "成功结算后入队 store")
}

func TestRecoverMissingStore_ReenqueuesStaleSettled(t *testing.T) {
	s := setupTaskSuite(t)
	ctx := context.Background()
	s.seedAccount(t, 100_000)
	taskID, err := s.svc.Submit(ctx, s.submitParams(1000))
	require.NoError(t, err)
	// SETTLED（COMPLETED 来源）、terminal_at 在 24h 窗内、updated_at 超阈值、无 meta。
	_, err = s.pool.Exec(ctx,
		`UPDATE task SET status='SETTLED', error_code=NULL, upstream_task_id='cgt-x',
		 terminal_at=NOW()-interval '1 hour', updated_at=NOW()-interval '10 minutes' WHERE id=$1`, taskID)
	require.NoError(t, err)

	fake := &fakeObjectStore{}
	svc := s.newServiceWithStore(t, fakeFactory(fake), httpClient())

	require.NoError(t, svc.recoverMissingStore(ctx))
	assert.Equal(t, 1, s.enq.storeCount(), "丢失 store 的 SETTLED 任务被重投")
}

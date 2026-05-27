package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// AuditRecord JSON 序列化
// =============================================================================

func TestAuditRecord_JSONShape(t *testing.T) {
	rec := AuditRecord{
		Event:            "admin_audit",
		Tier:             Tier1,
		RequestID:        "rid-1",
		TimestampUTC:     time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC),
		TokenID:          42,
		TokenDescription: "test",
		Actor:            "admin_token:42",
		SourceIP:         "10.0.0.1",
		Method:           "POST",
		Path:             "/admin/v1/business-accounts/:id/refund",
		RequestHash:      "abcdef0123456789abcdef0123456789",
		BodySizeBytes:    123,
		Status:           200,
		DurationMs:       12,
		OutcomeCode:      "ok",
	}
	b, err := json.Marshal(rec)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(b, &got))
	assert.Equal(t, "admin_audit", got["event"])
	assert.EqualValues(t, 1, got["tier"])
	assert.Equal(t, "admin_token:42", got["actor"])
	assert.Equal(t, "/admin/v1/business-accounts/:id/refund", got["path"])
}

// =============================================================================
// SyncFileSink
// =============================================================================

func TestSyncFileSink_WritesAndFsyncs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")
	sink, err := NewSyncFileSink(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sink.Close() })

	rec := AuditRecord{Event: "admin_audit", Tier: Tier1, RequestID: "r1", OutcomeCode: "ok"}
	require.NoError(t, sink.Emit(context.Background(), rec))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	// JSON Lines：必须以 \n 结尾，单行
	assert.True(t, bytes.HasSuffix(data, []byte("\n")))
	lines := bytes.Split(bytes.TrimRight(data, "\n"), []byte("\n"))
	require.Len(t, lines, 1)
	var got map[string]any
	require.NoError(t, json.Unmarshal(lines[0], &got))
	assert.Equal(t, "admin_audit", got["event"])
	assert.Equal(t, "r1", got["request_id"])
}

func TestSyncFileSink_AppendsMultipleLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")
	sink, err := NewSyncFileSink(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sink.Close() })

	for i := 0; i < 3; i++ {
		rec := AuditRecord{Event: "admin_audit", Tier: Tier1, RequestID: "r" + string(rune('a'+i))}
		require.NoError(t, sink.Emit(context.Background(), rec))
	}
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	lines := bytes.Split(bytes.TrimRight(data, "\n"), []byte("\n"))
	assert.Len(t, lines, 3)
}

func TestSyncFileSink_ConcurrentWrites_NoInterleave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")
	sink, err := NewSyncFileSink(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sink.Close() })

	const N = 100
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			rec := AuditRecord{Event: "admin_audit", Tier: Tier1, OutcomeCode: "ok"}
			require.NoError(t, sink.Emit(context.Background(), rec))
		}()
	}
	wg.Wait()

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	lines := bytes.Split(bytes.TrimRight(data, "\n"), []byte("\n"))
	assert.Len(t, lines, N)
	// 每行必须是合法 JSON（无半行交错）
	for _, ln := range lines {
		var v map[string]any
		require.NoError(t, json.Unmarshal(ln, &v))
	}
}

func TestSyncFileSink_EmptyPathRejects(t *testing.T) {
	_, err := NewSyncFileSink("")
	require.Error(t, err)
}

func TestSyncFileSink_NonexistentDirFails(t *testing.T) {
	_, err := NewSyncFileSink("/nonexistent-dir-1234/audit.log")
	require.Error(t, err)
}

func TestSyncFileSink_CloseIdempotent(t *testing.T) {
	dir := t.TempDir()
	sink, err := NewSyncFileSink(filepath.Join(dir, "x.log"))
	require.NoError(t, err)
	require.NoError(t, sink.Close())
	require.NoError(t, sink.Close(), "二次 Close 必须幂等")
}

// =============================================================================
// AsyncStderrSink
// =============================================================================

func TestAsyncStderrSink_WritesJSONWithAttrs(t *testing.T) {
	var buf bytes.Buffer
	sink := newAsyncSinkTo(&buf)
	rec := AuditRecord{
		Event:       "admin_audit",
		Tier:        Tier2,
		RequestID:   "r2",
		Method:      "GET",
		Path:        "/admin/v1/x",
		Status:      200,
		OutcomeCode: "ok",
	}
	require.NoError(t, sink.Emit(context.Background(), rec))
	out := buf.String()
	// 直接 json.Marshal(record)，不再有 slog 框架外层 time/level/msg
	assert.Contains(t, out, "\"event\":\"admin_audit\"")
	assert.Contains(t, out, "\"request_id\":\"r2\"")
	assert.Contains(t, out, "\"tier\":2")
	assert.Contains(t, out, "\"path\":\"/admin/v1/x\"")
}

func TestAsyncStderrSink_ExternalRefInjectionEscaped(t *testing.T) {
	var buf bytes.Buffer
	sink := newAsyncSinkTo(&buf)
	rec := AuditRecord{
		Event:       "admin_audit",
		Tier:        Tier2,
		Reason:      "evil\n{\"injected\":true}",
		OutcomeCode: "ok",
	}
	require.NoError(t, sink.Emit(context.Background(), rec))
	// 输出必须是单行 JSON（\n 转义为字面 \n）
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	assert.Len(t, lines, 1, "slog.JSONHandler 必须把 \\n 转义；不能允许注入新 log line")
}

// =============================================================================
// Logger 路由
// =============================================================================

type recordingSink struct {
	mu     sync.Mutex
	emits  []AuditRecord
	failOn atomic.Int32 // > 0 = 注入 N 次失败
}

func (s *recordingSink) Emit(_ context.Context, r AuditRecord) error {
	if s.failOn.Load() > 0 {
		s.failOn.Add(-1)
		return errors.New("recording sink injected error")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.emits = append(s.emits, r)
	return nil
}
func (s *recordingSink) Close() error { return nil }
func (s *recordingSink) snapshot() []AuditRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]AuditRecord, len(s.emits))
	copy(out, s.emits)
	return out
}

func TestLogger_RoutesTier1ToTier1Sink(t *testing.T) {
	t1, t2 := &recordingSink{}, &recordingSink{}
	l := NewLogger(t1, t2, nil)
	require.NoError(t, l.Emit(context.Background(), AuditRecord{Tier: Tier1}))
	assert.Len(t, t1.snapshot(), 1)
	assert.Empty(t, t2.snapshot())
}

func TestLogger_RoutesTier2ToTier2Sink(t *testing.T) {
	t1, t2 := &recordingSink{}, &recordingSink{}
	l := NewLogger(t1, t2, nil)
	require.NoError(t, l.Emit(context.Background(), AuditRecord{Tier: Tier2}))
	assert.Empty(t, t1.snapshot())
	assert.Len(t, t2.snapshot(), 1)
}

func TestLogger_UnknownTierFailsClosed(t *testing.T) {
	l := NewLogger(&recordingSink{}, &recordingSink{}, nil)
	err := l.Emit(context.Background(), AuditRecord{Tier: TierUnknown})
	require.Error(t, err)
}

func TestLogger_Tier1NoSinkReturnsError(t *testing.T) {
	l := NewLogger(nil, &recordingSink{}, nil)
	err := l.Emit(context.Background(), AuditRecord{Tier: Tier1})
	require.Error(t, err, "Tier1 sink 缺失必须 fail-closed")
}

func TestLogger_Tier2SinkFailureSwallowedAndWarned(t *testing.T) {
	t2 := &recordingSink{}
	t2.failOn.Store(1)
	var failBuf bytes.Buffer
	failLogger := slog.New(slog.NewJSONHandler(&failBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	l := NewLogger(&recordingSink{}, t2, failLogger)
	err := l.Emit(context.Background(), AuditRecord{Tier: Tier2, RequestID: "r-x"})
	require.NoError(t, err, "Tier2 失败必须 best-effort 不返 error")
	assert.Contains(t, failBuf.String(), "r-x", "Tier2 失败应 warn 包含 request_id")
}

func TestLogger_Tier1SinkFailurePropagated(t *testing.T) {
	t1 := &recordingSink{}
	t1.failOn.Store(1)
	l := NewLogger(t1, &recordingSink{}, nil)
	err := l.Emit(context.Background(), AuditRecord{Tier: Tier1})
	require.Error(t, err, "Tier1 sink 失败必须传播给 caller 升级告警")
}

func TestLogger_CloseClosesBothSinks(t *testing.T) {
	dir := t.TempDir()
	t1, err := NewSyncFileSink(filepath.Join(dir, "audit.log"))
	require.NoError(t, err)
	t2 := NewAsyncStderrSink()
	l := NewLogger(t1, t2, nil)
	require.NoError(t, l.Close())
	// 二次 Close（通过底层 sink 直接调）应幂等
	require.NoError(t, t1.Close())
}

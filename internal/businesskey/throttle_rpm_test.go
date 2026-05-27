package businesskey

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// =============================================================================
// InProcessRPM —— 不依赖 PG，复用 admintoken 测试模式
// =============================================================================

func TestRPM_HappyPath(t *testing.T) {
	rpm := newTestRPM()
	t.Cleanup(func() { _ = rpm.Close() })

	limit := int32(5)
	key := &Key{ID: 100, RequestsPerMinute: &limit}

	for i := 0; i < 5; i++ {
		require.NoError(t, rpm.Check(key), "第 %d 次必须通过", i+1)
	}
	require.ErrorIs(t, rpm.Check(key), ErrRPMExceeded, "第 6 次必须拒绝")
}

func TestRPM_NilLimitNoLimit(t *testing.T) {
	rpm := newTestRPM()
	t.Cleanup(func() { _ = rpm.Close() })
	key := &Key{ID: 1, RequestsPerMinute: nil}
	for i := 0; i < 1000; i++ {
		require.NoError(t, rpm.Check(key))
	}
}

func TestRPM_ZeroLimitAlwaysDeny(t *testing.T) {
	rpm := newTestRPM()
	t.Cleanup(func() { _ = rpm.Close() })
	zero := int32(0)
	key := &Key{ID: 2, RequestsPerMinute: &zero}
	require.ErrorIs(t, rpm.Check(key), ErrRPMExceeded)
}

func TestRPM_RejectedRequestsDoNotConsumeQuota(t *testing.T) {
	rpm := newTestRPM()
	t.Cleanup(func() { _ = rpm.Close() })
	limit := int32(3)
	key := &Key{ID: 3, RequestsPerMinute: &limit}

	for i := 0; i < 3; i++ {
		require.NoError(t, rpm.Check(key))
	}
	require.ErrorIs(t, rpm.Check(key), ErrRPMExceeded)
	require.ErrorIs(t, rpm.Check(key), ErrRPMExceeded)

	require.Equal(t, 3, rpm.peekCount(key.ID))
}

func TestRPM_WindowSliding(t *testing.T) {
	rpm := newTestRPM()
	t.Cleanup(func() { _ = rpm.Close() })

	nowNs := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC).UnixNano()
	rpm.now = func() time.Time { return time.Unix(0, atomic.LoadInt64(&nowNs)) }

	limit := int32(2)
	key := &Key{ID: 10, RequestsPerMinute: &limit}

	require.NoError(t, rpm.Check(key))
	require.NoError(t, rpm.Check(key))
	require.ErrorIs(t, rpm.Check(key), ErrRPMExceeded)

	atomic.AddInt64(&nowNs, int64(61*time.Second))
	require.NoError(t, rpm.Check(key), "窗口滑出后应再次通过")
	require.NoError(t, rpm.Check(key))
	require.ErrorIs(t, rpm.Check(key), ErrRPMExceeded)
}

func TestRPM_BurstWithinSecond(t *testing.T) {
	rpm := newTestRPM()
	t.Cleanup(func() { _ = rpm.Close() })

	nowNs := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC).UnixNano()
	rpm.now = func() time.Time { return time.Unix(0, atomic.LoadInt64(&nowNs)) }

	limit := int32(50)
	key := &Key{ID: 20, RequestsPerMinute: &limit}

	for i := 0; i < 50; i++ {
		require.NoError(t, rpm.Check(key))
		atomic.AddInt64(&nowNs, int64(time.Millisecond))
	}
	require.ErrorIs(t, rpm.Check(key), ErrRPMExceeded)
}

func TestRPM_GCRemovesIdleKeys(t *testing.T) {
	rpm := newTestRPM()
	t.Cleanup(func() { _ = rpm.Close() })

	nowNs := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC).UnixNano()
	rpm.now = func() time.Time { return time.Unix(0, atomic.LoadInt64(&nowNs)) }
	rpm.idleThreshold = time.Minute

	limit := int32(10)
	key := &Key{ID: 99, RequestsPerMinute: &limit}
	require.NoError(t, rpm.Check(key))
	require.Equal(t, 1, rpm.peekCount(key.ID))

	atomic.AddInt64(&nowNs, int64(2*time.Minute))
	rpm.gcOnce()
	require.Equal(t, 0, rpm.peekCount(key.ID), "idle key 的 state 应被 GC")
}

func TestRPM_ConcurrentSafe(t *testing.T) {
	rpm := newTestRPM()
	t.Cleanup(func() { _ = rpm.Close() })

	limit := int32(1000)
	key := &Key{ID: 50, RequestsPerMinute: &limit}

	var wg sync.WaitGroup
	const N = 200
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			_ = rpm.Check(key)
		}()
	}
	wg.Wait()
	require.Equal(t, N, rpm.peekCount(key.ID))
}

func TestRPM_ColdStartHookFiresOnce(t *testing.T) {
	var n int32
	rpm := NewInProcessRPM(newSilentLogger(), func() { atomic.AddInt32(&n, 1) })
	t.Cleanup(func() { _ = rpm.Close() })
	require.Equal(t, int32(1), atomic.LoadInt32(&n))
}

func TestRPM_PanicOnNilLogger(t *testing.T) {
	defer func() {
		require.NotNil(t, recover(), "nil logger 必须 panic")
	}()
	_ = NewInProcessRPM(nil, nil)
}

func TestRPM_CloseIdempotent(t *testing.T) {
	rpm := NewInProcessRPM(newSilentLogger(), nil)
	require.NoError(t, rpm.Close())
	require.NoError(t, rpm.Close(), "二次 Close 必须幂等")
}

// newTestRPM 构造测试用 InProcessRPM（不启 GC goroutine 避免 race）。
func newTestRPM() *InProcessRPM {
	return newInProcessRPMBase(newSilentLogger(), nil)
}

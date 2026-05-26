package obs

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewLogger(t *testing.T) {
	t.Run("合法等级返回非空 logger", func(t *testing.T) {
		for _, lvl := range []string{"debug", "info", "warn", "error"} {
			lg, err := NewLogger(lvl)
			require.NoError(t, err)
			require.NotNil(t, lg)
		}
	})

	t.Run("非法等级返回错误", func(t *testing.T) {
		_, err := NewLogger("trace")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "非法日志等级")
	})

	t.Run("JSON 格式且包含字段", func(t *testing.T) {
		var buf bytes.Buffer
		lg := newLoggerWithWriter(&buf, slog.LevelInfo)
		lg.Info("hello", "k", "v")

		var got map[string]any
		require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
		assert.Equal(t, "hello", got["msg"])
		assert.Equal(t, "v", got["k"])
		assert.Equal(t, "INFO", got["level"])
	})

	t.Run("等级过滤生效", func(t *testing.T) {
		var buf bytes.Buffer
		lg := newLoggerWithWriter(&buf, slog.LevelWarn)
		lg.Info("info-msg")
		lg.Warn("warn-msg")
		out := buf.String()
		assert.NotContains(t, out, "info-msg")
		assert.Contains(t, out, "warn-msg")
	})
}

func TestNewMetrics(t *testing.T) {
	m := NewMetrics("v0.0.1-test", "abc123")
	require.NotNil(t, m)

	// build_info 初始即为 1
	val := testutil.ToFloat64(m.BuildInfo.WithLabelValues("v0.0.1-test", "abc123"))
	assert.Equal(t, float64(1), val)

	// panic_total 初始 0；自增后 1
	assert.Equal(t, float64(0), testutil.ToFloat64(m.PanicTotal))
	m.PanicTotal.Inc()
	assert.Equal(t, float64(1), testutil.ToFloat64(m.PanicTotal))

	// HTTPRequestDuration 可正常 Observe
	m.HTTPRequestDuration.WithLabelValues("GET", "/healthz", "200").Observe(0.01)

	// Registry 收集到 3 个 metric family
	mfs, err := m.Registry.Gather()
	require.NoError(t, err)
	names := []string{}
	for _, mf := range mfs {
		names = append(names, mf.GetName())
	}
	assert.Contains(t, names, "gateway_http_request_duration_seconds")
	assert.Contains(t, names, "gateway_build_info")
	assert.Contains(t, names, "gateway_panic_total")
}

func TestNewTracerProvider(t *testing.T) {
	t.Run("stdout exporter 成功", func(t *testing.T) {
		tp, err := NewTracerProvider("stdout")
		require.NoError(t, err)
		require.NotNil(t, tp)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		// Shutdown 不应 panic（即使 ctx 已 cancel）
		_ = tp.Shutdown(ctx)
	})

	t.Run("otlp 报未实现", func(t *testing.T) {
		_, err := NewTracerProvider("otlp")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "Phase 1")
	})

	t.Run("非法值报错", func(t *testing.T) {
		_, err := NewTracerProvider("jaeger")
		require.Error(t, err)
		assert.True(t, strings.Contains(err.Error(), "非法"))
	})
}

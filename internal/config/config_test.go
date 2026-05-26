package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setMinimalRequiredEnv 设置三项必填环境变量，让 validate 通过。
// 返回一个清理函数（defer 调用）。
func setMinimalRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("PG_DSN", "postgres://test:test@localhost:5432/test?sslmode=disable")
	t.Setenv("GATEWAY_KEK_V1", "dGVzdC1rZWstdjE=") // base64("test-kek-v1")
	t.Setenv("ADMIN_TOKEN_SIGNING_KEY", "test-admin-signing-key")
}

func TestLoad默认值生效(t *testing.T) {
	clearGatewayEnv(t)
	setMinimalRequiredEnv(t)

	cfg, err := Load("")
	require.NoError(t, err)
	require.NotNil(t, cfg)

	assert.Equal(t, ":8080", cfg.HTTPAddr)
	assert.Equal(t, "info", cfg.LogLevel)
	assert.Equal(t, "localhost:6379", cfg.RedisAddr)
	assert.Equal(t, "stdout", cfg.OTelExporter)
	assert.Empty(t, cfg.CORSAllowedOrigins, "默认应为空白名单 = 拒绝所有跨域")
}

func TestLoad环境变量覆盖默认值(t *testing.T) {
	clearGatewayEnv(t)
	setMinimalRequiredEnv(t)
	t.Setenv("HTTP_ADDR", ":9090")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("REDIS_ADDR", "redis.internal:6380")
	t.Setenv("OTEL_EXPORTER", "otlp")
	t.Setenv("CORS_ALLOWED_ORIGINS", "https://a.example.com, https://b.example.com ,,  ")

	cfg, err := Load("")
	require.NoError(t, err)

	assert.Equal(t, ":9090", cfg.HTTPAddr)
	assert.Equal(t, "debug", cfg.LogLevel)
	assert.Equal(t, "redis.internal:6380", cfg.RedisAddr)
	assert.Equal(t, "otlp", cfg.OTelExporter)
	assert.Equal(t, []string{"https://a.example.com", "https://b.example.com"}, cfg.CORSAllowedOrigins)
}

func TestLoad缺少必填项时报错(t *testing.T) {
	cases := []struct {
		name        string
		unsetKey    string
		wantInError string
	}{
		{"缺 PG_DSN", "PG_DSN", "PGDSN"},
		{"缺 GATEWAY_KEK_V1", "GATEWAY_KEK_V1", "GATEWAY_KEK_V1"},
		{"缺 ADMIN_TOKEN_SIGNING_KEY", "ADMIN_TOKEN_SIGNING_KEY", "ADMIN_TOKEN_SIGNING_KEY"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearGatewayEnv(t)
			setMinimalRequiredEnv(t)
			// 强制清掉某一项。
			t.Setenv(tc.unsetKey, "")

			_, err := Load("")
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantInError)
			assert.Contains(t, err.Error(), "配置缺失必填项")
		})
	}
}

func TestLoad非法OTelExporter报错(t *testing.T) {
	clearGatewayEnv(t)
	setMinimalRequiredEnv(t)
	t.Setenv("OTEL_EXPORTER", "jaeger")

	_, err := Load("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "OTEL_EXPORTER")
}

func TestLoad非法LogLevel报错(t *testing.T) {
	clearGatewayEnv(t)
	setMinimalRequiredEnv(t)
	t.Setenv("LOG_LEVEL", "trace")

	_, err := Load("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "LOG_LEVEL")
}

func TestLoadDotenv文件加载env覆盖文件(t *testing.T) {
	clearGatewayEnv(t)
	// 在临时目录写一个 .env.local。
	dir := t.TempDir()
	envFile := filepath.Join(dir, ".env.local")
	content := `HTTP_ADDR=:7070
LOG_LEVEL=warn
PG_DSN=postgres://from-file
GATEWAY_KEK_V1=kek-from-file
ADMIN_TOKEN_SIGNING_KEY=signkey-from-file
`
	require.NoError(t, os.WriteFile(envFile, []byte(content), 0o600))

	// 进程 env 设置 HTTP_ADDR，应胜出。
	t.Setenv("HTTP_ADDR", ":6060")

	cfg, err := Load(envFile)
	require.NoError(t, err)

	assert.Equal(t, ":6060", cfg.HTTPAddr, "进程 env 应覆盖 .env 文件")
	assert.Equal(t, "warn", cfg.LogLevel, "未被 env 覆盖时使用文件值")
	assert.Equal(t, "postgres://from-file", cfg.PGDSN)
	assert.Equal(t, "kek-from-file", cfg.GatewayKEKV1)
	assert.Equal(t, "signkey-from-file", cfg.AdminTokenSigningKey)
}

func TestLoadEnvFilePath不存在时静默忽略(t *testing.T) {
	clearGatewayEnv(t)
	setMinimalRequiredEnv(t)

	cfg, err := Load("/path/does/not/exist/.env.local")
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Equal(t, ":8080", cfg.HTTPAddr)
}

func TestParseOrigins(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", []string{}},
		{"  ", []string{}},
		{"https://a.com", []string{"https://a.com"}},
		{"https://a.com, https://b.com", []string{"https://a.com", "https://b.com"}},
		{",,a,,b,,", []string{"a", "b"}},
	}
	for _, tc := range cases {
		got := parseOrigins(tc.in)
		assert.Equal(t, tc.want, got, "input=%q", tc.in)
	}
}

// clearGatewayEnv 清掉本测试关心的所有 env，避免外部环境干扰。
func clearGatewayEnv(t *testing.T) {
	t.Helper()
	keys := []string{
		"HTTP_ADDR",
		"LOG_LEVEL",
		"PG_DSN",
		"REDIS_ADDR",
		"GATEWAY_KEK_V1",
		"ADMIN_TOKEN_SIGNING_KEY",
		"OTEL_EXPORTER",
		"CORS_ALLOWED_ORIGINS",
	}
	for _, k := range keys {
		t.Setenv(k, "")
	}
}

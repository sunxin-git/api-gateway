package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setMinimalRequiredEnv 设置必填环境变量，让 validate 通过。
// 包含 D-min Unit 7 新增的 GATEWAY_TOKEN_PEPPER（所有环境强制 ≥ 32 字节）。
func setMinimalRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("PG_DSN", "postgres://test:test@localhost:5432/test?sslmode=disable")
	t.Setenv("GATEWAY_KEK_V1", "dGVzdC1rZWstdjE=") // base64("test-kek-v1")
	t.Setenv("ADMIN_TOKEN_SIGNING_KEY", "test-admin-signing-key")
	// GATEWAY_TOKEN_PEPPER：32 字节 hex = 64 字符
	t.Setenv("GATEWAY_TOKEN_PEPPER", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
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
	assert.False(t, cfg.AsyncEnabled, "默认不启用异步基座")
	assert.Equal(t, 10, cfg.AsyncConcurrency, "异步并发默认 10")
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
GATEWAY_TOKEN_PEPPER=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
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

// =============================================================================
// D-min Unit 7 fail-fast 矩阵
// =============================================================================

func TestPepper_MissingFails(t *testing.T) {
	clearGatewayEnv(t)
	t.Setenv("PG_DSN", "postgres://t")
	t.Setenv("GATEWAY_KEK_V1", "x")
	t.Setenv("ADMIN_TOKEN_SIGNING_KEY", "x")
	// 不设 GATEWAY_TOKEN_PEPPER
	_, err := Load("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "GATEWAY_TOKEN_PEPPER")
}

func TestPepper_TooShortFails(t *testing.T) {
	clearGatewayEnv(t)
	setMinimalRequiredEnv(t)
	// 31 字节 hex = 62 字符
	t.Setenv("GATEWAY_TOKEN_PEPPER", "00112233445566778899aabbccddeeff00112233445566778899aabbccddee")
	_, err := Load("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "32 字节")
}

func TestPepper_InvalidEncodingFails(t *testing.T) {
	clearGatewayEnv(t)
	setMinimalRequiredEnv(t)
	// 既非 hex 也非 base64
	t.Setenv("GATEWAY_TOKEN_PEPPER", "@@@illegal@@@")
	_, err := Load("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "GATEWAY_TOKEN_PEPPER")
}

func TestPepper_Base64Accepted(t *testing.T) {
	clearGatewayEnv(t)
	setMinimalRequiredEnv(t)
	// 32 字节 base64：base64.StdEncoding.EncodeToString(make([]byte,32))
	t.Setenv("GATEWAY_TOKEN_PEPPER", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	cfg, err := Load("")
	require.NoError(t, err)
	assert.Len(t, cfg.TokenPepperBytes, 32)
}

func TestTrustedProxies_ProductionMissingFails(t *testing.T) {
	clearGatewayEnv(t)
	setMinimalRequiredEnv(t)
	t.Setenv("GATEWAY_ENV", "production")
	t.Setenv("GATEWAY_FRONT_TLS_ACK", "true")
	// 不设 GATEWAY_TRUSTED_PROXIES
	_, err := Load("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "GATEWAY_TRUSTED_PROXIES")
}

func TestTrustedProxies_ProductionWildcardRejected(t *testing.T) {
	clearGatewayEnv(t)
	setMinimalRequiredEnv(t)
	t.Setenv("GATEWAY_ENV", "production")
	t.Setenv("GATEWAY_FRONT_TLS_ACK", "true")
	t.Setenv("GATEWAY_TRUSTED_PROXIES", "0.0.0.0/0")
	_, err := Load("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "0.0.0.0/0")
}

func TestTrustedProxies_ProductionMalformedRejected(t *testing.T) {
	clearGatewayEnv(t)
	setMinimalRequiredEnv(t)
	t.Setenv("GATEWAY_ENV", "production")
	t.Setenv("GATEWAY_FRONT_TLS_ACK", "true")
	t.Setenv("GATEWAY_TRUSTED_PROXIES", "not-a-cidr")
	_, err := Load("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "GATEWAY_TRUSTED_PROXIES")
}

func TestTrustedProxies_ProductionValidPasses(t *testing.T) {
	clearGatewayEnv(t)
	setMinimalRequiredEnv(t)
	t.Setenv("GATEWAY_ENV", "production")
	t.Setenv("GATEWAY_FRONT_TLS_ACK", "true")
	t.Setenv("GATEWAY_TRUSTED_PROXIES", "10.0.0.0/8,127.0.0.1/32")
	t.Setenv("ADMIN_AUDIT_HIGH_VALUE_LOG_PATH", "/tmp/audit.log")
	cfg, err := Load("")
	require.NoError(t, err)
	assert.Equal(t, []string{"10.0.0.0/8", "127.0.0.1/32"}, cfg.TrustedProxyCIDRs)
}

func TestTrustedProxies_DevEmptyOK(t *testing.T) {
	clearGatewayEnv(t)
	setMinimalRequiredEnv(t)
	// 不设 GATEWAY_ENV → dev
	cfg, err := Load("")
	require.NoError(t, err)
	assert.Empty(t, cfg.TrustedProxyCIDRs)
	assert.Equal(t, EnvDev, cfg.GatewayEnv)
}

func TestTLS_ProductionMissingTLSAndAckFails(t *testing.T) {
	clearGatewayEnv(t)
	setMinimalRequiredEnv(t)
	t.Setenv("GATEWAY_ENV", "production")
	t.Setenv("GATEWAY_TRUSTED_PROXIES", "10.0.0.0/8")
	// 不设 LISTEN_TLS / FRONT_TLS_ACK
	_, err := Load("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "TLS")
}

func TestTLS_ProductionListenTLSWithoutCertFails(t *testing.T) {
	clearGatewayEnv(t)
	setMinimalRequiredEnv(t)
	t.Setenv("GATEWAY_ENV", "production")
	t.Setenv("GATEWAY_TRUSTED_PROXIES", "10.0.0.0/8")
	t.Setenv("GATEWAY_LISTEN_TLS", "true")
	t.Setenv("ADMIN_AUDIT_HIGH_VALUE_LOG_PATH", "/tmp/audit.log")
	// 不设 cert/key
	_, err := Load("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "TLS_CERT_PATH")
}

func TestTLS_ProductionListenTLSWithMissingFiles(t *testing.T) {
	clearGatewayEnv(t)
	setMinimalRequiredEnv(t)
	t.Setenv("GATEWAY_ENV", "production")
	t.Setenv("GATEWAY_TRUSTED_PROXIES", "10.0.0.0/8")
	t.Setenv("GATEWAY_LISTEN_TLS", "true")
	t.Setenv("GATEWAY_TLS_CERT_PATH", "/nonexistent/cert.pem")
	t.Setenv("GATEWAY_TLS_KEY_PATH", "/nonexistent/key.pem")
	t.Setenv("ADMIN_AUDIT_HIGH_VALUE_LOG_PATH", "/tmp/audit.log")
	_, err := Load("")
	require.Error(t, err)
}

func TestTLS_ProductionFrontAckOK(t *testing.T) {
	clearGatewayEnv(t)
	setMinimalRequiredEnv(t)
	t.Setenv("GATEWAY_ENV", "production")
	t.Setenv("GATEWAY_TRUSTED_PROXIES", "10.0.0.0/8")
	t.Setenv("GATEWAY_FRONT_TLS_ACK", "true")
	t.Setenv("ADMIN_AUDIT_HIGH_VALUE_LOG_PATH", "/tmp/audit.log")
	cfg, err := Load("")
	require.NoError(t, err)
	assert.True(t, cfg.FrontTLSAck)
}

func TestAdminAuditPath_ProductionRequired(t *testing.T) {
	clearGatewayEnv(t)
	setMinimalRequiredEnv(t)
	t.Setenv("GATEWAY_ENV", "production")
	t.Setenv("GATEWAY_TRUSTED_PROXIES", "10.0.0.0/8")
	t.Setenv("GATEWAY_FRONT_TLS_ACK", "true")
	// 不设 ADMIN_AUDIT_HIGH_VALUE_LOG_PATH
	_, err := Load("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ADMIN_AUDIT_HIGH_VALUE_LOG_PATH")
}

func TestNormalizeEnv(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"production", EnvProduction},
		{"PRODUCTION", EnvProduction},
		{" Dev ", EnvDev},
		{"", EnvDev},
		{"staging", EnvDev}, // 未知值 fallback dev
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			assert.Equal(t, tc.want, normalizeEnv(tc.in))
		})
	}
}

// =============================================================================
// Phase 2 异步基座（ADR-0006 / Unit 1）
// =============================================================================

func TestAsync_EnabledValidConcurrency(t *testing.T) {
	clearGatewayEnv(t)
	setMinimalRequiredEnv(t)
	t.Setenv("GATEWAY_ASYNC_ENABLED", "true")
	t.Setenv("GATEWAY_ASYNC_CONCURRENCY", "25")
	cfg, err := Load("")
	require.NoError(t, err)
	assert.True(t, cfg.AsyncEnabled)
	assert.Equal(t, 25, cfg.AsyncConcurrency)
}

func TestAsync_EnabledZeroConcurrencyFails(t *testing.T) {
	clearGatewayEnv(t)
	setMinimalRequiredEnv(t)
	t.Setenv("GATEWAY_ASYNC_ENABLED", "true")
	t.Setenv("GATEWAY_ASYNC_CONCURRENCY", "0")
	_, err := Load("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "GATEWAY_ASYNC_CONCURRENCY")
}

func TestAsync_EnabledTooHighConcurrencyFails(t *testing.T) {
	clearGatewayEnv(t)
	setMinimalRequiredEnv(t)
	t.Setenv("GATEWAY_ASYNC_ENABLED", "true")
	t.Setenv("GATEWAY_ASYNC_CONCURRENCY", "5000")
	_, err := Load("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "GATEWAY_ASYNC_CONCURRENCY")
}

func TestAsync_DisabledSkipsConcurrencyValidation(t *testing.T) {
	clearGatewayEnv(t)
	setMinimalRequiredEnv(t)
	// AsyncEnabled 默认 false；即便 concurrency 非法（0）也不校验，照常启动。
	t.Setenv("GATEWAY_ASYNC_CONCURRENCY", "0")
	cfg, err := Load("")
	require.NoError(t, err)
	assert.False(t, cfg.AsyncEnabled)
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
		"LEDGER_DRIFT_ACTION",
		// D-min Unit 7 新增
		"GATEWAY_ENV",
		"GATEWAY_TRUSTED_PROXIES",
		"GATEWAY_LISTEN_TLS",
		"GATEWAY_FRONT_TLS_ACK",
		"GATEWAY_TLS_CERT_PATH",
		"GATEWAY_TLS_KEY_PATH",
		"GATEWAY_TOKEN_PEPPER",
		"ADMIN_AUDIT_HIGH_VALUE_LOG_PATH",
		// Phase 2 异步基座
		"GATEWAY_ASYNC_ENABLED",
		"GATEWAY_ASYNC_CONCURRENCY",
	}
	for _, k := range keys {
		t.Setenv(k, "")
	}
}

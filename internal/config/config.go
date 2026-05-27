// Package config 提供基于 koanf 的配置加载层。
//
// 加载顺序（后者覆盖前者）：
//  1. 内置默认值（HTTP 监听地址、日志等级等）。
//  2. 仓库根目录下的 .env.local（如存在，用于本地开发）。
//  3. 进程环境变量（最终胜出）。
//
// Fail-fast 约束：缺少以下任一项即返回错误：
//   - PGDSN（PostgreSQL 连接串）
//   - GATEWAY_KEK_V1（信封加密主密钥 v1）
//   - ADMIN_TOKEN_SIGNING_KEY（Admin Token 签名密钥）
package config

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strings"

	"github.com/knadh/koanf/parsers/dotenv"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

// 配置键常量（统一在此声明，避免散落各处 magic string）。
const (
	keyHTTPAddr             = "http_addr"
	keyLogLevel             = "log_level"
	keyPGDSN                = "pg_dsn"
	keyRedisAddr            = "redis_addr"
	keyGatewayKEKV1         = "gateway_kek_v1"
	keyAdminTokenSigningKey = "admin_token_signing_key"
	keyOTelExporter         = "otel_exporter"
	keyCORSAllowedOrigins   = "cors_allowed_origins"
	keyLedgerDriftAction    = "ledger_drift_action"

	// D-min Unit 7 新增（admin API 安全 / 部署侧约束）。
	keyGatewayEnv          = "gateway_env"
	keyTrustedProxyCIDRs   = "gateway_trusted_proxies"
	keyListenTLS           = "gateway_listen_tls"
	keyFrontTLSAck         = "gateway_front_tls_ack"
	keyTLSCertPath         = "gateway_tls_cert_path"
	keyTLSKeyPath          = "gateway_tls_key_path"
	keyTokenPepper         = "gateway_token_pepper"
	keyAdminAuditTier1Path = "admin_audit_high_value_log_path"
)

// 环境模式常量。
const (
	EnvProduction = "production"
	EnvDev        = "dev"
	EnvTest       = "test"
)

// 最小 pepper 字节数（决策 D1）。
const minTokenPepperBytes = 32

// Config 是整个 api-gateway 进程的配置快照。
// 所有字段在 Load() 成功返回后即不可变（请勿在运行时修改）。
type Config struct {
	// HTTPAddr Gin HTTP 监听地址，默认 ":8080"。
	HTTPAddr string
	// LogLevel slog 日志等级（debug/info/warn/error），默认 "info"。
	LogLevel string
	// PGDSN PostgreSQL 连接串，**必填**。
	PGDSN string
	// RedisAddr Redis 连接地址（host:port），默认 "localhost:6379"。
	RedisAddr string
	// GatewayKEKV1 信封加密主密钥 v1（base64 或 hex 编码原始字节，由调用方解码），**必填**。
	GatewayKEKV1 string
	// AdminTokenSigningKey Admin Token 签名密钥，**必填**。
	AdminTokenSigningKey string
	// OTelExporter OTel trace exporter 类型，枚举 "stdout" | "otlp"，默认 "stdout"。
	OTelExporter string
	// CORSAllowedOrigins CORS 允许的 origin 白名单。空切片 = 拒绝所有跨域（fail-closed）。
	// 环境变量来源：CORS_ALLOWED_ORIGINS（逗号分隔，例 "https://a.com,https://b.com"）。
	CORSAllowedOrigins []string
	// LedgerDriftAction reconciler 发现 drift 后的处理动作（计划 Unit 7）。
	// 取值：
	//   - "log"    P0 默认；dry-run 模式，仅 log + bump metric，不冻结账户
	//   - "freeze" 二次确认仍不一致即调 service.Freeze，账户进入 frozen 状态
	// 生产环境建议先跑 1-2 周 dry-run 零误报后再切 "freeze"。
	LedgerDriftAction string

	// ===== D-min Unit 7 新增（admin API 安全 / 部署侧约束） =====

	// GatewayEnv 部署环境模式，枚举 "production" | "dev" | "test"，默认 "dev"。
	// production 模式下若 TrustedProxies 缺失 / TLS 未明示 / pepper 缺失 → 拒启动。
	GatewayEnv string

	// TrustedProxyCIDRs Gin engine.SetTrustedProxies 输入；
	// CSV 例：GATEWAY_TRUSTED_PROXIES=10.0.0.0/8,127.0.0.1/32。
	// production 模式必须显式配置（空 / 含 0.0.0.0/0 → 拒启动），否则 c.ClientIP() 受 XFF 伪造。
	TrustedProxyCIDRs []string

	// ListenTLS 进程是否自带 TLS（true → main.go 调 srv.StartTLS(cert, key)）。
	// 与 FrontTLSAck 互斥但不要求二选一在 dev 模式；production 必须任一为 true。
	ListenTLS bool

	// FrontTLSAck 运维明示"前端反代已 TLS 终止，本进程接受 HTTP 接收"。
	// production 模式下若 ListenTLS=false 必须 FrontTLSAck=true，否则拒启动。
	FrontTLSAck bool

	// TLSCertPath / TLSKeyPath 当 ListenTLS=true 时必填；启动期校验文件可读。
	TLSCertPath string
	TLSKeyPath  string

	// TokenPepper Admin Token HMAC pepper（决策 D1）；输入字符串为 base64 或 hex 编码 ≥ 32 字节。
	// 解码后的原始字节存于 TokenPepperBytes；fail-fast 在所有环境下校验长度 ≥ 32。
	TokenPepper      string
	TokenPepperBytes []byte

	// AdminAuditTier1Path Tier1（refund / token lifecycle / 401 / idempotency_conflict）
	// 同步审计日志文件路径。production 强制有值；dev 可空（dev 不写 Tier1 文件）。
	AdminAuditTier1Path string
}

// Load 加载并校验配置。
//
// envFilePath 为 .env 文件路径（通常传 ".env.local"）；传空串则跳过文件加载。
// 即使 envFilePath 指向的文件不存在，函数也只是忽略它而不报错（本地开发可缺省）。
func Load(envFilePath string) (*Config, error) {
	k := koanf.New(".")

	// 第 1 步：默认值。
	defaults := map[string]any{
		keyHTTPAddr:           ":8080",
		keyLogLevel:           "info",
		keyRedisAddr:          "localhost:6379",
		keyOTelExporter:       "stdout",
		keyCORSAllowedOrigins: "",
		keyLedgerDriftAction:  "log",
		keyGatewayEnv:         EnvDev, // 默认 dev；production 必须显式设置
		keyTrustedProxyCIDRs:  "",
		keyListenTLS:          "false",
		keyFrontTLSAck:        "false",
	}
	if err := k.Load(mapProvider(defaults), nil); err != nil {
		return nil, fmt.Errorf("加载默认值失败: %w", err)
	}

	// 第 2 步：.env 文件（如存在）。
	// 同样把 KEY 统一转 lowercase，与默认值/env provider 的 key 命名空间对齐。
	if envFilePath != "" {
		if _, statErr := os.Stat(envFilePath); statErr == nil {
			parser := dotenv.ParserEnvWithValue("", ".", func(key, value string) (string, any) {
				if value == "" {
					return "", nil
				}
				return strings.ToLower(key), value
			})
			if err := k.Load(file.Provider(envFilePath), parser); err != nil {
				return nil, fmt.Errorf("加载 env 文件 %q 失败: %w", envFilePath, err)
			}
		}
	}

	// 第 3 步：进程环境变量（最终胜出）。
	// 约定：env 变量名直接映射到 koanf key（lowercase，下划线分隔）。
	// 例：HTTP_ADDR -> http_addr；GATEWAY_KEK_V1 -> gateway_kek_v1。
	//
	// 空值的 env 不参与覆盖（避免「设了变量但内容为空」把默认值/文件值抹掉）。
	envProvider := env.ProviderWithValue("", ".", func(key, value string) (string, any) {
		if value == "" {
			return "", nil // 返回空 key → env provider 会忽略此项
		}
		return strings.ToLower(key), value
	})
	if err := k.Load(envProvider, nil); err != nil {
		return nil, fmt.Errorf("加载环境变量失败: %w", err)
	}

	cfg := &Config{
		HTTPAddr:             k.String(keyHTTPAddr),
		LogLevel:             k.String(keyLogLevel),
		PGDSN:                k.String(keyPGDSN),
		RedisAddr:            k.String(keyRedisAddr),
		GatewayKEKV1:         k.String(keyGatewayKEKV1),
		AdminTokenSigningKey: k.String(keyAdminTokenSigningKey),
		OTelExporter:         k.String(keyOTelExporter),
		CORSAllowedOrigins:   parseOrigins(k.String(keyCORSAllowedOrigins)),
		LedgerDriftAction:    k.String(keyLedgerDriftAction),
		GatewayEnv:           normalizeEnv(k.String(keyGatewayEnv)),
		TrustedProxyCIDRs:    parseOrigins(k.String(keyTrustedProxyCIDRs)),
		ListenTLS:            k.Bool(keyListenTLS),
		FrontTLSAck:          k.Bool(keyFrontTLSAck),
		TLSCertPath:          k.String(keyTLSCertPath),
		TLSKeyPath:           k.String(keyTLSKeyPath),
		TokenPepper:          k.String(keyTokenPepper),
		AdminAuditTier1Path:  k.String(keyAdminAuditTier1Path),
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// normalizeEnv 把环境标识统一为小写；空 / 未知值 fallback 到 dev（与默认值一致）。
func normalizeEnv(s string) string {
	v := strings.ToLower(strings.TrimSpace(s))
	switch v {
	case EnvProduction, EnvDev, EnvTest:
		return v
	default:
		return EnvDev
	}
}

// validate 执行 fail-fast 校验。
func (c *Config) validate() error {
	var missing []string
	if strings.TrimSpace(c.PGDSN) == "" {
		missing = append(missing, "PGDSN")
	}
	if strings.TrimSpace(c.GatewayKEKV1) == "" {
		missing = append(missing, "GATEWAY_KEK_V1")
	}
	if strings.TrimSpace(c.AdminTokenSigningKey) == "" {
		missing = append(missing, "ADMIN_TOKEN_SIGNING_KEY")
	}
	if len(missing) > 0 {
		return fmt.Errorf("配置缺失必填项: %s（请检查环境变量或 .env.local）",
			strings.Join(missing, ", "))
	}

	switch c.OTelExporter {
	case "stdout", "otlp":
		// ok
	default:
		return fmt.Errorf("配置项 OTEL_EXPORTER 非法值 %q，仅支持 stdout|otlp",
			c.OTelExporter)
	}

	switch c.LogLevel {
	case "debug", "info", "warn", "error":
		// ok
	default:
		return fmt.Errorf("配置项 LOG_LEVEL 非法值 %q，仅支持 debug|info|warn|error",
			c.LogLevel)
	}

	switch c.LedgerDriftAction {
	case "log", "freeze":
		// ok
	default:
		return fmt.Errorf("配置项 LEDGER_DRIFT_ACTION 非法值 %q，仅支持 log|freeze",
			c.LedgerDriftAction)
	}

	// ===== D-min Unit 7 fail-fast 校验 =====
	if err := c.validateAdminSecurity(); err != nil {
		return err
	}

	return nil
}

// validateAdminSecurity 落实 Unit 7 plan 中三套 fail-fast 矩阵：
//
//   - GATEWAY_TOKEN_PEPPER：所有环境强制 ≥ 32 字节有效原始字节（决策 D1）
//   - GATEWAY_TRUSTED_PROXIES：production 强制显式列出（决策 D5 + Unit 7 §1）
//   - TLS：production 必须 ListenTLS=true (cert/key 可读) 或 FrontTLSAck=true 二选一
func (c *Config) validateAdminSecurity() error {
	// 1. TokenPepper：全环境强制（即便 dev / test，DB 也可能泄露）
	if strings.TrimSpace(c.TokenPepper) == "" {
		return errors.New(
			"配置缺失必填项: GATEWAY_TOKEN_PEPPER（生成：openssl rand -hex 32；" +
				"决策 D1：所有环境强制 ≥ 32 字节，防 DB 泄露后离线穷举）",
		)
	}
	pepperBytes, err := decodePepper(c.TokenPepper)
	if err != nil {
		return fmt.Errorf("GATEWAY_TOKEN_PEPPER 解码失败（支持 hex 或 base64）: %w", err)
	}
	if len(pepperBytes) < minTokenPepperBytes {
		return fmt.Errorf(
			"GATEWAY_TOKEN_PEPPER 解码后长度 %d 字节 < 最小 %d 字节",
			len(pepperBytes), minTokenPepperBytes,
		)
	}
	c.TokenPepperBytes = pepperBytes

	// 2. TrustedProxies：production 强制显式 CIDR；dev/test warn-only
	if err := c.validateTrustedProxies(); err != nil {
		return err
	}

	// 3. TLS：production 强制 ListenTLS 或 FrontTLSAck 二选一
	if err := c.validateTLS(); err != nil {
		return err
	}

	// 4. AdminAuditTier1Path：production 强制；dev/test 可空（fallback 到 stderr）
	if c.GatewayEnv == EnvProduction && strings.TrimSpace(c.AdminAuditTier1Path) == "" {
		return errors.New(
			"production 模式必须配置 ADMIN_AUDIT_HIGH_VALUE_LOG_PATH（Tier1 高价值审计 " +
				"refund / token lifecycle / idempotency_conflict / auth_failed 同步落盘路径；" +
				"决策 D3，写失败将关闭 /readyz）",
		)
	}

	return nil
}

func (c *Config) validateTrustedProxies() error {
	if c.GatewayEnv != EnvProduction {
		return nil
	}
	if len(c.TrustedProxyCIDRs) == 0 {
		return errors.New(
			"production 模式必须显式配置 GATEWAY_TRUSTED_PROXIES 为反代实际 CIDR " +
				"（当前配置会导致 X-Forwarded-For 可被任意伪造，IP allowlist 形同虚设）",
		)
	}
	for _, raw := range c.TrustedProxyCIDRs {
		p, err := netip.ParsePrefix(strings.TrimSpace(raw))
		if err != nil {
			return fmt.Errorf("GATEWAY_TRUSTED_PROXIES 含非法 CIDR %q: %w", raw, err)
		}
		// 拒绝 0.0.0.0/0 / ::/0（等同未限制）
		if p.Bits() == 0 {
			return fmt.Errorf(
				"GATEWAY_TRUSTED_PROXIES 不允许 %q（覆盖全 IP 等同未限制；"+
					"production 必须填具体反代 CIDR）", raw,
			)
		}
	}
	return nil
}

func (c *Config) validateTLS() error {
	if c.GatewayEnv != EnvProduction {
		return nil
	}
	if !c.ListenTLS && !c.FrontTLSAck {
		return errors.New(
			"production 模式必须确保 TLS：要么进程自带 TLS（GATEWAY_LISTEN_TLS=true + " +
				"GATEWAY_TLS_CERT_PATH + GATEWAY_TLS_KEY_PATH），" +
				"要么明示前端反代已 TLS 终止（GATEWAY_FRONT_TLS_ACK=true）",
		)
	}
	if c.ListenTLS {
		if strings.TrimSpace(c.TLSCertPath) == "" || strings.TrimSpace(c.TLSKeyPath) == "" {
			return errors.New("GATEWAY_LISTEN_TLS=true 时必须同时设置 GATEWAY_TLS_CERT_PATH 与 GATEWAY_TLS_KEY_PATH")
		}
		if _, err := os.Stat(c.TLSCertPath); err != nil {
			return fmt.Errorf("TLS cert 文件不可读 %q: %w", c.TLSCertPath, err)
		}
		if _, err := os.Stat(c.TLSKeyPath); err != nil {
			return fmt.Errorf("TLS key 文件不可读 %q: %w", c.TLSKeyPath, err)
		}
	}
	return nil
}

// decodePepper 把 base64 或 hex 编码字符串解码为原始字节。
//
// 优先尝试 hex（OpenSSL `rand -hex 32` 输出），失败再尝试 base64。
// 两种都失败返合并错误。
func decodePepper(s string) ([]byte, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return nil, errors.New("empty")
	}
	// hex 先：长度偶数且全 [0-9a-fA-F]；典型 32 字节 → 64 hex 字符
	if b, err := hex.DecodeString(raw); err == nil {
		return b, nil
	}
	// base64：try std then raw urlsafe（含 padding 的 std 最常见）
	if b, err := base64.StdEncoding.DecodeString(raw); err == nil {
		return b, nil
	}
	if b, err := base64.RawStdEncoding.DecodeString(raw); err == nil {
		return b, nil
	}
	if b, err := base64.URLEncoding.DecodeString(raw); err == nil {
		return b, nil
	}
	if b, err := base64.RawURLEncoding.DecodeString(raw); err == nil {
		return b, nil
	}
	return nil, errors.New("既非合法 hex 也非合法 base64")
}

// parseOrigins 把逗号分隔的字符串拆为切片，去除空项与首尾空白。
func parseOrigins(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return []string{}
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// mapProvider 实现一个最简单的内存 map koanf.Provider，仅用于默认值注入。
func mapProvider(m map[string]any) koanf.Provider {
	return &inlineProvider{data: m}
}

type inlineProvider struct {
	data map[string]any
}

func (p *inlineProvider) ReadBytes() ([]byte, error) {
	return nil, errors.New("inline provider does not support ReadBytes")
}

func (p *inlineProvider) Read() (map[string]any, error) {
	// 拷贝一份避免外部修改污染。
	out := make(map[string]any, len(p.data))
	for k, v := range p.data {
		out[k] = v
	}
	return out, nil
}

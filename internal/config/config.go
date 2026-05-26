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
	"errors"
	"fmt"
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
)

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
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return cfg, nil
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

	return nil
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

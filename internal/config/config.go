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
	"math"
	"net/netip"
	"os"
	"strings"

	"github.com/knadh/koanf/parsers/dotenv"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"

	"github.com/sunxin-git/api-gateway/internal/crypto"
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

	// 管理后台配置线 Unit 3 新增（ADR-0008：初始管理员 env 种子，仅表空时生效）。
	keyAdminBootstrapUsername = "gateway_admin_bootstrap_username"
	keyAdminBootstrapPassword = "gateway_admin_bootstrap_password"

	// 管理后台配置线 Unit 4 新增（ADR-0008：会话 TTL）。
	keyAdminSessionTTLSeconds = "gateway_admin_session_ttl_seconds"

	// F-min Unit 7 新增（relay 业务路由 /v1 配置）。
	// RelayEnabled=false（默认）时 /v1 路由不注册（admin-only 部署）；
	// =true 时 8 个 RELAY_* 字段经 relay.NewEnvCatalog fail-fast 校验（main.go 装配时）。
	keyRelayEnabled               = "gateway_relay_enabled"
	keyRelayModelName             = "gateway_relay_model_name"
	keyRelayUpstreamProviderType  = "gateway_relay_upstream_provider_type"
	keyRelayUpstreamBaseURL       = "gateway_relay_upstream_base_url"
	keyRelayUpstreamAPIKey        = "gateway_relay_upstream_api_key"
	keyRelayUpstreamModelName     = "gateway_relay_upstream_model_name"
	keyRelayPriceInputPer1MMinor  = "gateway_relay_price_input_per_1m_minor"
	keyRelayPriceOutputPer1MMinor = "gateway_relay_price_output_per_1m_minor"
	keyRelayMaxContextTokens      = "gateway_relay_max_context_tokens"

	// Phase 2 异步基座（ADR-0006 / Unit 1）新增。
	// AsyncEnabled=false（默认）时不构造 Asynq server、不要求 Redis 可达（保持 admin-only /
	// 同步 relay 部署零 Redis 依赖）；=true 时 main.go 对 Redis 做启动 ping fail-fast。
	keyAsyncEnabled     = "gateway_async_enabled"
	keyAsyncConcurrency = "gateway_async_concurrency"
	// Redis 认证 / TLS（评审 #9：生产 Redis 须支持 ACL/mTLS）。
	keyRedisPassword   = "gateway_redis_password"
	keyRedisTLSEnabled = "gateway_redis_tls_enabled"

	// Phase 2 异步视频中继（Unit 4）新增（GATEWAY_VIDEO_RELAY_* 视频字典配置）。
	// VideoRelayEnabled=false（默认）时 /v1/video/* 路由不注册；=true 时 main.go（Unit 10）
	// 用以下字段构造 video.NewEnvVideoCatalog（fail-fast 校验在该构造内，与 RelayEnabled 同风格）。
	keyVideoRelayEnabled           = "gateway_video_relay_enabled"
	keyVideoRelayModelName         = "gateway_video_relay_model_name"
	keyVideoRelayProviderType      = "gateway_video_relay_provider_type"
	keyVideoRelayUpstreamBaseURL   = "gateway_video_relay_upstream_base_url"
	keyVideoRelayUpstreamModelName = "gateway_video_relay_upstream_model_name"
	keyVideoRelayChannelName       = "gateway_video_relay_channel_name"
	// 分辨率档单价（CNY 分 / 百万 token）；0 = 该档不在售（至少一档 > 0）。
	keyVideoRelayPrice480pPer1MMinor  = "gateway_video_relay_price_480p_per_1m_minor"
	keyVideoRelayPrice720pPer1MMinor  = "gateway_video_relay_price_720p_per_1m_minor"
	keyVideoRelayPrice1080pPer1MMinor = "gateway_video_relay_price_1080p_per_1m_minor"
	// 商业加价倍率基点（10000=1.0×，默认 11000=1.1×，对齐参考实现 video_credit_multiplier）。
	keyVideoRelayBillingMultiplierBP = "gateway_video_relay_billing_multiplier_bp"
	// 取值档（能力描述符约束来源；均有默认值，最小配置只需填 model/channel/价格）。
	keyVideoRelayDurationMinSeconds     = "gateway_video_relay_duration_min_seconds"
	keyVideoRelayDurationMaxSeconds     = "gateway_video_relay_duration_max_seconds"
	keyVideoRelayDurationDefaultSeconds = "gateway_video_relay_duration_default_seconds"
	keyVideoRelayFpsDefault             = "gateway_video_relay_fps_default"
	keyVideoRelayFpsMax                 = "gateway_video_relay_fps_max"
	keyVideoRelayRatios                 = "gateway_video_relay_ratios"
	keyVideoRelayRatioDefault           = "gateway_video_relay_ratio_default"
	keyVideoRelayResolutionDefault      = "gateway_video_relay_resolution_default"

	// Phase 2 Unit 8：回调入口 + 账户×模型并发默认上限。
	keyVideoCallbackBaseURL         = "gateway_video_callback_base_url"
	keyVideoRelayConcurrencyDefault = "gateway_video_relay_concurrency_default"

	// Unit 10：reserve 估算安全系数 / 最低 token 下限（Unit 7 残留单一配置源）+ 结果签名 URL TTL。
	keyVideoRelaySafetyFactorBP = "gateway_video_relay_safety_factor_bp"
	keyVideoRelayMinTokenFloor  = "gateway_video_relay_min_token_floor"
	keyVideoResultURLTTLSeconds = "gateway_video_result_url_ttl_seconds"
)

// asyncConcurrency 上下界（仅 AsyncEnabled 时校验）。
const (
	minAsyncConcurrency = 1
	maxAsyncConcurrency = 1000
)

// maxVideoConcurrencyDefault 账户×模型并发默认上限的上界（防误配天文数字；Unit 8）。
const maxVideoConcurrencyDefault = 1000

// Unit 10 视频计费/结果 URL 校验边界。
const (
	// minVideoSafetyFactorBP 安全系数下界（10000=1.0×）：低于此 reserve 会 under-reserve。
	minVideoSafetyFactorBP = 10_000
	// maxResultURLTTLSeconds 结果签名 URL TTL 上界（= TOS 预签名上限 7 天）。
	maxResultURLTTLSeconds = 604_800
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
	// RedisPassword Redis ACL 密码（评审 #9）；空 = 无密码。json:"-" 防序列化泄露。
	RedisPassword string `json:"-"`
	// RedisTLSEnabled 是否对 Redis 启用 TLS（评审 #9）。
	RedisTLSEnabled bool
	// GatewayKEKV1 信封加密主密钥 v1（base64 或 hex 编码原始字节，由调用方解码），**必填**。
	// json:"-" 防 Config 被意外 json 序列化 / 反射打日志时泄露密钥（评审 #19）。
	GatewayKEKV1 string `json:"-"`
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
	TokenPepper      string `json:"-"` // 防序列化泄露（评审 #19）
	TokenPepperBytes []byte `json:"-"`

	// AdminAuditTier1Path Tier1（refund / token lifecycle / 401 / idempotency_conflict）
	// 同步审计日志文件路径。production 强制有值；dev 可空（dev 不写 Tier1 文件）。
	AdminAuditTier1Path string

	// AdminBootstrapUsername / AdminBootstrapPassword 管理后台初始管理员种子（ADR-0008 决策 6）。
	// 仅 operator_account 表空时生效（幂等）；production 表空且二者均空 → 启动 fail-fast（见 operator.Bootstrap）。
	AdminBootstrapUsername string `json:"-"` // 防序列化泄露（认证路径一半，与口令同惯例）
	AdminBootstrapPassword string `json:"-"` // 防序列化泄露

	// AdminSessionTTLSeconds 管理后台会话有效期（秒，ADR-0008）；默认 43200（12h）。
	AdminSessionTTLSeconds int64

	// ===== F-min Unit 7 新增（relay 业务路由 /v1 配置） =====

	// RelayEnabled 是否启用 relay 业务路由 /v1/chat/completions（默认 false = admin-only 部署）。
	// =true 时 main.go 用以下 8 个 Relay* 字段构造 relay.EnvCatalog（fail-fast 校验在
	// relay.NewEnvCatalog 内，避免与 config 重复校验）。
	RelayEnabled bool

	// RelayModelName 业务可见 model 名（如 "gw-default"）。
	RelayModelName string
	// RelayUpstreamProviderType provider 协议簇；MVP 唯一 "openai_compat"。
	RelayUpstreamProviderType string
	// RelayUpstreamBaseURL 上游 base url（不含 /chat/completions）；production 强制 https。
	RelayUpstreamBaseURL string
	// RelayUpstreamAPIKey 上游凭据（MVP env 明文；P1 envelope encryption）。json:"-" 防泄露（评审 #19）。
	RelayUpstreamAPIKey string `json:"-"`
	// RelayUpstreamModelName 上游真实 model 名（如 "doubao-1-5-pro-32k-250115"）。
	RelayUpstreamModelName string
	// RelayPriceInputPer1MMinor / RelayPriceOutputPer1MMinor input/output token 单价（每 1M token，minor / CNY 分）。
	RelayPriceInputPer1MMinor  int64
	RelayPriceOutputPer1MMinor int64
	// RelayMaxContextTokens 字典默认 max context（业务可传更小 max_tokens）。
	RelayMaxContextTokens int32

	// ===== Phase 2 异步基座（ADR-0006 / Unit 1）新增 =====

	// AsyncEnabled 是否启用异步执行基座（Asynq + Redis），默认 false。
	// =true 时 main.go 构造 Asynq server、对 Redis 做启动 ping fail-fast（与 pgxpool 同风格）。
	// =false 时完全不碰 Redis（保持现有 admin-only / 同步 relay 部署零 Redis 依赖）。
	AsyncEnabled bool

	// AsyncConcurrency Asynq server worker 池大小，默认 10。
	// 这是**执行层吞吐**（一次跑几个 job），非 R15 业务并发上限（后者走 DB 原子 claim，ADR-0006）。
	AsyncConcurrency int

	// ===== Phase 2 异步视频中继（Unit 4）新增（GATEWAY_VIDEO_RELAY_* 视频字典配置） =====

	// VideoRelayEnabled 是否启用视频中继路由 /v1/video/*（默认 false）。
	// =true 时 main.go（Unit 10）用以下字段构造 video.NewEnvVideoCatalog（字段自洽 fail-fast 在
	// 该构造内，避免与 config 重复校验）。**强制依赖 AsyncEnabled=true**（视频走异步基座），
	// validate 落实此跨字段依赖。
	VideoRelayEnabled bool

	// VideoRelayModelName 业务可见视频 model 名（如 "gw-video"）。
	VideoRelayModelName string
	// VideoRelayProviderType 视频 provider 协议簇；MVP 唯一 "volc_seedance"，默认即此。
	VideoRelayProviderType string
	// VideoRelayUpstreamBaseURL 上游 base url（不含 endpoint path）；production 强制 https。
	VideoRelayUpstreamBaseURL string
	// VideoRelayUpstreamModelName 上游真实 model 名（如 "doubao-seedance-2-0-..."）。
	VideoRelayUpstreamModelName string
	// VideoRelayChannelName 绑定的 channel 名（凭据来源；Unit 3 channel service 据此取凭据）。
	VideoRelayChannelName string

	// VideoRelayPrice{480p,720p,1080p}Per1MMinor 各分辨率档单价（CNY 分 / 百万 token）。
	// 0 = 该档不在售（不进 capability 枚举、不进定价表）；至少一档须 > 0（catalog fail-fast）。
	VideoRelayPrice480pPer1MMinor  int64
	VideoRelayPrice720pPer1MMinor  int64
	VideoRelayPrice1080pPer1MMinor int64
	// VideoRelayBillingMultiplierBP 商业加价倍率基点（10000=1.0×），默认 11000=1.1×。
	VideoRelayBillingMultiplierBP int64

	// 取值档（能力描述符约束来源；均有默认值）。
	VideoRelayDurationMinSeconds     int64
	VideoRelayDurationMaxSeconds     int64
	VideoRelayDurationDefaultSeconds int64
	VideoRelayFpsDefault             int64
	VideoRelayFpsMax                 int64
	VideoRelayRatios                 []string // aspect ratio 枚举（CSV 解析）
	VideoRelayRatioDefault           string
	VideoRelayResolutionDefault      string

	// VideoCallbackBaseURL 回调入口 base URL（如 https://gw.example.com）；空 = 纯轮询兜底（不注册
	// 回调路由、submit 不带回调 URL）。production 强制 https。submit worker 据此 + per-task token
	// 构造交给上游的回调 URL（含 token 的 URL 绝不入日志）。Unit 8。
	VideoCallbackBaseURL string
	// VideoRelayConcurrencyDefault 账户×模型并发默认上限（R15；DB 原子 claim 的 cap）。默认 5；
	// per-(account,model) 覆写预留给 Unit 11 admin。Unit 8。
	VideoRelayConcurrencyDefault int

	// VideoRelaySafetyFactorBP reserve 估算安全系数基点（10000=1.0×，默认 12000=1.2×；Unit 7 残留单一配置源）。
	// 须 ≥ 10000，否则 reserve 可能 under-reserve（撞账本 ErrCommitExceedsReserved）。
	VideoRelaySafetyFactorBP int64
	// VideoRelayMinTokenFloor 最低 token 计费下限（seedance 2.0，ADR-0006；默认 0=无下限，落地核对官方）。
	VideoRelayMinTokenFloor int64
	// VideoResultURLTTLSeconds 结果签名 URL TTL（秒；默认 900=15min，最小化为业务取回所需，非整个轮询窗口）。
	VideoResultURLTTLSeconds int64
}

// Load 加载并校验配置。
//
// envFilePath 为 .env 文件路径（通常传 ".env.local"）；传空串则跳过文件加载。
// 即使 envFilePath 指向的文件不存在，函数也只是忽略它而不报错（本地开发可缺省）。
func Load(envFilePath string) (*Config, error) {
	k := koanf.New(".")

	// 第 1 步：默认值。
	defaults := map[string]any{
		keyHTTPAddr:                  ":8080",
		keyLogLevel:                  "info",
		keyRedisAddr:                 "localhost:6379",
		keyOTelExporter:              "stdout",
		keyCORSAllowedOrigins:        "",
		keyLedgerDriftAction:         "log",
		keyGatewayEnv:                EnvDev, // 默认 dev；production 必须显式设置
		keyTrustedProxyCIDRs:         "",
		keyListenTLS:                 "false",
		keyFrontTLSAck:               "false",
		keyRelayEnabled:              "false", // 默认 admin-only；显式 =true 开启 /v1 relay
		keyRelayUpstreamProviderType: "openai_compat",
		keyAsyncEnabled:              "false", // 默认不启用异步基座 → 零 Redis 依赖
		keyAsyncConcurrency:          10,
		keyRedisTLSEnabled:           "false",

		// Phase 2 视频中继默认值（仅 VideoRelayEnabled=true 时这些档生效）。
		keyVideoRelayEnabled:                "false",
		keyVideoRelayProviderType:           "volc_seedance",
		keyVideoRelayBillingMultiplierBP:    11000, // 1.1×（参考实现 video_credit_multiplier）
		keyVideoRelayDurationMinSeconds:     4,
		keyVideoRelayDurationMaxSeconds:     15,
		keyVideoRelayDurationDefaultSeconds: 5,
		keyVideoRelayFpsDefault:             24,
		keyVideoRelayFpsMax:                 30,
		keyVideoRelayRatios:                 "16:9,9:16,1:1,adaptive",
		keyVideoRelayRatioDefault:           "16:9",
		keyVideoRelayResolutionDefault:      "720p",
		keyVideoRelayConcurrencyDefault:     5,     // 账户×模型并发默认上限（Unit 8）
		keyAdminSessionTTLSeconds:           43200, // 管理后台会话 TTL，12h（Unit 4）
		keyVideoRelaySafetyFactorBP:         12000, // 1.2×（reserve 安全系数，Unit 7/10）
		keyVideoRelayMinTokenFloor:          0,     // 默认无最低 token 下限（落地核对 seedance 官方）
		keyVideoResultURLTTLSeconds:         900,   // 15min 结果签名 URL TTL（Unit 10）
		// keyVideoCallbackBaseURL 无默认（空 = 纯轮询兜底，不注册回调路由）
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

	// RelayMaxContextTokens int64→int32：越界置 math.MaxInt32（catalog 限 [1, 1e6] 会拒），
	// 防止截断把超大值 wrap 成看似合法的小值绕过 relay.NewEnvCatalog 范围校验。
	relayMaxCtx := k.Int64(keyRelayMaxContextTokens)
	if relayMaxCtx < 0 || relayMaxCtx > math.MaxInt32 {
		relayMaxCtx = math.MaxInt32
	}

	cfg := &Config{
		HTTPAddr:               k.String(keyHTTPAddr),
		LogLevel:               k.String(keyLogLevel),
		PGDSN:                  k.String(keyPGDSN),
		RedisAddr:              k.String(keyRedisAddr),
		GatewayKEKV1:           k.String(keyGatewayKEKV1),
		AdminTokenSigningKey:   k.String(keyAdminTokenSigningKey),
		OTelExporter:           k.String(keyOTelExporter),
		CORSAllowedOrigins:     parseOrigins(k.String(keyCORSAllowedOrigins)),
		LedgerDriftAction:      k.String(keyLedgerDriftAction),
		GatewayEnv:             normalizeEnv(k.String(keyGatewayEnv)),
		TrustedProxyCIDRs:      parseOrigins(k.String(keyTrustedProxyCIDRs)),
		ListenTLS:              k.Bool(keyListenTLS),
		FrontTLSAck:            k.Bool(keyFrontTLSAck),
		TLSCertPath:            k.String(keyTLSCertPath),
		TLSKeyPath:             k.String(keyTLSKeyPath),
		TokenPepper:            k.String(keyTokenPepper),
		AdminAuditTier1Path:    k.String(keyAdminAuditTier1Path),
		AdminBootstrapUsername: k.String(keyAdminBootstrapUsername),
		AdminBootstrapPassword: k.String(keyAdminBootstrapPassword),
		AdminSessionTTLSeconds: k.Int64(keyAdminSessionTTLSeconds),

		RelayEnabled:               k.Bool(keyRelayEnabled),
		RelayModelName:             k.String(keyRelayModelName),
		RelayUpstreamProviderType:  k.String(keyRelayUpstreamProviderType),
		RelayUpstreamBaseURL:       k.String(keyRelayUpstreamBaseURL),
		RelayUpstreamAPIKey:        k.String(keyRelayUpstreamAPIKey),
		RelayUpstreamModelName:     k.String(keyRelayUpstreamModelName),
		RelayPriceInputPer1MMinor:  k.Int64(keyRelayPriceInputPer1MMinor),
		RelayPriceOutputPer1MMinor: k.Int64(keyRelayPriceOutputPer1MMinor),
		RelayMaxContextTokens:      int32(relayMaxCtx),

		AsyncEnabled:     k.Bool(keyAsyncEnabled),
		AsyncConcurrency: int(k.Int64(keyAsyncConcurrency)),
		RedisPassword:    k.String(keyRedisPassword),
		RedisTLSEnabled:  k.Bool(keyRedisTLSEnabled),

		VideoRelayEnabled:              k.Bool(keyVideoRelayEnabled),
		VideoRelayModelName:            k.String(keyVideoRelayModelName),
		VideoRelayProviderType:         k.String(keyVideoRelayProviderType),
		VideoRelayUpstreamBaseURL:      k.String(keyVideoRelayUpstreamBaseURL),
		VideoRelayUpstreamModelName:    k.String(keyVideoRelayUpstreamModelName),
		VideoRelayChannelName:          k.String(keyVideoRelayChannelName),
		VideoRelayPrice480pPer1MMinor:  k.Int64(keyVideoRelayPrice480pPer1MMinor),
		VideoRelayPrice720pPer1MMinor:  k.Int64(keyVideoRelayPrice720pPer1MMinor),
		VideoRelayPrice1080pPer1MMinor: k.Int64(keyVideoRelayPrice1080pPer1MMinor),
		VideoRelayBillingMultiplierBP:  k.Int64(keyVideoRelayBillingMultiplierBP),

		VideoRelayDurationMinSeconds:     k.Int64(keyVideoRelayDurationMinSeconds),
		VideoRelayDurationMaxSeconds:     k.Int64(keyVideoRelayDurationMaxSeconds),
		VideoRelayDurationDefaultSeconds: k.Int64(keyVideoRelayDurationDefaultSeconds),
		VideoRelayFpsDefault:             k.Int64(keyVideoRelayFpsDefault),
		VideoRelayFpsMax:                 k.Int64(keyVideoRelayFpsMax),
		VideoRelayRatios:                 parseOrigins(k.String(keyVideoRelayRatios)),
		VideoRelayRatioDefault:           k.String(keyVideoRelayRatioDefault),
		VideoRelayResolutionDefault:      k.String(keyVideoRelayResolutionDefault),
		VideoCallbackBaseURL:             strings.TrimSpace(k.String(keyVideoCallbackBaseURL)),
		VideoRelayConcurrencyDefault:     int(k.Int64(keyVideoRelayConcurrencyDefault)),
		VideoRelaySafetyFactorBP:         k.Int64(keyVideoRelaySafetyFactorBP),
		VideoRelayMinTokenFloor:          k.Int64(keyVideoRelayMinTokenFloor),
		VideoResultURLTTLSeconds:         k.Int64(keyVideoResultURLTTLSeconds),
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

	// ===== Phase 2 异步基座校验（仅 AsyncEnabled 时） =====
	// Redis 可达性不在此校验：由 main.go 启动期 ping fail-fast（与 pgxpool 同风格）。
	if c.AsyncEnabled {
		// 评审 #6：启用异步基座时 RedisAddr 必须非空（否则空值静默回落默认/误连）。
		if strings.TrimSpace(c.RedisAddr) == "" {
			return errors.New("GATEWAY_ASYNC_ENABLED=true 时 REDIS_ADDR 不能为空")
		}
		if c.AsyncConcurrency < minAsyncConcurrency || c.AsyncConcurrency > maxAsyncConcurrency {
			return fmt.Errorf(
				"GATEWAY_ASYNC_CONCURRENCY 非法值 %d，启用异步基座时须在 [%d, %d] 区间",
				c.AsyncConcurrency, minAsyncConcurrency, maxAsyncConcurrency,
			)
		}
	}

	// ===== Phase 2 视频中继跨字段依赖校验（Unit 4） =====
	// 视频中继走异步基座（Asynq submit/settle/recover/poll），故启用视频中继必须同时启用异步基座。
	// 视频字典字段自洽（model/price/取值档）的 fail-fast 在 video.NewEnvVideoCatalog（main.go 装配），
	// 不在此重复——config 只负责跨字段依赖，catalog 负责单字段合法性（与 RelayEnabled 同分工）。
	if c.VideoRelayEnabled && !c.AsyncEnabled {
		return errors.New(
			"GATEWAY_VIDEO_RELAY_ENABLED=true 时必须同时设置 GATEWAY_ASYNC_ENABLED=true" +
				"（视频中继依赖异步执行基座 Asynq+Redis）",
		)
	}

	// Unit 8：并发默认上限 + 回调 base URL 校验（仅 VideoRelayEnabled 时）。
	if c.VideoRelayEnabled {
		if c.VideoRelaySafetyFactorBP < minVideoSafetyFactorBP {
			return fmt.Errorf("%s=%d 不能小于 %d（安全系数须 ≥ 1.0×，否则 reserve 可能 under-reserve）",
				keyVideoRelaySafetyFactorBP, c.VideoRelaySafetyFactorBP, minVideoSafetyFactorBP)
		}
		if c.VideoRelayMinTokenFloor < 0 {
			return fmt.Errorf("%s=%d 不能为负", keyVideoRelayMinTokenFloor, c.VideoRelayMinTokenFloor)
		}
		if c.VideoResultURLTTLSeconds < 1 || c.VideoResultURLTTLSeconds > maxResultURLTTLSeconds {
			return fmt.Errorf("%s=%d 秒越界（须 1..%d）",
				keyVideoResultURLTTLSeconds, c.VideoResultURLTTLSeconds, maxResultURLTTLSeconds)
		}
		if c.VideoRelayConcurrencyDefault < 1 || c.VideoRelayConcurrencyDefault > maxVideoConcurrencyDefault {
			return fmt.Errorf(
				"GATEWAY_VIDEO_RELAY_CONCURRENCY_DEFAULT 非法值 %d，须在 [1, %d]（账户×模型并发默认上限）",
				c.VideoRelayConcurrencyDefault, maxVideoConcurrencyDefault,
			)
		}
		if c.VideoCallbackBaseURL != "" {
			https := strings.HasPrefix(c.VideoCallbackBaseURL, "https://")
			if !https && !strings.HasPrefix(c.VideoCallbackBaseURL, "http://") {
				return fmt.Errorf(
					"GATEWAY_VIDEO_CALLBACK_BASE_URL 必须以 http:// 或 https:// 开头（当前 %q）",
					c.VideoCallbackBaseURL,
				)
			}
			if c.GatewayEnv == EnvProduction && !https {
				return fmt.Errorf(
					"production 模式 GATEWAY_VIDEO_CALLBACK_BASE_URL 必须 https（防回调 token 明文传输；当前 %q）",
					c.VideoCallbackBaseURL,
				)
			}
		}
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
// 复用 crypto.DecodeHexOrBase64（评审 #11：消除与 crypto 包的重复实现）。
// pepper 长度可变（≥32 由调用方校验），故用不消歧义的 hex-优先解码，而非 KEK 的按长度路由。
func decodePepper(s string) ([]byte, error) {
	return crypto.DecodeHexOrBase64(s)
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

// Package audit 提供 Admin API 审计日志的结构化记录与多 sink 路由。
//
// 计划：docs/plans/2026-05-27-003-feat-workflow-d-min-admin-api-plan.md Unit 4
// 设计文档：docs/multimedia-gateway-design.md §9bis.6 R8（审计日志五件套）+ 决策 D3
//
// 核心概念：
//   - AuditRecord：一次 Admin API 请求的结构化审计记录（不依赖 HTTP 框架）
//   - Sink：实际落地目标（文件 / stderr / 未来 OTel / Loki）
//   - Logger：按 record.Tier 把 record 路由到正确 sink
//
// 分级（决策 D3）：
//   - Tier1（高价值）：refund / token lifecycle / idempotency_conflict / auth_failed →
//     同步 O_APPEND+O_SYNC 写本地文件，写失败即 HTTP 503 + readiness 关闸
//   - Tier2（低价值）：create account / recharge / balance read / 其他 → 异步 slog stderr
//
// 不入 DB：避免每请求 2 次写（auth + audit），由部署侧 log shipper 转 long-term storage。
package audit

import "time"

// AuditTier 审计行的优先级分级（决策 D3）。
type AuditTier int

const (
	// TierUnknown 未指定；Logger.Emit 应拒绝并返错（fail-closed，避免误降级）。
	TierUnknown AuditTier = 0
	// Tier1 高价值同步落盘（refund / token lifecycle / idempotency_conflict / auth_failed）。
	Tier1 AuditTier = 1
	// Tier2 低价值异步 stderr（create / recharge / balance read 等）。
	Tier2 AuditTier = 2
)

// AuditRecord 一次 HTTP 请求的审计行；admin 与 business 两类请求共用此 struct。
//
// JSON tag 严格控制对外字段名（部署侧 log shipper 依赖）；admin/business 各自路径
// 仅填用到的字段，其余 omitempty 不写。
//
// 通用字段（admin + business 共用）：
//
//   - Event：admin_audit / business_relay；便于日志聚合器分类
//   - Tier：1 / 2；Logger 路由依据；JSON 输出方便 log shipper 按 tier 分流
//   - RequestID：全局唯一；与 access log 同源（middleware.GetRequestID）
//   - TimestampUTC：record emit 时刻
//   - Actor：结构化身份串，如 "admin_token:42" / "business_key:99" / "anonymous"
//   - SourceIP：c.ClientIP() 解析后字符串
//   - Method / Path：path 用 Gin route template 避免高基数
//   - Status / DurationMs / OutcomeCode / Reason：HTTP / 业务级结果
//
// admin 专用字段（D-min Unit 4）：
//   - TokenID / TokenDescription / RequestHash / BodySizeBytes
//
// business 专用字段（F-min Unit 4，全部 omitempty）：
//   - BusinessAccountID / APIKeyID：业务账户 + API key 标识
//   - GatewayModel / UpstreamModel：网关字典名 / 上游真实 model 名
//   - InputTokens / OutputTokens：upstream usage（200 + 含 usage 时填）
//   - CostMinor：本次请求计费（minor unit）
//   - UpstreamStatus / UpstreamDurationMs：上游 HTTP 状态 / 耗时
//
// **不**记 messages body（含 PII / prompt 敏感；plan §决策 D8）。
type AuditRecord struct {
	Event        string    `json:"event"`
	Tier         AuditTier `json:"tier"`
	RequestID    string    `json:"request_id"`
	TimestampUTC time.Time `json:"timestamp_utc"`
	Actor        string    `json:"actor"`
	SourceIP     string    `json:"source_ip"`
	Method       string    `json:"method"`
	Path         string    `json:"path"`
	Status       int       `json:"status"`
	DurationMs   int64     `json:"duration_ms"`
	OutcomeCode  string    `json:"outcome_code"`
	Reason       string    `json:"reason,omitempty"`

	// ===== admin 专用字段（D-min Unit 4） =====
	TokenID          int64  `json:"token_id,omitempty"`
	TokenDescription string `json:"token_description,omitempty"`
	RequestHash      string `json:"request_hash,omitempty"`
	BodySizeBytes    int64  `json:"body_size_bytes,omitempty"`

	// ===== business 专用字段（F-min Unit 4） =====
	BusinessAccountID  string `json:"business_account_id,omitempty"`
	APIKeyID           int64  `json:"api_key_id,omitempty"`
	GatewayModel       string `json:"gateway_model,omitempty"`
	UpstreamModel      string `json:"upstream_model,omitempty"`
	InputTokens        int    `json:"input_tokens,omitempty"`
	OutputTokens       int    `json:"output_tokens,omitempty"`
	CostMinor          int64  `json:"cost_minor,omitempty"`
	UpstreamStatus     int    `json:"upstream_status,omitempty"`
	UpstreamDurationMs int64  `json:"upstream_duration_ms,omitempty"`
}

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

// AuditRecord Admin API 一次请求的审计行；JSON tag 严格控制对外字段名（部署侧 log shipper 依赖）。
//
// 字段约定（计划 R8）：
//
//   - Event：固定 "admin_audit"，便于日志聚合器分类
//   - Tier：1 / 2；Logger 路由依据；JSON 输出方便 log shipper 按 tier 分流（如 Tier1 走 cold storage）
//   - RequestID：全局唯一；与 access log 同源（middleware.GetRequestID）
//   - TimestampUTC：record emit 时刻（不是 request 入站时刻；二者 latency 由 duration_ms 推出）
//   - TokenID：鉴权通过后填；auth_failed 时为 0
//   - TokenDescription：运维可读标签（不暴露 token hash）；auth_failed 时空串
//   - Actor：结构化身份串，如 "admin_token:42" / "anonymous"（与 ledger.Actor 风格平行）
//   - SourceIP：c.ClientIP() 解析后的字符串（决策 D5 + Unit 7 trusted proxies 配置）
//   - Method：HTTP method（GET/POST/...）
//   - Path：Gin route template（c.FullPath()，如 "/admin/v1/business-accounts/:id/recharge"）；
//     避免高基数 label，便于 metric / 日志聚合
//   - RequestHash：sha256(method + " " + path + "?" + sorted_query + "\n" + body[:64KB])[:32] hex（决策 D8）
//   - BodySizeBytes：原始 body 字节数；body > 64KB 时 audit 行注明并只 hash 前 64KB
//   - Status：HTTP 响应 code
//   - DurationMs：handler + middleware 总耗时
//   - OutcomeCode：业务级 outcome 串（如 "ok" / "unauthorized" / "idempotency_conflict" / "internal_error"）
//   - Reason：可选；auth_failed / scope_denied 等 fail-closed 路径补充原因字符串
type AuditRecord struct {
	Event            string    `json:"event"`
	Tier             AuditTier `json:"tier"`
	RequestID        string    `json:"request_id"`
	TimestampUTC     time.Time `json:"timestamp_utc"`
	TokenID          int64     `json:"token_id"`
	TokenDescription string    `json:"token_description,omitempty"`
	Actor            string    `json:"actor"`
	SourceIP         string    `json:"source_ip"`
	Method           string    `json:"method"`
	Path             string    `json:"path"`
	RequestHash      string    `json:"request_hash"`
	BodySizeBytes    int64     `json:"body_size_bytes"`
	Status           int       `json:"status"`
	DurationMs       int64     `json:"duration_ms"`
	OutcomeCode      string    `json:"outcome_code"`
	Reason           string    `json:"reason,omitempty"`
}

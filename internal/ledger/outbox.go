package ledger

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// OutboxPublisher 同事务事件发布抽象。
//
// 接口在 internal/ledger 包内定义（DIP：consumer-owns-interface），
// 具体实现在 internal/outbox 包（PostgresPublisher 走 webhook_event_outbox INSERT）。
//
// **签名要求**：所有方法接受 pgx.Tx 而非 *pgxpool.Pool —— 强制调用方提供事务，
// 防止「ledger 写入成功但 outbox 没发」的双写不一致。
type OutboxPublisher interface {
	// PublishInTx 在调用方提供的事务内 INSERT outbox event。
	//
	// 调用方负责：BeginTx → ledger 写 → PublishInTx → Commit/Rollback；
	// 本接口实现不开新 tx，也不 Commit/Rollback，单纯 INSERT。
	PublishInTx(ctx context.Context, tx pgx.Tx, event Event) error
}

// Event 待发布的事件载荷。
//
// service 层根据写操作类型构造对应的 Payload（typed struct → json.Marshal → []byte），
// 然后填充 IsFinancial / RetentionUntil / DeliveryIdempotencyKey 调用 PublishInTx。
type Event struct {
	// Type 事件类型，如 EventTypeAccountRecharged。
	Type EventType

	// BusinessAccountID 关联账户 ID；非账户绑定的全局事件可留空（P0 全部带账户）。
	BusinessAccountID string

	// Payload typed payload struct 的 json.Marshal 结果；不接受 interface{} 透传。
	Payload []byte

	// IsFinancial 财务事件标志：
	//   true  → outbox 保留 ≥ 1 年（recharged / refunded）
	//   false → 保留 5 分钟（created / frozen / unfrozen 状态事件）
	// 详见计划 Unit 4 + 设计文档 §9bis.4.1。
	IsFinancial bool

	// RetentionUntil 保留截止时间；建议调用方根据 IsFinancial 算好。
	RetentionUntil time.Time

	// DeliveryIdempotencyKey 投递幂等键，全局唯一约束（webhook_event_outbox.delivery_idempotency_key）。
	// 推荐格式：`<event_type>:<account_id>:<ledger_entry_id_or_correlation_id>`。
	DeliveryIdempotencyKey string
}

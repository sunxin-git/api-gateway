// Package outbox 提供 webhook_event_outbox 的 INSERT 实现（PostgresPublisher）。
//
// 本工作流（Phase 2 E）仅实现「写出箱」一侧；
// claim/lease 抢占 + HTTP 推送 + DLQ 处理由 Phase 2 C-min（outbox dispatcher）落地。
//
// 设计文档：docs/multimedia-gateway-design.md §9bis.4.1
// 实施计划：docs/plans/2026-05-26-002-...-plan.md Unit 4
package outbox

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/sunxin-git/api-gateway/internal/db"
	"github.com/sunxin-git/api-gateway/internal/ledger"
)

// PostgresPublisher 是 ledger.OutboxPublisher 的 Postgres 实现。
//
// 不持有 *pgxpool.Pool —— PublishInTx 接受调用方提供的 pgx.Tx，
// 因此本结构体完全无状态（除可注入的 sqlc Queries 工厂以外）；
// 多 goroutine 共享同一实例是安全的。
type PostgresPublisher struct{}

// NewPostgresPublisher 构造无状态 Publisher。
func NewPostgresPublisher() *PostgresPublisher {
	return &PostgresPublisher{}
}

// 编译期断言实现接口（DIP 健康检查）。
var _ ledger.OutboxPublisher = (*PostgresPublisher)(nil)

// PublishInTx 在调用方提供的 pgx.Tx 内 INSERT 一条 outbox event。
//
// 行为：
//   - retention_until / is_financial / delivery_idempotency_key 由 event 字段提供（service 算好）
//   - delivery_status 默认 'pending'，等待 dispatcher 扫描
//   - 同 delivery_idempotency_key 第二次 INSERT 触发 UNIQUE 冲突 → 包装为明确 error
//
// 返回 error 时调用方应回滚整 tx（账本写 + outbox 写双双失败）。
func (p *PostgresPublisher) PublishInTx(ctx context.Context, tx pgx.Tx, event ledger.Event) error {
	if event.Type == "" {
		return fmt.Errorf("outbox.PublishInTx: event.Type 不能为空")
	}
	if event.DeliveryIdempotencyKey == "" {
		return fmt.Errorf("outbox.PublishInTx: event.DeliveryIdempotencyKey 不能为空")
	}
	if event.RetentionUntil.IsZero() {
		return fmt.Errorf("outbox.PublishInTx: event.RetentionUntil 不能为零值")
	}
	if len(event.Payload) == 0 {
		return fmt.Errorf("outbox.PublishInTx: event.Payload 不能为空")
	}

	// business_account_id 在 P0 全部带账户；空字符串视为「全局事件」可入 NULL（schema 允许 NULL）。
	accIDArg := pgtype.Text{}
	if event.BusinessAccountID != "" {
		accIDArg.String = event.BusinessAccountID
		accIDArg.Valid = true
	}

	queries := db.New(tx)
	_, err := queries.InsertOutboxEvent(ctx, db.InsertOutboxEventParams{
		BusinessAccountID:      accIDArg,
		EventType:              string(event.Type),
		Payload:                event.Payload,
		IsFinancial:            event.IsFinancial,
		RetentionUntil:         event.RetentionUntil,
		DeliveryIdempotencyKey: event.DeliveryIdempotencyKey,
	})
	if err != nil {
		return fmt.Errorf("outbox.PublishInTx: INSERT 失败 (event_type=%s, idem_key=%s): %w",
			event.Type, event.DeliveryIdempotencyKey, err)
	}
	return nil
}

package ledger

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/sunxin-git/api-gateway/internal/db"
)

// =============================================================================
// freezeInTx / unfreezeInTx —— 同包可见的事务内辅助
// =============================================================================

// freezeInTx 在调用方提供的 tx 内冻结账户余额。
//
// 行为：
//   - CAS 0 行（frozen 已 true）→ 视为幂等成功，**不**发 outbox event；返 nil
//   - CAS 0 行（version 不匹配）→ ErrVersionConflict
//   - CAS 1 行 → 发 outbox account.frozen + reason_code → 返 nil
//
// 调用方（公开 Freeze / rebuild TX1）负责 BeginTx / Commit。
func (s *PostgresService) freezeInTx(ctx context.Context, tx pgx.Tx, actor Actor, accountID string, reason ReasonCode) error {
	q := s.queries.WithTx(tx)

	bal, err := q.GetBalanceInTx(ctx, accountID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAccountNotFound
		}
		return fmt.Errorf("GetBalanceInTx 失败: %w", err)
	}

	if bal.Frozen {
		// 已 frozen：幂等成功；不发 outbox（防重复事件）。
		s.log.Info("freezeInTx 幂等成功（已 frozen）",
			slog.String("account_id", accountID),
			slog.String("actor", actor.String()),
			slog.String("requested_reason", string(reason)))
		return nil
	}

	_, err = q.FreezeAtomic(ctx, db.FreezeAtomicParams{
		FrozenReason:      pgtype.Text{String: string(reason), Valid: true},
		BusinessAccountID: accountID,
		ExpectedVersion:   bal.Version,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// 同 tx 内 fresh-read：判 frozen / version。
			cur, e2 := q.GetBalanceInTx(ctx, accountID)
			if e2 != nil {
				return fmt.Errorf("CAS 失败后 fresh-read 也失败: %w", e2)
			}
			if cur.Frozen {
				// 竞态：另一个 freeze 已先一步成功。本次幂等。
				return nil
			}
			return ErrVersionConflict
		}
		return fmt.Errorf("FreezeAtomic 失败: %w", err)
	}

	// outbox event。
	now := time.Now().UTC()
	payload, _ := json.Marshal(AccountFrozenPayload{
		BusinessAccountID: accountID,
		ReasonCode:        reason,
		OccurredAt:        now,
	})
	evt := Event{
		Type:                   EventTypeAccountFrozen,
		BusinessAccountID:      accountID,
		Payload:                payload,
		IsFinancial:            false,
		RetentionUntil:         now.Add(nonFinancialRetention),
		DeliveryIdempotencyKey: fmt.Sprintf("%s:%s:%d", EventTypeAccountFrozen, accountID, now.UnixNano()),
	}
	if err := s.outbox.PublishInTx(ctx, tx, evt); err != nil {
		return fmt.Errorf("outbox.Publish 失败: %w", err)
	}
	return nil
}

// unfreezeInTx 在调用方提供的 tx 内解冻账户。
//
// 行为对称 freezeInTx：已 unfrozen → 幂等成功不发事件；状态变更 → 发 account.unfrozen。
func (s *PostgresService) unfreezeInTx(ctx context.Context, tx pgx.Tx, actor Actor, accountID string, reason ReasonCode) error {
	q := s.queries.WithTx(tx)

	bal, err := q.GetBalanceInTx(ctx, accountID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAccountNotFound
		}
		return fmt.Errorf("GetBalanceInTx 失败: %w", err)
	}

	if !bal.Frozen {
		s.log.Info("unfreezeInTx 幂等成功（已 unfrozen）",
			slog.String("account_id", accountID),
			slog.String("actor", actor.String()),
			slog.String("requested_reason", string(reason)))
		return nil
	}

	_, err = q.UnfreezeAtomic(ctx, db.UnfreezeAtomicParams{
		FrozenReason:      pgtype.Text{String: string(reason), Valid: true},
		BusinessAccountID: accountID,
		ExpectedVersion:   bal.Version,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			cur, e2 := q.GetBalanceInTx(ctx, accountID)
			if e2 != nil {
				return fmt.Errorf("CAS 失败后 fresh-read 也失败: %w", e2)
			}
			if !cur.Frozen {
				return nil
			}
			return ErrVersionConflict
		}
		return fmt.Errorf("UnfreezeAtomic 失败: %w", err)
	}

	now := time.Now().UTC()
	payload, _ := json.Marshal(AccountUnfrozenPayload{
		BusinessAccountID: accountID,
		ReasonCode:        reason,
		OccurredAt:        now,
	})
	evt := Event{
		Type:                   EventTypeAccountUnfrozen,
		BusinessAccountID:      accountID,
		Payload:                payload,
		IsFinancial:            false,
		RetentionUntil:         now.Add(nonFinancialRetention),
		DeliveryIdempotencyKey: fmt.Sprintf("%s:%s:%d", EventTypeAccountUnfrozen, accountID, now.UnixNano()),
	}
	if err := s.outbox.PublishInTx(ctx, tx, evt); err != nil {
		return fmt.Errorf("outbox.Publish 失败: %w", err)
	}
	return nil
}

// =============================================================================
// rebuild 专用：ExpectedBalance + replaceBalanceInTx（CAS by last_ledger_id）
// =============================================================================

// ExpectedBalance 是 rebuild 在 TX2 从 ledger 全量回放算出的应有余额快照。
//
// 用作 TX3 写回 balance 的入参；rebuild 自身在 TX2 也读取 max(ledger_entry_id)
// 作为 expected_last_ledger_id（防 TX2-TX3 间被并发新 entry 打断）。
type ExpectedBalance struct {
	Available     int64
	Reserved      int64
	UsedTotal     int64
	RechargeTotal int64
	RefundTotal   int64
}

// replaceBalanceInTx 在调用方提供的 tx 内用 ReplaceBalance（last_ledger_id CAS）写回 balance。
//
// 返回 rowsAffected：
//   - 1 表示 CAS 成功（TX2-TX3 间无新 entry 写入）
//   - 0 表示 last_ledger_id 已被并发写入更改，调用方应回到 TX2 重读快照
//
// 不调用 Commit/Rollback：由调用方负责事务生命周期。
func (s *PostgresService) replaceBalanceInTx(
	ctx context.Context,
	tx pgx.Tx,
	accountID string,
	expected ExpectedBalance,
	expectedLastLedgerID int64,
) (int64, error) {
	q := s.queries.WithTx(tx)
	rows, err := q.ReplaceBalance(ctx, db.ReplaceBalanceParams{
		Available:            expected.Available,
		Reserved:             expected.Reserved,
		UsedTotal:            expected.UsedTotal,
		RechargeTotal:        expected.RechargeTotal,
		RefundTotal:          expected.RefundTotal,
		BusinessAccountID:    accountID,
		ExpectedLastLedgerID: expectedLastLedgerID,
	})
	if err != nil {
		return 0, fmt.Errorf("ReplaceBalance 失败: %w", err)
	}
	return rows, nil
}

// =============================================================================
// CAS 失败错误分类（CAS 0 行后同 tx 内 fresh-read 判错）
// =============================================================================

// classifyRechargeError CAS 失败：按 frozen → version 顺序判错。
// 不存在 → ErrAccountNotFound。
func (s *PostgresService) classifyRechargeError(ctx context.Context, q *db.Queries, accountID string, expectedVersion int64) error {
	cur, err := q.GetBalanceInTx(ctx, accountID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAccountNotFound
		}
		return fmt.Errorf("CAS 失败后 fresh-read 失败: %w", err)
	}
	if cur.Frozen {
		return ErrAccountFrozen
	}
	if cur.Version != expectedVersion {
		return ErrVersionConflict
	}
	// 不可达：CAS 失败但 frozen=false 且 version 一致 → 应当成功
	return fmt.Errorf("Recharge CAS 失败但 fresh-read 一致（疑似 PG 时序异常）: account=%s version=%d", accountID, expectedVersion)
}

// classifyReserveError CAS 失败：按 frozen → version → available 顺序判错。
func (s *PostgresService) classifyReserveError(ctx context.Context, q *db.Queries, accountID string, expectedVersion, requestedAmount int64) error {
	cur, err := q.GetBalanceInTx(ctx, accountID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAccountNotFound
		}
		return fmt.Errorf("CAS 失败后 fresh-read 失败: %w", err)
	}
	if cur.Frozen {
		return ErrAccountFrozen
	}
	if cur.Version != expectedVersion {
		return ErrVersionConflict
	}
	if cur.Available < requestedAmount {
		return ErrInsufficientBalance
	}
	return fmt.Errorf("Reserve CAS 失败但 fresh-read 一致: account=%s version=%d amount=%d available=%d",
		accountID, expectedVersion, requestedAmount, cur.Available)
}

// classifyCommitError CAS 失败：按 version → reserved 顺序判错（commit 不查 frozen）。
func (s *PostgresService) classifyCommitError(ctx context.Context, q *db.Queries, accountID string, expectedVersion, reserveAmount int64) error {
	cur, err := q.GetBalanceInTx(ctx, accountID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAccountNotFound
		}
		return fmt.Errorf("CAS 失败后 fresh-read 失败: %w", err)
	}
	if cur.Version != expectedVersion {
		return ErrVersionConflict
	}
	if cur.Reserved < reserveAmount {
		return ErrInsufficientReserved
	}
	return fmt.Errorf("Commit CAS 失败但 fresh-read 一致: account=%s version=%d reserve=%d reserved=%d",
		accountID, expectedVersion, reserveAmount, cur.Reserved)
}

// classifyReleaseError CAS 失败：按 version → reserved 顺序判错（release 不查 frozen）。
func (s *PostgresService) classifyReleaseError(ctx context.Context, q *db.Queries, accountID string, expectedVersion, amount int64) error {
	cur, err := q.GetBalanceInTx(ctx, accountID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAccountNotFound
		}
		return fmt.Errorf("CAS 失败后 fresh-read 失败: %w", err)
	}
	if cur.Version != expectedVersion {
		return ErrVersionConflict
	}
	if cur.Reserved < amount {
		return ErrInsufficientReserved
	}
	return fmt.Errorf("Release CAS 失败但 fresh-read 一致: account=%s amount=%d reserved=%d",
		accountID, amount, cur.Reserved)
}

// classifyRefundError CAS 失败：按 version → used_total 顺序判错（refund 不查 frozen）。
func (s *PostgresService) classifyRefundError(ctx context.Context, q *db.Queries, accountID string, expectedVersion, amount int64) error {
	cur, err := q.GetBalanceInTx(ctx, accountID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAccountNotFound
		}
		return fmt.Errorf("CAS 失败后 fresh-read 失败: %w", err)
	}
	if cur.Version != expectedVersion {
		return ErrVersionConflict
	}
	if cur.UsedTotal < amount {
		return ErrInsufficientUsed
	}
	return fmt.Errorf("Refund CAS 失败但 fresh-read 一致: account=%s amount=%d used_total=%d",
		accountID, amount, cur.UsedTotal)
}

// =============================================================================
// canonicalizeBody —— 简化 JCS 替代（计划 Unit 5 Feas F11）
// =============================================================================

// canonicalizeBody 计算 RechargeBody 的 canonical sha256。
//
// 实现：reflect 字段名 lexicographic 排序后 json.Marshal → sha256。
// 不引入 RFC 8785 JCS 完整实现；本算法在 P0 admin-cli 路径足够（字段集已限定为 int64 + string）。
//
// 输入 nil 返 ([]byte{}, nil)（service 决定如何处理空 body）。
func canonicalizeBody(body *RechargeBody) ([]byte, error) {
	if body == nil {
		return nil, nil
	}

	// 用 reflect 把 struct 字段按 json tag 名 lexicographic 排序后构造 ordered map。
	v := reflect.ValueOf(*body)
	t := v.Type()

	type kv struct {
		Key string
		Val interface{}
	}
	pairs := make([]kv, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		fld := t.Field(i)
		// 取 json tag 第一个段作为 key；无 tag 用字段名。
		tag := fld.Tag.Get("json")
		name := fld.Name
		if tag != "" {
			if comma := indexByte(tag, ','); comma >= 0 {
				name = tag[:comma]
			} else {
				name = tag
			}
		}
		if name == "-" {
			continue
		}
		pairs = append(pairs, kv{Key: name, Val: v.Field(i).Interface()})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].Key < pairs[j].Key })

	// 构造确定的 JSON：手写 buffer 保证 key 顺序（json.Marshal map 不保证）。
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, p := range pairs {
		if i > 0 {
			buf.WriteByte(',')
		}
		// key
		keyBytes, err := json.Marshal(p.Key)
		if err != nil {
			return nil, fmt.Errorf("marshal key %q 失败: %w", p.Key, err)
		}
		buf.Write(keyBytes)
		buf.WriteByte(':')
		// value
		valBytes, err := json.Marshal(p.Val)
		if err != nil {
			return nil, fmt.Errorf("marshal value of %q 失败: %w", p.Key, err)
		}
		buf.Write(valBytes)
	}
	buf.WriteByte('}')

	sum := sha256.Sum256(buf.Bytes())
	return sum[:], nil
}

// indexByte 是 strings.IndexByte 的内联（避免 import strings）。
func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// bytesEqual 比较两个 byte slice 是否完全相等（含 nil/空切片视为相等）。
func bytesEqual(a, b []byte) bool {
	return bytes.Equal(a, b)
}

// =============================================================================
// 通用小工具
// =============================================================================

// nullableText 把可空字符串包成 pgtype.Text（空字符串 → Valid=false）。
func nullableText(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}

// textOrEmpty 从 pgtype.Text 取字符串（NULL → ""）。
func textOrEmpty(t pgtype.Text) string {
	if !t.Valid {
		return ""
	}
	return t.String
}

// mustMetadata 把 nil / 空切片转 '{}'::jsonb。
func mustMetadata(m []byte) []byte {
	if len(m) == 0 {
		return []byte("{}")
	}
	return m
}

// toDBActorType 把 service 层 ActorType 转 db 层 ActorType。
//
// 两个类型常量值相同（admin_token/cli/system/task），仅 Go 类型名分离；
// 转换是字符串重定义，编译期成本为 0。
func toDBActorType(t ActorType) db.ActorType {
	return db.ActorType(t)
}

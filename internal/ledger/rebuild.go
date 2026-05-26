package ledger

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/sunxin-git/api-gateway/internal/db"
)

// =============================================================================
// RebuildBalance —— 余额重建（3 个独立 tx）
//
// 计划 Unit 8：拆分 freeze / read snapshot REPEATABLE READ / replace+unfreeze 三个独立事务，
// 避免长事务持锁阻塞 inflight Commit/Release。
//
// 流程：
//   TX1 (ReadCommitted) — freezeInTx(reason=rebuild_in_progress)；幂等
//   TX2 (RepeatableRead, ReadOnly) — 全量 SELECT ledger + 应用层累加得 ExpectedBalance + 取 max_ledger_id 作为 CAS 锚点
//   TX3 (ReadCommitted) — replaceBalanceInTx CAS by last_ledger_id；命中后 unfreezeInTx(reason=rebuild_completed)
//
// TX2/TX3 形成「光学锁」：TX3 CAS 0 行 = TX2 后又有并发写入 → 回到 TX2 重读快照；
// 最多 maxRebuildRetries 次仍失败 → ErrRebuildContention，账户保持 frozen 等运营。
//
// **不**暴露 admin-cli 子命令（计划 §Scope）。
// =============================================================================

// maxRebuildRetries TX2-TX3 重读快照的最大次数。
//
// 数学：高频账户每秒新写入 N 笔，TX2 读全量耗时 T 秒；
// CAS 命中率 ≈ exp(-N*T)；N=10qps T=0.1s 时单次命中率 37%，3 次累积 75%。
// P0 暂用 3 次；后续若高频账户大量出现可调到 5。
const maxRebuildRetries = 3

// RebuildBalance 从 ledger 全量回放重建 balance。
//
// 入口校验：actor 合法 + accountID 非空。
//
// 错误：
//   - ErrAccountNotFound  账户不存在（TX1 freezeInTx 返回）
//   - ErrRebuildContention TX2-TX3 重试耗尽
//   - 其他底层 PG 错误（包装 %w）
//
// 成功后返回最新 balance（用 GetBalance 读，不在 tx 内）。
func (s *PostgresService) RebuildBalance(ctx context.Context, actor Actor, accountID string) (*Balance, error) {
	if err := actor.Validate(); err != nil {
		return nil, fmt.Errorf("invalid actor: %w", err)
	}
	if accountID == "" {
		return nil, errors.New("accountID 不能为空")
	}

	// ============ TX1 — Freeze ============
	if err := s.rebuildTx1Freeze(ctx, actor, accountID); err != nil {
		return nil, fmt.Errorf("rebuild TX1 freeze: %w", err)
	}

	// TX2 + TX3 重试循环。
	var lastErr error
	for attempt := 0; attempt < maxRebuildRetries; attempt++ {
		expected, snapshotLastLedgerID, err := s.rebuildTx2ReadSnapshot(ctx, accountID)
		if err != nil {
			return nil, fmt.Errorf("rebuild TX2 read snapshot (attempt %d): %w", attempt, err)
		}

		rows, err := s.rebuildTx3ReplaceAndUnfreeze(ctx, actor, accountID, expected, snapshotLastLedgerID)
		if err != nil {
			return nil, fmt.Errorf("rebuild TX3 replace+unfreeze (attempt %d): %w", attempt, err)
		}
		if rows == 1 {
			// CAS 成功：返回最新 balance。
			s.log.Info("rebuild 成功",
				slog.String("account_id", accountID),
				slog.String("actor", actor.String()),
				slog.Int("attempt", attempt),
				slog.Int64("expected_last_ledger_id", snapshotLastLedgerID))
			return s.GetBalance(ctx, accountID)
		}
		// rows == 0：CAS 失败（TX2-TX3 间有新 entry 写入）；回到 TX2 重读。
		lastErr = fmt.Errorf("CAS by last_ledger_id=%d 0 rows affected (concurrent write detected)", snapshotLastLedgerID)
		s.log.Warn("rebuild CAS 失败，回到 TX2 重读快照",
			slog.String("account_id", accountID),
			slog.Int("attempt", attempt),
			slog.Int64("snapshot_last_ledger_id", snapshotLastLedgerID))
	}

	// 重试耗尽。
	s.log.Error("rebuild 重试耗尽，账户保持 frozen",
		slog.String("account_id", accountID),
		slog.String("actor", actor.String()),
		slog.Int("max_attempts", maxRebuildRetries),
		slog.String("last_err", fmt.Sprintf("%v", lastErr)))
	return nil, ErrRebuildContention
}

// rebuildTx1Freeze TX1：自带 tx 调用 freezeInTx。
//
// 若账户已 frozen（无论 reason）：freezeInTx 视为幂等成功，不发新事件；
// 此场景见于 reconciler 已 freeze 后运营主动调用 rebuild 修复。
func (s *PostgresService) rebuildTx1Freeze(ctx context.Context, actor Actor, accountID string) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("BeginTx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := s.freezeInTx(ctx, tx, actor, accountID, ReasonCodeRebuildInProgress); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("Commit: %w", err)
	}
	return nil
}

// rebuildTx2ReadSnapshot TX2：REPEATABLE READ, READ ONLY 全量读 ledger 算 ExpectedBalance。
//
// 关键决策：只读 tx 不持 balance 行锁 → inflight Commit/Release 不被阻塞。
//
// 返回：
//   - expected：5 SUM 字段算出的应有余额
//   - lastLedgerID：账户当前最大 ledger entry id，作为 TX3 CAS 锚点
func (s *PostgresService) rebuildTx2ReadSnapshot(
	ctx context.Context,
	accountID string,
) (ExpectedBalance, int64, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return ExpectedBalance{}, 0, fmt.Errorf("BeginTx: %w", err)
	}
	// 只读 tx：始终 Rollback（不留 commit 状态）。
	defer func() { _ = tx.Rollback(ctx) }()

	q := s.queries.WithTx(tx)
	entries, err := q.GetLedgerEntriesForRebuild(ctx, accountID)
	if err != nil {
		return ExpectedBalance{}, 0, fmt.Errorf("GetLedgerEntriesForRebuild: %w", err)
	}

	var (
		expected     ExpectedBalance
		lastLedgerID int64
	)
	for _, e := range entries {
		expected.Available += e.AvailableDelta
		expected.Reserved += e.ReservedDelta
		expected.UsedTotal += e.UsedDelta
		switch e.EntryType {
		case db.LedgerEntryTypeRecharge:
			expected.RechargeTotal += e.Amount
		case db.LedgerEntryTypeRefund:
			expected.RefundTotal += e.Amount
		}
		if e.ID > lastLedgerID {
			lastLedgerID = e.ID
		}
	}

	// 不变量自检：rebuild 算出的 5 字段必须守恒，否则 ledger 本身已被污染。
	// 这里 panic 是有意为之 —— 数据已损坏，直接给运营硬信号；不进 outbox/不 wrap。
	if expected.Available < 0 || expected.Reserved < 0 || expected.UsedTotal < 0 ||
		expected.RechargeTotal < 0 || expected.RefundTotal < 0 {
		s.log.Error("rebuild 算出负值，ledger 数据已污染",
			slog.String("account_id", accountID),
			slog.Any("expected", expected))
		return ExpectedBalance{}, 0, fmt.Errorf("rebuild ledger 数据污染（含负值）：account=%s expected=%+v", accountID, expected)
	}
	if expected.Available+expected.Reserved+expected.UsedTotal != expected.RechargeTotal {
		s.log.Error("rebuild 算出不变量违反",
			slog.String("account_id", accountID),
			slog.Any("expected", expected))
		return ExpectedBalance{}, 0, fmt.Errorf("rebuild 不变量违反：account=%s available(%d)+reserved(%d)+used(%d) != recharge(%d)",
			accountID, expected.Available, expected.Reserved, expected.UsedTotal, expected.RechargeTotal)
	}

	return expected, lastLedgerID, nil
}

// rebuildTx3ReplaceAndUnfreeze TX3：CAS by last_ledger_id + unfreezeInTx。
//
// 返回：
//   - 1 行：CAS 成功，已 unfreeze；调用方应停止重试
//   - 0 行：CAS 失败（TX2 后有新 entry 写入）；调用方应回到 TX2
//
// 注意：CAS 0 行时，必须**显式** Rollback（防 unfreezeInTx 仍执行写）；本函数借 defer tx.Rollback
// 兜底，但提前在 0 行分支 return 让 unfreezeInTx 不被调用。
func (s *PostgresService) rebuildTx3ReplaceAndUnfreeze(
	ctx context.Context,
	actor Actor,
	accountID string,
	expected ExpectedBalance,
	expectedLastLedgerID int64,
) (int64, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, fmt.Errorf("BeginTx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := s.replaceBalanceInTx(ctx, tx, accountID, expected, expectedLastLedgerID)
	if err != nil {
		return 0, err
	}
	if rows == 0 {
		// CAS 失败：显式 Rollback（已经 defer 兜底，但 explicit return 给阅读者清晰信号）。
		return 0, nil
	}

	// CAS 成功：unfreeze 同 tx。
	if err := s.unfreezeInTx(ctx, tx, actor, accountID, ReasonCodeRebuildCompleted); err != nil {
		return 0, fmt.Errorf("unfreezeInTx: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("Commit: %w", err)
	}
	return rows, nil
}

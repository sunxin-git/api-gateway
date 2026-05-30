package task

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/sunxin-git/api-gateway/internal/db"
	"github.com/sunxin-git/api-gateway/internal/ledger"
	"github.com/sunxin-git/api-gateway/internal/relay/video"
)

// settle 编排（settle worker 调用）。从 service.go 拆出（控制单文件 ≤~400 行，CLAUDE.md SRP）。

// settleTask 结算一个已进上游终态的任务（plan §Unit 6 settle）：
//
//   - COMPLETED：Poll 反查真实 usage → commit(≤reserve)；缺 usage / Poll 失败 → settle_failed。
//   - FAILED/CANCELLED/EXPIRED：release(reserve) 全额回退。
//
// 可重入：已 SETTLED/SETTLE_FAILED → no-op；CAS terminal→SETTLING 抢占，输则放弃（他人处理）。
// 账本幂等（同 correlation）+ Commit 的 ErrAlreadySettled 兜底双 settle job。
//
// ⚠️ 已知窗口（6b 兜底）：CAS→SETTLING 后若 worker 进程**硬崩溃**（panic/OOM）于 commit/release
// 与终态 CAS 之间，task 卡 SETTLING（钱可能已落账、态未终）。本进程内的 finalizeSettle 已对
// ctx 过期 / 瞬时 DB 错做独立 ctx + 重试，把窗口收窄到「硬崩溃」；硬崩溃后的 SETTLING 复活
// 须 6b reconciler 扫卡住 SETTLING 重投（靠账本 ErrAlreadySettled 幂等收敛）。
func (s *Service) settleTask(ctx context.Context, taskID string) error {
	t, err := s.q.GetTaskByID(ctx, taskID)
	if err != nil {
		return fmt.Errorf("task.settle get: %w", err)
	}
	switch t.Status {
	case db.TaskStatusSETTLED, db.TaskStatusSETTLEFAILED:
		return nil // 已结算终态，幂等
	case db.TaskStatusSETTLING:
		// 他人在结算 / 上次崩溃在 SETTLING；幂等放弃（6b reconciler 兜底重投，靠账本幂等安全）。
		return nil
	}
	if !isUpstreamTerminal(t.Status) {
		// 非上游终态（早投 / 还在途）→ 无可结算，放弃
		return nil
	}
	terminal := t.Status

	// CAS terminal → SETTLING（抢占结算）
	affected, err := s.q.CompareAndSwapTaskStatus(ctx, db.CompareAndSwapTaskStatusParams{
		ToStatus:   db.TaskStatusSETTLING,
		ID:         taskID,
		FromStatus: terminal,
	})
	if err != nil {
		return fmt.Errorf("task.settle cas settling: %w", err)
	}
	if affected == 0 {
		return nil // 他人抢到，放弃
	}

	snap, err := ParseSnapshot(t.FinancialSnapshot)
	if err != nil {
		s.logger.Error("task.settle: 快照损坏 → settle_failed", slog.String("task_id", taskID), slog.String("err", err.Error()))
		return s.casSettleFailed(taskID)
	}

	if terminal == db.TaskStatusCOMPLETED {
		return s.settleCompleted(t, snap)
	}
	return s.settleReleased(t, snap)
}

// settleCompleted COMPLETED 结算：Poll 真实 usage → commit；缺/非正 usage → settle_failed。
func (s *Service) settleCompleted(t db.Task, snap TaskFinancialSnapshot) error {
	usage := s.pollUsage(t, snap)
	// 缺 usage / Poll 失败 / 非正 token（防御：adapter 已挡 ≤0，此处兜底）→ settle_failed + 对账
	// （不按上界 commit、不静默 release，ADR-0006）。
	if usage == nil || usage.CompletionTokens <= 0 {
		s.logger.Error("task.settle: COMPLETED 缺可信 usage → settle_failed（reserve 留对账）",
			slog.String("task_id", t.ID))
		return s.casSettleFailed(t.ID)
	}
	settleMinor, capped := snap.SettleMinor(usage.CompletionTokens)
	if capped {
		s.logger.Warn("task.settle: settle 触顶 reserve（疑似 provider 计费异常）",
			slog.String("task_id", t.ID), slog.Int64("usage_tokens", usage.CompletionTokens))
	}
	actor := ledger.Actor{Type: ledger.ActorTypeTask, ID: t.ID}
	if err := s.commitWithRetry(actor, t.BusinessAccountID, snap.ReservationCorrelationID, settleMinor); err != nil {
		// 已 SETTLED 过（双 settle）→ 幂等成功，推进终态
		if errors.Is(err, ledger.ErrAlreadySettled) {
			return s.casSettled(t.ID)
		}
		s.logger.Error("task.settle: commit 永久失败 → settle_failed（reserve 留对账）",
			slog.String("task_id", t.ID), slog.String("err", err.Error()))
		return s.casSettleFailed(t.ID)
	}
	return s.casSettled(t.ID)
}

// settleReleased 失败终态结算：release reserve 全额回退。
func (s *Service) settleReleased(t db.Task, snap TaskFinancialSnapshot) error {
	actor := ledger.Actor{Type: ledger.ActorTypeTask, ID: t.ID}
	err := s.releaseWithRetry(actor, t.BusinessAccountID, snap.ReservationCorrelationID, snap.ReserveMinor)
	// ErrAlreadySettled = 已释放过（双 settle）→ 幂等成功。其余错误（含 ErrReserveNotFound——
	// reserve 本应在提交时必建，找不到属异常）→ settle_failed 交对账，**不**静默推 SETTLED（ce-review）。
	if err != nil && !errors.Is(err, ledger.ErrAlreadySettled) {
		s.logger.Error("task.settle: release 失败（含 reserve 不存在异常）→ settle_failed（对账）",
			slog.String("task_id", t.ID), slog.String("err", err.Error()))
		return s.casSettleFailed(t.ID)
	}
	return s.casSettled(t.ID)
}

// pollUsage settle 内独立 ctx Poll 反查上游真实 usage；nil = 缺 usage / Poll 失败（交 settle_failed）。
func (s *Service) pollUsage(t db.Task, snap TaskFinancialSnapshot) *video.UpstreamUsage {
	if !t.UpstreamTaskID.Valid || t.UpstreamTaskID.String == "" {
		return nil // 无上游 task_id，无法反查
	}
	entry, ok := s.catalog.Lookup(snap.GatewayModel)
	if !ok || entry == nil {
		return nil
	}
	pollCtx, cancel := context.WithTimeout(context.Background(), s.pollTO)
	defer cancel()
	creds, err := s.resolveCreds(pollCtx, t.ChannelID)
	if err != nil {
		s.logger.Error("task.settle: 凭据解密失败，无法 Poll 反查 usage",
			slog.String("task_id", t.ID), slog.String("err", err.Error()))
		return nil
	}
	res, err := s.adapter.Poll(pollCtx, entry, creds, t.UpstreamTaskID.String)
	if err != nil {
		s.logger.Warn("task.settle: Poll 反查 usage 失败",
			slog.String("task_id", t.ID), slog.String("err", err.Error()))
		return nil
	}
	return res.Usage // 缺 usage 时 adapter 已返 nil
}

// =============================================================================
// SETTLING → 终态推进（独立 ctx + 重试，钱已落账须尽力推进）
// =============================================================================

// casSettled / casSettleFailed 推进 SETTLING → 终态。
//
// **独立 ctx + 有限重试**（ce-review R-001）：到达这里时 ledger commit/release 已落账，
// task 状态必须尽力推到终态——否则 task 卡 SETTLING（钱已动、态未终）。用独立 ctx 不受
// settle worker 的 asynq ctx 过期影响；CAS 受影响 0 行（已被他人推进，如 6b）视为幂等成功。
func (s *Service) casSettled(taskID string) error {
	return s.finalizeSettle(taskID, db.TaskStatusSETTLED)
}

func (s *Service) casSettleFailed(taskID string) error {
	return s.finalizeSettle(taskID, db.TaskStatusSETTLEFAILED)
}

func (s *Service) finalizeSettle(taskID string, to db.TaskStatus) error {
	ctx, cancel := context.WithTimeout(context.Background(), s.settleTO)
	defer cancel()
	var lastErr error
	for attempt := 0; attempt <= len(settleRetryBackoff); attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(settleRetryBackoff[attempt-1]):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		// affected 0（已被推进）/ 1（本次推进）均算成功；仅 DB error 重试。
		if _, err := s.q.CompareAndSwapTaskStatus(ctx, db.CompareAndSwapTaskStatusParams{
			ToStatus: to, ID: taskID, FromStatus: db.TaskStatusSETTLING,
		}); err == nil {
			return nil
		} else {
			lastErr = err
		}
	}
	s.logger.Error("task.settle: 推进 SETTLING→终态永久失败；task 卡 SETTLING（钱已落账），待 6b reconciler 兜底",
		slog.String("task_id", taskID), slog.String("to", string(to)), slog.String("err", lastErr.Error()))
	return lastErr
}

// =============================================================================
// ledger commit/release 重试（独立 ctx + CAS 冲突退避；平行 relay handler，不复用）
// =============================================================================

var settleRetryBackoff = []time.Duration{100 * time.Millisecond, 300 * time.Millisecond, 1 * time.Second}

func (s *Service) commitWithRetry(actor ledger.Actor, accountID, correlationID string, actualCost int64) error {
	ctx, cancel := context.WithTimeout(context.Background(), s.settleTO)
	defer cancel()
	return retryOnCASConflict(ctx, settleRetryBackoff, func(ctx context.Context) error {
		_, err := s.ledger.Commit(ctx, actor, ledger.CommitParams{
			AccountID:     accountID,
			CorrelationID: correlationID,
			ActualCost:    actualCost,
			ReferenceType: referenceTypeVideoTask,
			ReferenceID:   correlationID,
		})
		return err
	})
}

func (s *Service) releaseWithRetry(actor ledger.Actor, accountID, correlationID string, amount int64) error {
	ctx, cancel := context.WithTimeout(context.Background(), s.settleTO)
	defer cancel()
	return retryOnCASConflict(ctx, settleRetryBackoff, func(ctx context.Context) error {
		_, err := s.ledger.Release(ctx, actor, ledger.ReleaseParams{
			AccountID:     accountID,
			CorrelationID: correlationID,
			Amount:        amount,
			ReferenceType: referenceTypeVideoTask,
			ReferenceID:   correlationID,
		})
		return err
	})
}

func (s *Service) releaseReserveBestEffort(actor ledger.Actor, accountID, correlationID string, amount int64, reason string) {
	if err := s.releaseWithRetry(actor, accountID, correlationID, amount); err != nil {
		s.logger.Error("task: 提交失败回滚 Release reserve 永久失败；orphan reserve（reconciler 6b 兜底）",
			slog.String("account_id", accountID), slog.String("correlation_id", correlationID),
			slog.String("reason", reason), slog.String("err", err.Error()))
	}
}

// retryOnCASConflict CAS（ledger.ErrVersionConflict）冲突时重试 + 退避；其他 error 立即返。
func retryOnCASConflict(ctx context.Context, backoffs []time.Duration, fn func(ctx context.Context) error) error {
	err := fn(ctx)
	if err == nil || !errors.Is(err, ledger.ErrVersionConflict) {
		return err
	}
	last := err
	for _, b := range backoffs {
		select {
		case <-time.After(b):
		case <-ctx.Done():
			return ctx.Err()
		}
		err = fn(ctx)
		if err == nil || !errors.Is(err, ledger.ErrVersionConflict) {
			return err
		}
		last = err
	}
	return last
}

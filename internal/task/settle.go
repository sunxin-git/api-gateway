package task

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

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
		// 已 SETTLED 过（双 settle）→ 幂等成功，推进终态 + 触发结果转存
		if errors.Is(err, ledger.ErrAlreadySettled) {
			return s.settleSucceeded(t.ID)
		}
		s.logger.Error("task.settle: commit 永久失败 → settle_failed（reserve 留对账）",
			slog.String("task_id", t.ID), slog.String("err", err.Error()))
		return s.casSettleFailed(t.ID)
	}
	// 成功结算 → 推进 SETTLED + 触发结果转存（Unit 9，best-effort）。
	return s.settleSucceeded(t.ID)
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

// =============================================================================
// SETTLING 崩溃恢复（6b fetch reconciler 调用；与正常 settle 的 SETTLING→no-op 路径分离）
// =============================================================================

// recoverSettling 恢复**卡住**的 SETTLING 任务（硬崩溃于 commit/release 落账后、终态 CAS 前）。
//
// 仅由 6b fetch reconciler 经 ScanStuckSettling（updated_at 超阈值）筛出的任务调用——阈值已
// 排除「正在被某 worker 结算中」的任务，故这里处理的是进程崩溃残留（钱可能已落账、态卡 SETTLING）。
// **不**走 settleTask 的 SETTLING→no-op 分支（那是给「他人正在结算」的去抖，不做恢复）。
//
// 钱是否已落账无法从 task 行直接判定（CAS terminal→SETTLING 已覆盖原上游终态）→ 按 error_code
// 判原上游终态，分两路恢复（各自幂等）：
//
//   - **失败终态**（error_code 非空）：直接重走 settleReleased。ledger.Release 自身幂等——命中既有
//     release entry 即返成功（settleReleased 视 err==nil → casSettled）；未 release 则全额回退。
//     **不预判**钱是否已落账：ledger 全额 release 的 entry 记在 `correlation+":release"` 下，
//     用 base correlation 的 FindActiveReserveByCorrelation 反查不到（会误判仍 active），故对 release
//     路径不可用此探针；靠 ledger.Release 的 :release 幂等检查即可。
//   - **COMPLETED**（error_code 空）：commit 路径。先用 FindActiveReserveByCorrelation(base) 探针判
//     是否已 commit——commit entry 记在 base correlation 下，已 commit 则反查 ErrNoRows → 直接
//     finalize SETTLED（**避免无谓 Poll**：钱已 commit 时 Poll 失败会误落 settle_failed）；
//     未 commit（active）则重走 settleCompleted（Poll 真实 usage → commit，ErrAlreadySettled 兜底）。
//
// 不变量依赖（与 markUpstreamTerminal / sweep.go 写入约定一致）：**COMPLETED ⟺ error_code 为空**；
// 失败终态（FAILED/CANCELLED/EXPIRED）一律写非空 error_code。
func (s *Service) recoverSettling(ctx context.Context, t db.Task) error {
	snap, err := ParseSnapshot(t.FinancialSnapshot)
	if err != nil {
		s.logger.Error("task.recoverSettling: 快照损坏 → settle_failed",
			slog.String("task_id", t.ID), slog.String("err", err.Error()))
		return s.casSettleFailed(t.ID)
	}

	// 失败终态：release 路径（ledger.Release 自身幂等，重走安全）。
	if t.ErrorCode.Valid && t.ErrorCode.String != "" {
		return s.settleReleased(t, snap)
	}

	// COMPLETED：先探针判是否已 commit（commit entry 在 base correlation 下，反查可靠；
	// 注意此探针对 release 路径不可靠——release 记在 ':release' 下——但 COMPLETED 永不走 release，
	// 故对本分支安全，见上方失败终态分支已分流）。
	_, err = s.q.FindActiveReserveByCorrelation(ctx, db.FindActiveReserveByCorrelationParams{
		BusinessAccountID: t.BusinessAccountID,
		CorrelationID:     snap.ReservationCorrelationID,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// reserve 不再 active：可能已 commit（→ finalize SETTLED），也可能该 correlation 从无 reserve
		// （异常：correlation 错乱 / 快照损坏）。二次反查区分（对齐 ledger.Commit 的 Active/Any 两段判定，
		// 显式优于隐式）：确有 reserve（已 commit）→ SETTLED；无任何 reserve → settle_failed 交对账，
		// **绝不**静默 SETTLED（否则会把「无任何资金动作」的任务标成功，掩盖账务异常）。
		if _, anyErr := s.q.FindAnyReserveByCorrelation(ctx, db.FindAnyReserveByCorrelationParams{
			BusinessAccountID: t.BusinessAccountID,
			CorrelationID:     snap.ReservationCorrelationID,
		}); errors.Is(anyErr, pgx.ErrNoRows) {
			s.logger.Error("task.recoverSettling: COMPLETED 但该 correlation 无任何 reserve（异常）→ settle_failed 交对账",
				slog.String("task_id", t.ID), slog.String("correlation_id", snap.ReservationCorrelationID))
			return s.casSettleFailed(t.ID)
		} else if anyErr != nil && !errors.Is(anyErr, pgx.ErrNoRows) {
			return fmt.Errorf("task.recoverSettling 反查 any reserve: %w", anyErr) // 瞬时 → 下轮重试
		}
		s.logger.Warn("task.recoverSettling: COMPLETED reserve 已 commit、仅缺终态 CAS → finalize SETTLED",
			slog.String("task_id", t.ID))
		return s.casSettled(t.ID)
	case err == nil:
		// reserve 仍 active → 尚未 commit（崩溃在 commit 前）→ 重走 settleCompleted。
		return s.settleCompleted(t, snap)
	default:
		// 反查瞬时失败 → 返 error 交下一轮 reconciler 重试（不强推终态）。
		return fmt.Errorf("task.recoverSettling 反查 active reserve: %w", err)
	}
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
		_, err := s.q.CompareAndSwapTaskStatus(ctx, db.CompareAndSwapTaskStatusParams{
			ToStatus: to, ID: taskID, FromStatus: db.TaskStatusSETTLING,
		})
		if err == nil {
			return nil
		}
		lastErr = err
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

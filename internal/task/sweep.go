package task

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/sunxin-git/api-gateway/internal/db"
	"github.com/sunxin-git/api-gateway/internal/ledger"
	"github.com/sunxin-git/api-gateway/internal/relay/video"
)

// 周期兜底 sweep（plan §Unit 6 workers / ADR-0006）。从 service.go/settle.go 拆出（控制单文件
// ≤~400 行，CLAUDE.md SRP）。各 sweep 经 asynq Scheduler 定时入队（QueueLow）→ workers.go 的
// handler 调用本文件方法；测试直接调本方法（无需 Redis）。
//
// 共性约束：
//   - 所有状态推进只走 markUpstreamTerminal（带 from 条件 CAS + 同事务释放 claim + 入队 settle），
//     并发/重投幂等（CAS 输则放弃）。
//   - 单条处理失败**记录后继续**下一条（不让一条坏数据阻断整轮）；仅扫描查询失败返 error
//     （交 asynq 记录 / 下一 tick 重试）。
//   - 各 sweep 是 idempotent + CAS 守卫，多副本/重复执行安全（MVP 单副本；asynq.Unique 再防重复入队）。

// 各 sweep 写入 task.error_code 的分类码（供审计/告警按码分类）。
//
// **不变量**（recoverSettling 依赖）：COMPLETED ⟺ error_code 为空；所有失败终态
// （FAILED/CANCELLED/EXPIRED）一律写非空 error_code（见下列常量 + pollFailureFields）。
const (
	// errCodeRecoverFailClosed recover 因 submit lease 过期 fail-closed 标 FAILED（不双扣）。
	errCodeRecoverFailClosed = "recover_fail_closed"
	// errCodeExecutionExpired expire 兜底：超最长执行期 → EXPIRED。
	errCodeExecutionExpired = "execution_expired"
	// errCodeUpstreamFailed / Cancelled / Expired fetch reconciler 主动 Poll 命中上游失败终态。
	errCodeUpstreamFailed    = "upstream_failed"
	errCodeUpstreamCancelled = "upstream_cancelled"
	errCodeUpstreamExpired   = "upstream_expired"
)

// =============================================================================
// fetch reconciler（回调缺失 / 入队丢失 / 卡 SETTLING 的轮询兜底）
// =============================================================================

// fetchReconcileOnce 跑一轮 fetch reconciler（scheduled，QueueLow）。各段独立扫描 + 处理：
//
//	① SUBMITTED 滞留（超 submittedNoJobAge，入队丢失 / Redis 抖动无 Asynq job）→ 幂等重投 submit；
//	② missing store（SETTLED/COMPLETED 来源超阈值仍无 oss_object_meta，24h 窗内）→ 幂等重投 store（Unit 9）；
//	③ stuck SETTLING（超 stuckSettlingAge，硬崩溃于结算落账后、终态 CAS 前）→ recoverSettling 幂等恢复；
//	④ stuck UPSTREAM_SUBMITTED（超 stuckUpstreamAge 未终态）→ 主动 Poll 上游 → 命中终态则 markUpstreamTerminal。
//
// **次序刻意**：先跑纯 DB / 仅入队的快段（①②③），最后跑唯一的逐条网络 Poll 段（④，慢，受上游时延
// 支配）——避免上游慢时 Poll 段拖长本轮、饿死动钱相关的 SETTLING 恢复（ce-review 并发 pile-up 缓解）。
// 各段错误互不阻断；用 errors.Join 汇总所有扫描错误供 asynq 记录（下一 tick 自然重试，不丢失任一段失败信号）。
func (s *Service) fetchReconcileOnce(ctx context.Context) error {
	var errs error
	record := func(stage string, err error) {
		if err == nil {
			return
		}
		s.logger.Error("fetch reconciler: 扫描失败（其余段继续）",
			slog.String("stage", stage), slog.String("err", err.Error()))
		errs = errors.Join(errs, err)
	}
	record("submitted_no_job", s.reenqueueSubmittedNoJob(ctx))
	record("missing_store", s.recoverMissingStore(ctx))
	record("stuck_settling", s.recoverStuckSettling(ctx))
	record("stuck_upstream_submitted", s.pollStuckUpstreamSubmitted(ctx))
	return errs
}

// pollStuckUpstreamSubmitted ①：扫 UPSTREAM_SUBMITTED 超时未终态 → 主动 Poll 上游兜底（回调缺失）。
func (s *Service) pollStuckUpstreamSubmitted(ctx context.Context) error {
	rows, err := s.q.ScanStuckUpstreamSubmitted(ctx, db.ScanStuckUpstreamSubmittedParams{
		Threshold: nullTime(time.Now().Add(-s.stuckUpstreamAge)),
		BatchSize: s.sweepBatchSize,
	})
	if err != nil {
		return err
	}
	for _, t := range rows {
		s.pollAndAdvance(ctx, t)
	}
	return nil
}

// pollAndAdvance 对单个卡住的 UPSTREAM_SUBMITTED 任务主动 Poll，命中上游终态则推进 task 终态。
//
// Poll 失败 / 仍 Running → 不动（下一轮重试 / expire 兜底，fail-safe，不提前终态）。
func (s *Service) pollAndAdvance(ctx context.Context, t db.Task) {
	if !t.UpstreamTaskID.Valid || t.UpstreamTaskID.String == "" {
		// UPSTREAM_SUBMITTED 必带 upstream_task_id（原子落点）；缺失属异常，告警留 expire 兜底。
		s.logger.Error("fetch reconciler: UPSTREAM_SUBMITTED 缺 upstream_task_id（异常，待 expire 兜底）",
			slog.String("task_id", t.ID))
		return
	}
	snap, err := ParseSnapshot(t.FinancialSnapshot)
	if err != nil {
		s.logger.Error("fetch reconciler: 快照损坏，无法 Poll（待 expire 兜底）",
			slog.String("task_id", t.ID), slog.String("err", err.Error()))
		return
	}
	entry, ok := s.catalog.Lookup(snap.GatewayModel)
	if !ok || entry == nil {
		s.logger.Error("fetch reconciler: catalog 未命中，无法 Poll（待 expire 兜底）",
			slog.String("task_id", t.ID))
		return
	}
	// 派生自 handler ctx（非 context.Background）：停机时随 ctx 取消，使逐条 Poll 循环及时退出，
	// 不阻塞 graceful shutdown、不在 pool 关闭后再发起 DB 写（ce-review 停机阻塞修复）。Poll 失败/
	// 取消 → 留下一轮重试，安全。
	pollCtx, cancel := context.WithTimeout(ctx, s.pollTO)
	defer cancel()
	creds, err := s.resolveCreds(pollCtx, t.ChannelID)
	if err != nil {
		s.logger.Error("fetch reconciler: 凭据解密失败，无法 Poll（待 expire 兜底）",
			slog.String("task_id", t.ID), slog.String("err", err.Error()))
		return
	}
	res, err := s.adapter.Poll(pollCtx, entry, creds, t.UpstreamTaskID.String)
	if err != nil {
		s.logger.Warn("fetch reconciler: Poll 上游失败（下一轮重试 / expire 兜底）",
			slog.String("task_id", t.ID), slog.String("err", err.Error()))
		return
	}
	terminal, ok := upstreamStatusToTaskTerminal(res.Status)
	if !ok {
		return // 仍 Running（含未知/空），继续等
	}
	errCode, errMsg := pollFailureFields(res)
	won, err := s.markUpstreamTerminal(ctx, t.ID,
		db.TaskStatusUPSTREAMSUBMITTED, terminal, errCode, errMsg, t.BusinessAccountID, t.Model)
	if err != nil {
		s.logger.Error("fetch reconciler: markUpstreamTerminal 失败（下一轮重试）",
			slog.String("task_id", t.ID), slog.String("err", err.Error()))
		return
	}
	if won {
		s.logger.Info("fetch reconciler: 主动 Poll 推进 task 终态",
			slog.String("task_id", t.ID), slog.String("to", string(terminal)))
	}
}

// pollFailureFields 把 Poll 命中的上游终态映射为 task.error_code / error_message。
//
// **Succeeded → COMPLETED 返空 error_code**（维持「COMPLETED ⟺ error_code 空」不变量，
// recoverSettling 据此区分 COMPLETED vs 失败终态）。失败终态写非空分类码 + 上游原因文本。
func pollFailureFields(res *video.PollResult) (errCode, errMsg string) {
	switch res.Status {
	case video.UpstreamFailed:
		return errCodeUpstreamFailed, res.FailureMessage
	case video.UpstreamCancelled:
		return errCodeUpstreamCancelled, res.FailureMessage
	case video.UpstreamExpired:
		return errCodeUpstreamExpired, res.FailureMessage
	default:
		return "", "" // Succeeded → COMPLETED
	}
}

// reenqueueSubmittedNoJob ②：扫 SUBMITTED 滞留超阈值（入队丢失）→ 幂等重投 submit。
//
// 安全性：task 仍 SUBMITTED，handleSubmit 的 CAS（SUBMITTED→UPSTREAM_SUBMITTING）保证与正常
// worker / 并发重投不重复调上游 Submit（确未调上游，故重投安全；区别于 recover 的 fail-closed）。
func (s *Service) reenqueueSubmittedNoJob(ctx context.Context) error {
	rows, err := s.q.ScanSubmittedNoJob(ctx, db.ScanSubmittedNoJobParams{
		Threshold: time.Now().Add(-s.submittedNoJobAge),
		BatchSize: s.sweepBatchSize,
	})
	if err != nil {
		return err
	}
	for _, t := range rows {
		if err := s.enqueuer.EnqueueSubmit(ctx, t.ID); err != nil {
			s.logger.Error("fetch reconciler: 重投 submit 失败（下一轮重试）",
				slog.String("task_id", t.ID), slog.String("err", err.Error()))
			continue
		}
		s.logger.Warn("fetch reconciler: SUBMITTED 滞留（入队丢失）→ 重投 submit",
			slog.String("task_id", t.ID))
	}
	return nil
}

// recoverStuckSettling ③：扫卡住 SETTLING（硬崩溃残留）→ recoverSettling 幂等恢复（见 settle.go）。
func (s *Service) recoverStuckSettling(ctx context.Context) error {
	rows, err := s.q.ScanStuckSettling(ctx, db.ScanStuckSettlingParams{
		Threshold: time.Now().Add(-s.stuckSettlingAge),
		BatchSize: s.sweepBatchSize,
	})
	if err != nil {
		return err
	}
	for _, t := range rows {
		if err := s.recoverSettling(ctx, t); err != nil {
			s.logger.Error("fetch reconciler: 恢复卡住 SETTLING 失败（下一轮重试）",
				slog.String("task_id", t.ID), slog.String("err", err.Error()))
		}
	}
	return nil
}

// =============================================================================
// recover（崩溃恢复 fail-closed）
// =============================================================================

// recoverOnce 跑一轮崩溃恢复（scheduled，QueueLow）：扫 UPSTREAM_SUBMITTING 且 submit lease 过期
// 的任务（崩溃落在「调上游 Submit ↔ 存 upstream_task_id」窗口）。
//
// **fail-closed（ADR-0006 决策 5）**：CAS→FAILED + release + 告警，**绝不自动重投上游**——上游无
// 幂等键、不可按我方标识反查，盲目重投 = 双任务双扣；宁可漏掉一个可能已生成的任务，不可双扣。
// （SUBMITTED 无 job 者由 fetch reconciler 安全重投，因其确未调上游；二者路径不可混。）
func (s *Service) recoverOnce(ctx context.Context) error {
	rows, err := s.q.ScanRecoverableTasks(ctx, db.ScanRecoverableTasksParams{
		Now:       nullTime(time.Now()),
		BatchSize: s.sweepBatchSize,
	})
	if err != nil {
		return err
	}
	for _, t := range rows {
		won, err := s.markUpstreamTerminal(ctx, t.ID,
			db.TaskStatusUPSTREAMSUBMITTING, db.TaskStatusFAILED,
			errCodeRecoverFailClosed,
			"submit lease 过期；fail-closed 不重投（上游可能已生成，宁漏不双扣）",
			t.BusinessAccountID, t.Model)
		if err != nil {
			s.logger.Error("recover: markUpstreamTerminal 失败（下一轮重试）",
				slog.String("task_id", t.ID), slog.String("err", err.Error()))
			continue
		}
		if won {
			// 告警：fail-closed 可能漏掉一个上游已生成的任务，须人工核查（ADR-0006）。
			s.logger.Error("recover: fail-closed 标 FAILED（上游可能已生成但无法判定，已 release 不双扣，需人工核查）",
				slog.String("task_id", t.ID),
				slog.String("upstream_task_id", upstreamTaskIDOrEmpty(t)),
				slog.String("submit_locked_by", t.SubmitLockedBy.String))
		}
	}
	return nil
}

// =============================================================================
// expire（终态收敛兜底）
// =============================================================================

// expireOnce 跑一轮 expire（scheduled，QueueLow）：扫在途三态超最长执行期的任务 → CAS→EXPIRED +
// release。保证所有 task 在有限时间进终态（终态收敛不变量），并释放并发 claim / reserve。
func (s *Service) expireOnce(ctx context.Context) error {
	rows, err := s.q.ScanExpirableTasks(ctx, db.ScanExpirableTasksParams{
		Threshold: time.Now().Add(-s.maxExecutionAge),
		BatchSize: s.sweepBatchSize,
	})
	if err != nil {
		return err
	}
	for _, t := range rows {
		// from = 扫描时的 status（SUBMITTED/UPSTREAM_SUBMITTING/UPSTREAM_SUBMITTED 之一，均允许 →EXPIRED）；
		// 并发已被推进 → CAS 0 行 → won=false → 跳过。
		won, err := s.markUpstreamTerminal(ctx, t.ID,
			t.Status, db.TaskStatusEXPIRED,
			errCodeExecutionExpired, "超最长执行期，expire 兜底收敛",
			t.BusinessAccountID, t.Model)
		if err != nil {
			s.logger.Error("expire: markUpstreamTerminal 失败（下一轮重试）",
				slog.String("task_id", t.ID), slog.String("from", string(t.Status)),
				slog.String("err", err.Error()))
			continue
		}
		if won {
			s.logger.Warn("expire: 超最长执行期 → EXPIRED + release",
				slog.String("task_id", t.ID), slog.String("from", string(t.Status)))
		}
	}
	return nil
}

// =============================================================================
// orphan reserve sweep（reserve 落了但无对应 task → 回退）
// =============================================================================

// orphanReserveSweepOnce 跑一轮孤儿 reserve 回收（scheduled，QueueLow）：扫 ledger active 视频
// reserve 反查无对应 task 行者（reserve 落了但 claim+task tx 没成且补偿 release 也失败）→ Release。
//
// **最小年龄阈值（orphanReserveMinAge）**：只回收确陈旧者，避免误回收 in-flight 窗口内
// 「reserve 已落、claim+task tx 即将提交」的 reserve（否则该 task settle 反查不到 reserve → 资金锁死）。
func (s *Service) orphanReserveSweepOnce(ctx context.Context) error {
	rows, err := s.q.ScanOrphanVideoReserves(ctx, db.ScanOrphanVideoReservesParams{
		MaxCreatedAt: time.Now().Add(-s.orphanReserveMinAge),
		BatchSize:    s.sweepBatchSize,
	})
	if err != nil {
		return err
	}
	for _, r := range rows {
		// release entry 的 reference_id 用 reserve 原始 reference_id（审计血缘一致）；
		// 当前 correlation==reference==task_id，回退仅防 NULL（scan 已用 IS NOT NULL 守卫，理论不触发）。
		referenceID := r.CorrelationID
		if r.ReferenceID.Valid && r.ReferenceID.String != "" {
			referenceID = r.ReferenceID.String
		}
		actor := ledger.Actor{Type: ledger.ActorTypeTask, ID: r.CorrelationID}
		_, relErr := s.ledger.Release(ctx, actor, ledger.ReleaseParams{
			AccountID:     r.BusinessAccountID,
			CorrelationID: r.CorrelationID,
			Amount:        r.Amount,
			ReferenceType: referenceTypeVideoTask,
			ReferenceID:   referenceID,
		})
		switch {
		case relErr == nil:
			s.logger.Warn("orphan-reserve sweep: 回收孤儿 reserve（reserve 落了但无对应 task）",
				slog.String("account_id", r.BusinessAccountID),
				slog.String("correlation_id", r.CorrelationID), slog.Int64("amount", r.Amount))
		case errors.Is(relErr, ledger.ErrAlreadySettled), errors.Is(relErr, ledger.ErrReserveNotFound):
			// 并发已处理（如 task tx 刚提交后被正常 settle / 他轮已回收）→ 幂等跳过。
			s.logger.Info("orphan-reserve sweep: reserve 已被处理，跳过",
				slog.String("account_id", r.BusinessAccountID),
				slog.String("correlation_id", r.CorrelationID))
		default:
			s.logger.Error("orphan-reserve sweep: Release 失败（下一轮重试）",
				slog.String("account_id", r.BusinessAccountID),
				slog.String("correlation_id", r.CorrelationID), slog.String("err", relErr.Error()))
		}
	}
	return nil
}

// upstreamTaskIDOrEmpty 安全取 upstream_task_id（无值返空串；供 recover 告警日志）。
func upstreamTaskIDOrEmpty(t db.Task) string {
	if t.UpstreamTaskID.Valid {
		return t.UpstreamTaskID.String
	}
	return ""
}

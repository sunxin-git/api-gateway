package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/hibiken/asynq"

	"github.com/sunxin-git/api-gateway/internal/asyncq"
	"github.com/sunxin-git/api-gateway/internal/db"
	"github.com/sunxin-git/api-gateway/internal/relay/video"
)

// Asynq task 类型（payload 仅 task_id，worker 从 task 行加载其余——存活于 job 丢失，
// 6b reconciler 可从行重投，无需把请求塞进 payload）。
const (
	TypeSubmit = "video:submit"
	TypeSettle = "video:settle"
)

// taskIDPayload submit/settle job 的 payload（仅 task_id）。
type taskIDPayload struct {
	TaskID string `json:"task_id"`
}

// Enqueuer 入队抽象（DIP；生产用 AsynqEnqueuer，测试可 fake，6a 测试不依赖 Redis）。
type Enqueuer interface {
	EnqueueSubmit(ctx context.Context, taskID string) error
	EnqueueSettle(ctx context.Context, taskID string) error
}

// AsynqEnqueuer 用 asynq.Client 把 submit/settle job 投入对应优先级队列。
//
// 队列映射（asyncq tier，仅调度优先级，非并发上限）：
//   - submit → QueueDefault（常规提交）
//   - settle → QueueCritical（动钱，优先取走）
type AsynqEnqueuer struct {
	client *asyncq.Client
}

// NewAsynqEnqueuer 构造（client 由 main.go 注入；停机序列须 defer client.Close()）。
func NewAsynqEnqueuer(client *asyncq.Client) *AsynqEnqueuer {
	if client == nil {
		panic("task.NewAsynqEnqueuer: client 不能为 nil")
	}
	return &AsynqEnqueuer{client: client}
}

var _ Enqueuer = (*AsynqEnqueuer)(nil)

func (e *AsynqEnqueuer) enqueue(ctx context.Context, typ, taskID, queue string) error {
	payload, err := json.Marshal(taskIDPayload{TaskID: taskID})
	if err != nil {
		return fmt.Errorf("task.enqueue marshal: %w", err)
	}
	// TaskID 作 asynq 唯一键，防同一 task 在队列内重复堆积（重投幂等）。
	_, err = e.client.EnqueueContext(ctx, asynq.NewTask(typ, payload),
		asynq.Queue(queue), asynq.TaskID(typ+":"+taskID))
	// asynq 唯一键冲突（已有同 job 在队列）视为成功（去重）。
	if errors.Is(err, asynq.ErrTaskIDConflict) {
		return nil
	}
	return err
}

func (e *AsynqEnqueuer) EnqueueSubmit(ctx context.Context, taskID string) error {
	return e.enqueue(ctx, TypeSubmit, taskID, asyncq.QueueDefault)
}

func (e *AsynqEnqueuer) EnqueueSettle(ctx context.Context, taskID string) error {
	return e.enqueue(ctx, TypeSettle, taskID, asyncq.QueueCritical)
}

// RegisterHandlers 把 submit/settle handler 注册到 asynq mux（main.go 装配；6b 再加 sweep 周期任务）。
func (s *Service) RegisterHandlers(mux *asynq.ServeMux) {
	mux.HandleFunc(TypeSubmit, s.HandleSubmit)
	mux.HandleFunc(TypeSettle, s.HandleSettle)
}

// HandleSubmit submit job 入口（解 payload → handleSubmit）。
func (s *Service) HandleSubmit(ctx context.Context, t *asynq.Task) error {
	var p taskIDPayload
	if err := json.Unmarshal(t.Payload(), &p); err != nil {
		return fmt.Errorf("submit: 非法 payload: %w", err) // 不可解码 payload → 不重试
	}
	return s.handleSubmit(ctx, p.TaskID)
}

// HandleSettle settle job 入口（解 payload → settleTask）。
func (s *Service) HandleSettle(ctx context.Context, t *asynq.Task) error {
	var p taskIDPayload
	if err := json.Unmarshal(t.Payload(), &p); err != nil {
		return fmt.Errorf("settle: 非法 payload: %w", err)
	}
	return s.settleTask(ctx, p.TaskID)
}

// handleSubmit submit worker 核心：CAS 抢占 → 调上游 Submit → 存 upstream_task_id（plan §Unit 6）。
//
// 双提交防护（ADR-0006 决策 5）：
//   - CAS SUBMITTED→UPSTREAM_SUBMITTING 保证仅一个 worker 提交（并发/重投/重试幂等）。
//   - 上游瞬时错误**不**自动重投上游（上游可能已建任务、无幂等键不可反查）→ 保留
//     UPSTREAM_SUBMITTING + lease，交 recover（6b）fail-closed；不返 err（防 Asynq 重投）。
//   - 上游明确拒绝/畸形 → fail-closed FAILED（上游确未受理）。
func (s *Service) handleSubmit(ctx context.Context, taskID string) error {
	t, err := s.q.GetTaskByID(ctx, taskID)
	if err != nil {
		return fmt.Errorf("submit get: %w", err) // DB 瞬时 → Asynq 重试
	}
	if t.Status != db.TaskStatusSUBMITTED {
		return nil // 幂等：已被处理（重投/重试/recover 推进）
	}

	// CAS SUBMITTED → UPSTREAM_SUBMITTING + 抢占 lease
	lockedUntil := time.Now().Add(s.submitLeaseTTL)
	affected, err := s.q.MarkTaskSubmitting(ctx, db.MarkTaskSubmittingParams{
		SubmitLockedUntil: nullTime(lockedUntil),
		SubmitLockedBy:    pgTextOrNull(s.workerID),
		ID:                taskID,
	})
	if err != nil {
		return fmt.Errorf("submit mark submitting: %w", err)
	}
	if affected == 0 {
		return nil // CAS 输（并发 worker 抢到）
	}

	snap, err := ParseSnapshot(t.FinancialSnapshot)
	if err != nil {
		return s.failSubmit(ctx, t, "snapshot_corrupt", err)
	}
	entry, ok := s.catalog.Lookup(snap.GatewayModel)
	if !ok || entry == nil {
		return s.failSubmit(ctx, t, "catalog_miss", fmt.Errorf("catalog 未命中 %q", snap.GatewayModel))
	}
	creds, err := s.resolveCreds(ctx, t.ChannelID)
	if err != nil {
		return s.failSubmit(ctx, t, "credential_error", err)
	}

	// 调上游 Submit（6a 纯轮询：callbackURL 空；Unit 8 注入带 token 回调 URL）
	upstreamTaskID, err := s.adapter.Submit(ctx, entry, creds, snap.ToValidatedRequest(), "")
	if err != nil {
		return s.handleSubmitError(ctx, t, err)
	}

	// CAS UPSTREAM_SUBMITTING → UPSTREAM_SUBMITTED + 存 upstream_task_id（双提交防护原子落点）
	affected, err = s.q.MarkTaskUpstreamSubmitted(ctx, db.MarkTaskUpstreamSubmittedParams{
		UpstreamTaskID: pgTextOrNull(upstreamTaskID),
		ID:             taskID,
	})
	if err != nil {
		return fmt.Errorf("submit mark upstream submitted: %w", err)
	}
	if affected == 0 {
		// CAS 输（lease 过期被 recover 抢 / 并发）：上游已建任务但本地未落 upstream_task_id。
		// 告警人工核查（不返 err，避免 Asynq 重投再次 Submit → double-submit）。
		s.logger.Error("submit: 上游已提交但本地 CAS UPSTREAM_SUBMITTED 失败（lease 可能过期）；人工核查",
			slog.String("task_id", taskID), slog.String("upstream_task_id", upstreamTaskID))
	}
	return nil
}

// failSubmit 提交前置失败（快照损坏/catalog缺/凭据错）→ fail-closed FAILED（上游确未受理）。
// 返回 nil（FAILED 已终态，settle 会 release；无需 Asynq 重投）。
func (s *Service) failSubmit(ctx context.Context, t db.Task, code string, cause error) error {
	s.logger.Warn("submit: 前置失败 → FAILED",
		slog.String("task_id", t.ID), slog.String("code", code), slog.String("err", cause.Error()))
	if _, err := s.markUpstreamTerminal(ctx, t.ID,
		db.TaskStatusUPSTREAMSUBMITTING, db.TaskStatusFAILED, code, cause.Error(),
		t.BusinessAccountID, t.Model); err != nil {
		// markUpstreamTerminal 自身失败（DB 瞬时）→ 返 err 让 Asynq 重试（CAS 守卫幂等）
		return fmt.Errorf("submit fail terminal: %w", err)
	}
	return nil
}

// handleSubmitError adapter.Submit 错误分流（plan §Unit 5 sentinel + ADR-0006 双提交防护）。
func (s *Service) handleSubmitError(ctx context.Context, t db.Task, err error) error {
	// 非瞬时（参数错/鉴权/畸形）→ 上游确未受理 → fail-closed FAILED
	if errors.Is(err, video.ErrUpstreamRejected) || errors.Is(err, video.ErrUpstreamMalformed) {
		return s.failSubmit(ctx, t, "upstream_rejected", err)
	}
	// 瞬时（Timeout/Server/Unreachable）→ 上游可能已建任务、不可反查（ADR-0006）：
	// **不**重投上游、**不**标 FAILED；保留 UPSTREAM_SUBMITTING + lease 交 recover（6b）fail-closed。
	// 返 nil（不让 Asynq 重投 → 防 double-submit；GetTaskByID 的 SUBMITTED 守卫亦双保险）。
	s.logger.Warn("submit: 瞬时错误，保留 UPSTREAM_SUBMITTING 待 recover fail-closed（不双扣）",
		slog.String("task_id", t.ID), slog.String("err", err.Error()))
	return nil
}

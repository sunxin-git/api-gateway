// Package ledger 的 Reconciler 部分（计划 Unit 7）。
//
// 职责：周期性扫描所有未冻结业务账户，比较 ledger SUM 与 balance 投影，
// 发现偏差（drift）时按 LedgerDriftAction 处理（log dry-run 或 freeze）。
//
// 关键设计点：
//   - 单事务 REPEATABLE READ + READ ONLY 读 SUM + balance，拿一致快照（R14 / 评审 C2）
//   - 首次发现 drift 后等 confirmDelay（默认 1s）二次确认，过滤瞬时不一致（评审 C5）
//   - PG advisory lock 互斥（pass-2 Adv F2-10）：多进程同时启动只有一个 Run，其他直接 return
//   - 默认 dry-run：driftAction="log" 仅 log + bump metric；1-2 周零误报后切 "freeze"
//   - trip-wire 告警：账户数 / 单轮耗时超阈值时 bump overload metric
//   - rebuild stuck watchdog：每轮收尾扫一遍 rebuild_in_progress 超阈值的账户
//   - panic recover：goroutine 内 panic 被捕获 + bump panic metric，下一轮继续跑
package ledger

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sunxin-git/api-gateway/internal/db"
	"github.com/sunxin-git/api-gateway/internal/obs"
)

// reconcilerAdvisoryLockKey PG advisory lock key（"ledger2" ASCII 拼接）。
// 多副本部署时只有一个进程能持锁，其他进程的 Run 直接 return；P0 仍只跑单副本。
const reconcilerAdvisoryLockKey int64 = 0x6C656467657232

// 默认阈值（计划 Unit 7 §默认阈值）。
const (
	defaultReconcilerInterval         = 5 * time.Minute
	defaultReconcilerInitialDelay     = 30 * time.Second
	defaultReconcilerConfirmDelay     = 1 * time.Second
	defaultReconcilerOverloadAccounts = 5000
	defaultReconcilerOverloadDuration = 60 * time.Second
	defaultReconcilerRebuildStuck     = 10 * time.Minute
)

// ReconcilerConfig Reconciler 构造参数集合。
//
// 零值字段会被 NewReconciler 套上默认值（见 const 块）；测试可注入更短的 interval/delay。
// log/metrics 必须非 nil（否则 NewReconciler panic，遵循显式优于隐式原则）。
type ReconcilerConfig struct {
	// Interval 两轮 RunOnce 之间的 ticker 周期；零值 → 5 分钟。
	Interval time.Duration
	// InitialDelay Run 启动后第一次 RunOnce 前的等待时间；零值 → 30s。
	// 测试可注入很小值（如 10ms）加快用例。
	InitialDelay time.Duration
	// ConfirmDelay 首次 drift 检测到不一致后等待二次确认的间隔；零值 → 1s。
	ConfirmDelay time.Duration
	// DriftAction "log"（默认 dry-run）或 "freeze"；空串视为 "log"。
	DriftAction string
	// RebuildStuckThreshold rebuild_in_progress 持续多久视为卡住；零值 → 10 分钟。
	RebuildStuckThreshold time.Duration
	// OverloadAccountsThreshold 单轮扫描账户数超此值即 bump overload；零值 → 5000。
	OverloadAccountsThreshold int
	// OverloadDurationThreshold 单轮耗时超此值即 bump overload；零值 → 60s。
	OverloadDurationThreshold time.Duration
	// AdvisoryLockKey 自定义 advisory lock key（测试用，让并行测试各自隔离）；零值 → 全局默认 0x6C656467657232。
	AdvisoryLockKey int64
	// Log slog logger（必填）。
	Log *slog.Logger
	// Metrics Prometheus 指标容器（必填）。
	Metrics *obs.Metrics
	// PreConfirmHook 仅供测试注入：在第一次检测发现不一致后、Sleep + 第二次检测前调用。
	// 用于模拟「确认窗口期内 Recharge 修正了 drift」的 false-positive 场景。生产代码不设置。
	PreConfirmHook func(ctx context.Context, accountID string)
	// OverrideListAccountsFn 仅供测试注入：替换 ListAllUnfrozenAccountsForReconciler 的返回，
	// 让 trip-wire 测试可以伪造 5001 个账户而不真插库。生产代码不设置。
	OverrideListAccountsFn func(ctx context.Context, actual []db.ListAllUnfrozenAccountsForReconcilerRow) []db.ListAllUnfrozenAccountsForReconcilerRow
	// PanicOnce 仅供测试注入：下一次 RunOnce 入口 panic，验证 recover + metric。生产代码不设置。
	PanicOnce *bool
}

// Reconciler 周期性比对 ledger SUM 与 balance；发现真 drift 时按 DriftAction 处理。
type Reconciler struct {
	service *PostgresService
	pool    *pgxpool.Pool
	queries *db.Queries

	interval              time.Duration
	initialDelay          time.Duration
	confirmDelay          time.Duration
	driftAction           string
	rebuildStuckThreshold time.Duration
	overloadAccountsTh    int
	overloadDurationTh    time.Duration
	advisoryLockKey       int64

	log     *slog.Logger
	metrics *obs.Metrics

	// 测试 hook（生产路径恒为 nil）
	preConfirmHook         func(ctx context.Context, accountID string)
	overrideListAccountsFn func(ctx context.Context, actual []db.ListAllUnfrozenAccountsForReconcilerRow) []db.ListAllUnfrozenAccountsForReconcilerRow
	panicOnce              *bool
}

// NewReconciler 构造 Reconciler。
//
// service / pool 不能为 nil；ReconcilerConfig 中 Log + Metrics 不能为 nil（panic）。
// 零值字段自动用默认常量。
func NewReconciler(service *PostgresService, pool *pgxpool.Pool, cfg ReconcilerConfig) *Reconciler {
	if service == nil {
		panic("ledger.NewReconciler: service 不能为 nil")
	}
	if pool == nil {
		panic("ledger.NewReconciler: pool 不能为 nil")
	}
	if cfg.Log == nil {
		panic("ledger.NewReconciler: cfg.Log 不能为 nil")
	}
	if cfg.Metrics == nil {
		panic("ledger.NewReconciler: cfg.Metrics 不能为 nil")
	}

	r := &Reconciler{
		service:                service,
		pool:                   pool,
		queries:                db.New(pool),
		interval:               cfg.Interval,
		initialDelay:           cfg.InitialDelay,
		confirmDelay:           cfg.ConfirmDelay,
		driftAction:            cfg.DriftAction,
		rebuildStuckThreshold:  cfg.RebuildStuckThreshold,
		overloadAccountsTh:     cfg.OverloadAccountsThreshold,
		overloadDurationTh:     cfg.OverloadDurationThreshold,
		advisoryLockKey:        cfg.AdvisoryLockKey,
		log:                    cfg.Log,
		metrics:                cfg.Metrics,
		preConfirmHook:         cfg.PreConfirmHook,
		overrideListAccountsFn: cfg.OverrideListAccountsFn,
		panicOnce:              cfg.PanicOnce,
	}

	if r.interval <= 0 {
		r.interval = defaultReconcilerInterval
	}
	if r.initialDelay < 0 {
		r.initialDelay = defaultReconcilerInitialDelay
	}
	if cfg.InitialDelay == 0 {
		// 零值用默认（注意：调用方若想立刻跑应传极小值，例如 1*time.Nanosecond，
		// 不能用 0，因 0 被视为「未配置」回落默认 30s）。
		r.initialDelay = defaultReconcilerInitialDelay
	}
	if r.confirmDelay <= 0 {
		r.confirmDelay = defaultReconcilerConfirmDelay
	}
	if r.driftAction == "" {
		r.driftAction = "log"
	}
	if r.rebuildStuckThreshold <= 0 {
		r.rebuildStuckThreshold = defaultReconcilerRebuildStuck
	}
	if r.overloadAccountsTh <= 0 {
		r.overloadAccountsTh = defaultReconcilerOverloadAccounts
	}
	if r.overloadDurationTh <= 0 {
		r.overloadDurationTh = defaultReconcilerOverloadDuration
	}
	if r.advisoryLockKey == 0 {
		r.advisoryLockKey = reconcilerAdvisoryLockKey
	}

	return r
}

// Run 启动 reconciler 主循环。
//
// 行为：
//  1. 抢 advisory lock；失败 → log + bump skipped + return nil（不算错误，让 main 继续提供 HTTP）
//  2. Sleep initialDelay 让 server 先就绪
//  3. ticker 循环跑 RunOnce；ctx.Done() 时优雅退出
//  4. 每次 RunOnce 套 defer recover，捕获 panic + bump panic metric，下一轮继续跑
//
// 返回 nil 表示 ctx 取消或 lock 抢占退出；其他 error 表示初始化阶段（pool.Acquire / lock query）失败。
func (r *Reconciler) Run(ctx context.Context) error {
	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("reconciler 申请专用连接失败: %w", err)
	}
	defer conn.Release()

	var locked bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", r.advisoryLockKey).Scan(&locked); err != nil {
		return fmt.Errorf("reconciler 申请 advisory lock 失败: %w", err)
	}
	if !locked {
		r.log.Warn("reconciler 另一实例已持锁，本进程跳过启动",
			slog.Int64("advisory_lock_key", r.advisoryLockKey))
		r.metrics.ReconcilerSkippedTotal.Inc()
		return nil
	}
	r.log.Info("reconciler advisory lock 获取成功",
		slog.Int64("advisory_lock_key", r.advisoryLockKey),
		slog.Duration("interval", r.interval),
		slog.Duration("initial_delay", r.initialDelay),
		slog.String("drift_action", r.driftAction))
	defer func() {
		// 用 Background 而非 ctx：ctx 可能已取消，但仍需释放锁。
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(releaseCtx, "SELECT pg_advisory_unlock($1)", r.advisoryLockKey); err != nil {
			r.log.Error("reconciler advisory lock 释放失败", slog.String("err", err.Error()))
		}
	}()

	// 启动延迟可被 ctx 中断（避免测试 cancel 后还卡 30s）。
	select {
	case <-ctx.Done():
		return nil
	case <-time.After(r.initialDelay):
	}

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	// 第一轮立即跑，不等 ticker（initialDelay 后就该跑一次；ticker 是后续周期）。
	r.runOnceWithRecover(ctx)

	for {
		select {
		case <-ctx.Done():
			r.log.Info("reconciler 收到 ctx 取消，退出循环")
			return nil
		case <-ticker.C:
			r.runOnceWithRecover(ctx)
		}
	}
}

// runOnceWithRecover 包装 RunOnce 加 panic recover；不向调用方暴露 panic。
func (r *Reconciler) runOnceWithRecover(ctx context.Context) {
	defer func() {
		if rec := recover(); rec != nil {
			r.metrics.ReconcilerPanicTotal.Inc()
			r.log.Error("reconciler RunOnce 内 panic（已 recover）",
				slog.Any("panic", rec))
		}
	}()
	if _, err := r.RunOnce(ctx); err != nil {
		r.log.Error("reconciler RunOnce 返回错误", slog.String("err", err.Error()))
	}
}

// RunResult RunOnce 单轮结果统计；admin-cli drift-check 命令复用。
type RunResult struct {
	// Checked 本轮扫描的账户数（含已确认一致 + 误报 + 真 drift）。
	Checked int
	// Drifted 真 drift（二次确认后仍不一致）的账户数。
	Drifted int
	// FalsePositives 首次不一致但二次确认一致的账户数。
	FalsePositives int
	// StuckRebuilds rebuild_in_progress 超阈值的卡住账户数。
	StuckRebuilds int
	// Duration 本轮端到端耗时。
	Duration time.Duration
}

// RunOnce 跑一次全表 drift 检测。
//
// admin-cli drift-check 直接调本方法，与后台 ticker 共享同一实现（计划 U9 复用）。
// 返回 error 仅来自不可恢复的基础设施异常（如 pool 不可用）；单账户错误内部 log 跳过。
func (r *Reconciler) RunOnce(ctx context.Context) (RunResult, error) {
	// 测试 hook：注入 panic。
	if r.panicOnce != nil && *r.panicOnce {
		*r.panicOnce = false
		panic("reconciler 测试注入 panic")
	}

	start := time.Now()
	result := RunResult{}

	accounts, err := r.queries.ListAllUnfrozenAccountsForReconciler(ctx)
	if err != nil {
		return result, fmt.Errorf("ListAllUnfrozenAccountsForReconciler 失败: %w", err)
	}

	// 测试 hook：替换账户列表。
	if r.overrideListAccountsFn != nil {
		accounts = r.overrideListAccountsFn(ctx, accounts)
	}

	for _, acc := range accounts {
		// ctx 提前取消时立即退出（避免长循环卡到 shutdown 之后）。
		if err := ctx.Err(); err != nil {
			result.Duration = time.Since(start)
			r.metrics.ReconcilerRunDuration.Observe(result.Duration.Seconds())
			return result, nil
		}
		r.checkAccount(ctx, acc.BusinessAccountID, &result)
		result.Checked++
	}

	// 收尾：rebuild stuck watchdog。
	stuck, err := r.queries.ListStuckRebuildAccounts(ctx, intervalFromDuration(r.rebuildStuckThreshold))
	if err != nil {
		r.log.Error("ListStuckRebuildAccounts 失败（不中断本轮）",
			slog.String("err", err.Error()))
	} else {
		for _, s := range stuck {
			r.metrics.LedgerRebuildStuckTotal.WithLabelValues(s.BusinessAccountID).Inc()
			frozenAt := "<nil>"
			if s.FrozenAt.Valid {
				frozenAt = s.FrozenAt.Time.UTC().Format(time.RFC3339)
			}
			r.log.Warn("rebuild stuck 超阈值",
				slog.String("account_id", s.BusinessAccountID),
				slog.String("frozen_at", frozenAt),
				slog.Duration("threshold", r.rebuildStuckThreshold))
		}
		result.StuckRebuilds = len(stuck)
	}

	result.Duration = time.Since(start)
	r.metrics.ReconcilerRunDuration.Observe(result.Duration.Seconds())

	// trip-wire 告警。
	if len(accounts) > r.overloadAccountsTh {
		r.metrics.ReconcilerOverloadTotal.WithLabelValues("accounts").Inc()
		r.log.Warn("reconciler 账户数超阈值（trip-wire）",
			slog.Int("count", len(accounts)),
			slog.Int("threshold", r.overloadAccountsTh))
	}
	if result.Duration > r.overloadDurationTh {
		r.metrics.ReconcilerOverloadTotal.WithLabelValues("duration").Inc()
		r.log.Warn("reconciler 单轮耗时超阈值（trip-wire）",
			slog.Duration("duration", result.Duration),
			slog.Duration("threshold", r.overloadDurationTh))
	}

	r.log.Info("reconciler RunOnce 完成",
		slog.Int("checked", result.Checked),
		slog.Int("drifted", result.Drifted),
		slog.Int("false_positives", result.FalsePositives),
		slog.Int("stuck_rebuilds", result.StuckRebuilds),
		slog.Duration("duration", result.Duration))

	return result, nil
}

// checkAccount 检测单个账户。结果累加到 result。
//
// 流程：
//  1. 第一次 REPEATABLE READ readonly tx 读快照
//  2. 一致 → 直接返回
//  3. 不一致 → 测试 hook（可选）→ Sleep confirmDelay → 第二次读快照
//  4. 第二次一致 → bump false-positive；第二次仍不一致 → bump drift + freeze（或 log）
func (r *Reconciler) checkAccount(ctx context.Context, accountID string, result *RunResult) {
	expected1, actual1, err := r.readSnapshot(ctx, accountID)
	if err != nil {
		r.log.Error("reconciler 第一次读快照失败",
			slog.String("account_id", accountID),
			slog.String("err", err.Error()))
		return
	}
	if isConsistent(expected1, actual1) {
		return
	}

	// 测试 hook：在二次确认前模拟 Recharge 修正 drift（false-positive 场景）。
	if r.preConfirmHook != nil {
		r.preConfirmHook(ctx, accountID)
	}

	// 二次确认：先等 confirmDelay（ctx 可中断）。
	select {
	case <-ctx.Done():
		return
	case <-time.After(r.confirmDelay):
	}

	expected2, actual2, err := r.readSnapshot(ctx, accountID)
	if err != nil {
		r.log.Error("reconciler 第二次读快照失败",
			slog.String("account_id", accountID),
			slog.String("err", err.Error()))
		return
	}
	if isConsistent(expected2, actual2) {
		r.metrics.LedgerDriftFalsePositiveTotal.WithLabelValues(accountID).Inc()
		result.FalsePositives++
		r.log.Info("reconciler drift cleared after confirm window",
			slog.String("account_id", accountID),
			slog.Duration("confirm_delay", r.confirmDelay),
			slog.Any("first_expected", expected1),
			slog.Any("first_actual", actual1))
		return
	}

	// 真 drift。
	result.Drifted++
	r.metrics.LedgerDriftTotal.WithLabelValues(accountID, "drift", r.driftAction).Inc()

	driftLog := r.log.With(
		slog.String("account_id", accountID),
		slog.Int64("expected_available", expected2.Available),
		slog.Int64("actual_available", actual2.Available),
		slog.Int64("expected_reserved", expected2.Reserved),
		slog.Int64("actual_reserved", actual2.Reserved),
		slog.Int64("expected_used_total", expected2.UsedTotal),
		slog.Int64("actual_used_total", actual2.UsedTotal),
		slog.Int64("expected_recharge_total", expected2.RechargeTotal),
		slog.Int64("actual_recharge_total", actual2.RechargeTotal),
		slog.Int64("expected_refund_total", expected2.RefundTotal),
		slog.Int64("actual_refund_total", actual2.RefundTotal),
	)

	switch r.driftAction {
	case "freeze":
		if err := r.service.Freeze(ctx, ActorSystem("reconciler"), accountID, ReasonCodeDriftDetected); err != nil {
			driftLog.Error("reconciler Freeze 失败",
				slog.String("err", err.Error()))
			return
		}
		driftLog.Warn("reconciler 检测真 drift 并冻结账户")
	case "log":
		fallthrough
	default:
		driftLog.Warn("reconciler 检测真 drift（dry-run，仅 log，不冻结）")
	}
}

// expectedBalance 是 ledger SUM 推导的「应有」余额；与 BusinessAccountBalance 字段对齐 5 个数值列。
type expectedBalance struct {
	Available     int64
	Reserved      int64
	UsedTotal     int64
	RechargeTotal int64
	RefundTotal   int64
}

// actualBalance 是从 balance 表读到的「实际」余额；只取 5 个数值列做比较。
type actualBalance struct {
	Available     int64
	Reserved      int64
	UsedTotal     int64
	RechargeTotal int64
	RefundTotal   int64
}

// readSnapshot 在单只读 REPEATABLE READ tx 内同时读 SUM(ledger) + balance，保证一致快照。
//
// REPEATABLE READ 在 tx 起始建立快照；tx 内任意 SELECT 看到的都是同一快照，
// 避免 READ COMMITTED 双 SELECT 之间被其他事务 Commit 改写 balance 导致误报（评审 C2）。
//
// 不持有 FOR UPDATE 行锁（reconciler 是只读旁路，不能阻塞业务 CAS）。
func (r *Reconciler) readSnapshot(ctx context.Context, accountID string) (expectedBalance, actualBalance, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return expectedBalance{}, actualBalance{}, fmt.Errorf("BeginTx(RepeatableRead, ReadOnly) 失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := r.queries.WithTx(tx)

	sumRow, err := q.SumLedgerDeltasByAccount(ctx, accountID)
	if err != nil {
		return expectedBalance{}, actualBalance{}, fmt.Errorf("SumLedgerDeltasByAccount 失败: %w", err)
	}
	balRow, err := q.GetBalanceInTx(ctx, accountID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return expectedBalance{}, actualBalance{}, ErrAccountNotFound
		}
		return expectedBalance{}, actualBalance{}, fmt.Errorf("GetBalanceInTx 失败: %w", err)
	}

	exp := expectedBalance{
		Available:     sumRow.AvailableSum,
		Reserved:      sumRow.ReservedSum,
		UsedTotal:     sumRow.UsedSum,
		RechargeTotal: sumRow.RechargeSum,
		RefundTotal:   sumRow.RefundSum,
	}
	act := actualBalance{
		Available:     balRow.Available,
		Reserved:      balRow.Reserved,
		UsedTotal:     balRow.UsedTotal,
		RechargeTotal: balRow.RechargeTotal,
		RefundTotal:   balRow.RefundTotal,
	}
	return exp, act, nil
}

// isConsistent 比较 expected vs actual 5 字段是否完全相等。
//
// 不做近似比较（账本是离散整数 minor unit；任何 1 分差异都是 drift）。
func isConsistent(exp expectedBalance, act actualBalance) bool {
	return exp.Available == act.Available &&
		exp.Reserved == act.Reserved &&
		exp.UsedTotal == act.UsedTotal &&
		exp.RechargeTotal == act.RechargeTotal &&
		exp.RefundTotal == act.RefundTotal
}

// intervalFromDuration 把 Go time.Duration 转成 pgtype.Interval（仅用微秒精度，月日均为 0）。
//
// PG interval 内部用 months/days/microseconds 三段表示；reconciler 阈值最长 10 分钟，
// 只填 Microseconds 即可（避免与 PG 的「30 天 = 1 个月」近似冲突）。
func intervalFromDuration(d time.Duration) pgtype.Interval {
	return pgtype.Interval{
		Microseconds: d.Microseconds(),
		Days:         0,
		Months:       0,
		Valid:        true,
	}
}

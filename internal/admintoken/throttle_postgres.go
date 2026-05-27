package admintoken

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sunxin-git/api-gateway/internal/db"
)

// PostgresThrottle Throttle 接口的 Postgres + 内存复合实现（计划 Unit 3）。
//
// 各阀门后端：
//   - daily counter（recharge / refund / create）→ gateway_admin_token_usage 表（ON CONFLICT UPSERT 原子累加）
//   - circuit breaker（error_count / tripped_until）→ gateway_admin_token_circuit 表
//   - RPM → 注入 *InProcessRPM（决策 D6；P0 单实例够用）
//   - single_recharge / single_refund → 纯内存 token 字段比对，不查 DB
//
// 设计原则（CLAUDE.md §四）：
//   - 显式优于隐式：Check 与 Record 严格分离；Record 仅在 LedgerService outcome=FreshlyWritten 调
//   - 失败优先：limit nil / pointer 解引用前必校验
//   - DIP：上层 middleware 只依赖 Throttle interface
type PostgresThrottle struct {
	pool    *pgxpool.Pool
	queries *db.Queries
	rpm     *InProcessRPM
	log     *slog.Logger
}

// 编译期断言实现接口。
var _ Throttle = (*PostgresThrottle)(nil)

// NewPostgresThrottle 构造 throttle 实例。
//
// 入参：
//   - pool：pgxpool.Pool（与 LedgerService / admintoken.Service 共用）
//   - rpm：InProcessRPM 实例（main.go 与本 throttle 同生命周期；Close 由 main.go 负责）
//   - log：slog logger（不能 nil）
func NewPostgresThrottle(pool *pgxpool.Pool, rpm *InProcessRPM, log *slog.Logger) *PostgresThrottle {
	if pool == nil {
		panic("admintoken.NewPostgresThrottle: pool 不能为 nil")
	}
	if rpm == nil {
		panic("admintoken.NewPostgresThrottle: rpm 不能为 nil")
	}
	if log == nil {
		panic("admintoken.NewPostgresThrottle: log 不能为 nil")
	}
	return &PostgresThrottle{
		pool:    pool,
		queries: db.New(pool),
		rpm:     rpm,
		log:     log,
	}
}

// =============================================================================
// Single-* 纯内存预检
// =============================================================================

func (t *PostgresThrottle) CheckSingleRecharge(token *Token, amount int64) error {
	if token == nil || token.SingleRechargeMax == nil {
		return nil
	}
	if amount > *token.SingleRechargeMax {
		return ErrSingleRechargeExceeded
	}
	return nil
}

func (t *PostgresThrottle) CheckSingleRefund(token *Token, amount int64) error {
	if token == nil || token.SingleRefundMax == nil {
		return nil
	}
	if amount > *token.SingleRefundMax {
		return ErrSingleRefundExceeded
	}
	return nil
}

// =============================================================================
// Daily 预检（只读 SELECT；不副作用）
// =============================================================================

func (t *PostgresThrottle) CheckDailyRecharge(ctx context.Context, token *Token, amount int64) error {
	if token == nil || token.DailyRechargeQuotaLimit == nil {
		return nil
	}
	cur, err := t.currentRechargeTotal(ctx, token.ID)
	if err != nil {
		return err
	}
	if cur+amount > *token.DailyRechargeQuotaLimit {
		return ErrDailyRechargeExceeded
	}
	return nil
}

func (t *PostgresThrottle) CheckDailyRefund(ctx context.Context, token *Token, amount int64) error {
	if token == nil || token.DailyRefundQuotaLimit == nil {
		return nil
	}
	cur, err := t.currentRefundTotal(ctx, token.ID)
	if err != nil {
		return err
	}
	if cur+amount > *token.DailyRefundQuotaLimit {
		return ErrDailyRefundExceeded
	}
	return nil
}

func (t *PostgresThrottle) CheckDailyCreate(ctx context.Context, token *Token) error {
	if token == nil || token.DailyAccountCreateLimit == nil {
		return nil
	}
	cur, err := t.currentCreateCount(ctx, token.ID)
	if err != nil {
		return err
	}
	if cur+1 > *token.DailyAccountCreateLimit {
		return ErrDailyCreateExceeded
	}
	return nil
}

// =============================================================================
// RPM（委托内存实现）
// =============================================================================

func (t *PostgresThrottle) CheckRPM(token *Token) error {
	return t.rpm.Check(token)
}

// =============================================================================
// Circuit breaker
// =============================================================================

func (t *PostgresThrottle) CheckCircuitBreaker(ctx context.Context, token *Token) error {
	if token == nil || !token.CircuitBreakerEnabled {
		return nil
	}
	state, err := t.queries.GetCircuitState(ctx, token.ID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// 首次失败前不存在 row；视作未熔断
			return nil
		}
		return fmt.Errorf("GetCircuitState 失败: %w", err)
	}
	if state.IsTripped.Valid && state.IsTripped.Bool {
		return ErrCircuitOpen
	}
	return nil
}

// =============================================================================
// Record 路径
// =============================================================================

func (t *PostgresThrottle) RecordSuccessfulRecharge(ctx context.Context, tokenID int64, amount int64) error {
	_, err := t.queries.IncrementTokenUsage(ctx, db.IncrementTokenUsageParams{
		TokenID:            tokenID,
		RechargeDelta:      amount,
		RefundDelta:        0,
		AccountCreateDelta: 0,
	})
	if err != nil {
		return fmt.Errorf("IncrementTokenUsage(recharge) 失败: %w", err)
	}
	return nil
}

func (t *PostgresThrottle) RecordSuccessfulRefund(ctx context.Context, tokenID int64, amount int64) error {
	_, err := t.queries.IncrementTokenUsage(ctx, db.IncrementTokenUsageParams{
		TokenID:            tokenID,
		RechargeDelta:      0,
		RefundDelta:        amount,
		AccountCreateDelta: 0,
	})
	if err != nil {
		return fmt.Errorf("IncrementTokenUsage(refund) 失败: %w", err)
	}
	return nil
}

func (t *PostgresThrottle) RecordSuccessfulCreate(ctx context.Context, tokenID int64) error {
	_, err := t.queries.IncrementTokenUsage(ctx, db.IncrementTokenUsageParams{
		TokenID:            tokenID,
		RechargeDelta:      0,
		RefundDelta:        0,
		AccountCreateDelta: 1,
	})
	if err != nil {
		return fmt.Errorf("IncrementTokenUsage(create) 失败: %w", err)
	}
	return nil
}

// RecordHandlerError 由 audit middleware 在 defer 中调（中间件链 status >= 400 路径）。
//
// 流程：
//
//  1. token.CircuitBreakerEnabled = false → 直接 return（避免无用 DB 写）
//  2. RecordCircuitError UPSERT 累加 error_count（1h 滚动窗口自动重置）
//  3. 新 error_count ≥ circuitErrorThreshold（100）→ TripCircuitBreaker 写 breaker_tripped_until = NOW() + 1h
//
// 已跳闸 token 重复调用：TripCircuitBreaker 用 GREATEST 不会缩短跳闸期。
func (t *PostgresThrottle) RecordHandlerError(ctx context.Context, token *Token) error {
	if token == nil || !token.CircuitBreakerEnabled {
		return nil
	}

	row, err := t.queries.RecordCircuitError(ctx, token.ID)
	if err != nil {
		return fmt.Errorf("RecordCircuitError 失败: %w", err)
	}

	if row.ErrorCount >= circuitErrorThreshold {
		// 跳闸（幂等：已跳闸时 GREATEST 不会缩短）
		_, err := t.queries.TripCircuitBreaker(ctx, token.ID)
		if err != nil {
			return fmt.Errorf("TripCircuitBreaker 失败: %w", err)
		}
		t.log.Warn("circuit breaker tripped for admin token",
			slog.Int64("token_id", token.ID),
			slog.Int("error_count", int(row.ErrorCount)),
		)
	}
	return nil
}

// =============================================================================
// Snapshot 只读快照（whoami / 运维用）
// =============================================================================

func (t *PostgresThrottle) GetUsageToday(ctx context.Context, tokenID int64) (UsageSnapshot, error) {
	usage, err := t.queries.GetTokenUsage(ctx, tokenID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return UsageSnapshot{}, nil
		}
		return UsageSnapshot{}, fmt.Errorf("GetTokenUsage 失败: %w", err)
	}
	return UsageSnapshot{
		RechargeTotalMinor: usage.RechargeTotalMinor,
		RefundTotalMinor:   usage.RefundTotalMinor,
		AccountCreateCount: usage.AccountCreateCount,
	}, nil
}

func (t *PostgresThrottle) GetCircuitSnapshot(ctx context.Context, tokenID int64) (CircuitSnapshot, error) {
	state, err := t.queries.GetCircuitState(ctx, tokenID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return CircuitSnapshot{}, nil
		}
		return CircuitSnapshot{}, fmt.Errorf("GetCircuitState 失败: %w", err)
	}
	snap := CircuitSnapshot{
		ErrorCount:      state.ErrorCount,
		WindowStartedAt: state.WindowStartedAt,
	}
	if state.IsTripped.Valid && state.IsTripped.Bool {
		snap.Open = true
	}
	if state.BreakerTrippedUntil.Valid {
		// 仅在仍处于未来时点报告 TrippedUntil，避免误导（已过期视作 nil）
		until := state.BreakerTrippedUntil.Time
		if until.After(time.Now()) {
			snap.TrippedUntil = &until
		}
	}
	return snap, nil
}

// =============================================================================
// 内部 helpers（GetTokenUsage no-rows → 0；统一封装）
// =============================================================================

func (t *PostgresThrottle) currentRechargeTotal(ctx context.Context, tokenID int64) (int64, error) {
	usage, err := t.queries.GetTokenUsage(ctx, tokenID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("GetTokenUsage 失败: %w", err)
	}
	return usage.RechargeTotalMinor, nil
}

func (t *PostgresThrottle) currentRefundTotal(ctx context.Context, tokenID int64) (int64, error) {
	usage, err := t.queries.GetTokenUsage(ctx, tokenID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("GetTokenUsage 失败: %w", err)
	}
	return usage.RefundTotalMinor, nil
}

func (t *PostgresThrottle) currentCreateCount(ctx context.Context, tokenID int64) (int32, error) {
	usage, err := t.queries.GetTokenUsage(ctx, tokenID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("GetTokenUsage 失败: %w", err)
	}
	return usage.AccountCreateCount, nil
}

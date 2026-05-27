package admintoken

import (
	"context"
	"errors"
	"time"
)

// UsageSnapshot 当日累计用量只读快照（whoami / 运维查询用）。
type UsageSnapshot struct {
	// RechargeTotalMinor 当日成功充值总额（minor unit, UTC day）。
	RechargeTotalMinor int64
	// RefundTotalMinor 当日成功退款总额（minor unit, UTC day）。
	RefundTotalMinor int64
	// AccountCreateCount 当日成功创建账户数。
	AccountCreateCount int32
}

// CircuitSnapshot 熔断器只读快照（whoami / 运维查询用）。
type CircuitSnapshot struct {
	// Open 是否当前跳闸（breaker_tripped_until > NOW()）。
	Open bool
	// ErrorCount 当前窗口内累计错误数。
	ErrorCount int32
	// TrippedUntil 跳闸截止时间；nil 表示未跳闸或已恢复。
	TrippedUntil *time.Time
	// WindowStartedAt 当前窗口起点；用于运维评估"还差多久窗口重置"。
	WindowStartedAt time.Time
}

// Throttle Admin Token 阀门 / 限流 / 熔断接口（计划 Unit 3 / R5）。
//
// 设计原则（CLAUDE.md §四）：
//   - SRP：每个 Check / Record 方法只做一件事
//   - 两步式（Check + Record）：成功后再累加，避免空计数（决策 D2 关键语义）
//   - DIP：middleware 持有 Throttle interface，便于测试 mock + P1 替换 Redis 实现
//
// 实现：
//   - daily / circuit（PG）→ PostgresThrottle
//   - RPM（内存）→ InProcessRPM；通常被 composite 实现包装；可独立单测
//   - single_recharge / single_refund（纯内存 token 字段比对）→ Postgres / RPM 都实装一份相同逻辑也可，
//     当前实装放 PostgresThrottle（避免业务方多接一个 interface 入参）
//
// 两步式语义（决策 D2 + D11）：
//   - Check* 方法：纯只读预检；通过不副作用，失败返 sentinel
//   - Record* 方法：仅当 LedgerService outcome=FreshlyWritten 时调；幂等命中不调
//   - 中间 LedgerService 调用失败 → 已通过的 Check 不需要回滚（语义为"今日成功充值总额上限"，失败不计数）
type Throttle interface {
	// CheckSingleRecharge 单笔充值预检：amount > token.SingleRechargeMax → ErrSingleRechargeExceeded。
	// token.SingleRechargeMax = nil → 无上限，永远通过。
	CheckSingleRecharge(token *Token, amount int64) error

	// CheckSingleRefund 单笔退款预检（document-review 新增；缓解 leaked refund-scope token）。
	// token.SingleRefundMax = nil → 无上限。
	CheckSingleRefund(token *Token, amount int64) error

	// CheckDailyRecharge 当日累计充值预检：current + amount > limit → ErrDailyRechargeExceeded。
	// token.DailyRechargeQuotaLimit = nil → 无上限。
	// 失败时**不**累加；成功时**不**累加；累加由 RecordSuccessfulRecharge 单独完成。
	CheckDailyRecharge(ctx context.Context, token *Token, amount int64) error

	// CheckDailyRefund 当日累计退款预检（document-review 新增）。
	CheckDailyRefund(ctx context.Context, token *Token, amount int64) error

	// CheckDailyCreate 当日累计创建账户数预检。
	// token.DailyAccountCreateLimit = nil → 无上限。
	CheckDailyCreate(ctx context.Context, token *Token) error

	// CheckRPM 滚动 60s 内调用数预检：count > token.RequestsPerMinute → ErrRPMExceeded。
	// token.RequestsPerMinute = nil → 无上限。
	// **副作用**：通过时把当前时间戳追加到 ring；失败时**不**追加（业界惯例：被拒请求不计入下次限速基数）。
	CheckRPM(token *Token) error

	// CheckCircuitBreaker 查询熔断器状态；tripped_until > NOW() → ErrCircuitOpen。
	// 不存在 token_id 记录 = 未熔断（首次失败前不存在）。
	// token.CircuitBreakerEnabled = false → 直接通过（即使 DB 有 row 也忽略）。
	CheckCircuitBreaker(ctx context.Context, token *Token) error

	// RecordSuccessfulRecharge LedgerService.Recharge 返 FreshlyWritten 后调；累加当日 recharge_total_minor。
	RecordSuccessfulRecharge(ctx context.Context, tokenID int64, amount int64) error

	// RecordSuccessfulRefund LedgerService.Refund 返 FreshlyWritten 后调；累加当日 refund_total_minor。
	RecordSuccessfulRefund(ctx context.Context, tokenID int64, amount int64) error

	// RecordSuccessfulCreate LedgerService.CreateAccount 返成功后调；累加当日 account_create_count。
	RecordSuccessfulCreate(ctx context.Context, tokenID int64) error

	// RecordHandlerError 4xx/5xx 后由 middleware 在 defer 中调；累计 error_count；
	// 触发 ≥ 100 / 1h 时跳闸（写 breaker_tripped_until）。
	// token.CircuitBreakerEnabled = false 时**不**计数（避免无用 DB 写）。
	RecordHandlerError(ctx context.Context, token *Token) error

	// GetUsageToday 读取指定 token 当日累计用量；whoami / 运维查询用。
	// 不存在记录（当日尚无成功操作）→ 全零 UsageSnapshot + nil。
	GetUsageToday(ctx context.Context, tokenID int64) (UsageSnapshot, error)

	// GetCircuitSnapshot 读取熔断器状态；whoami / 运维查询用。
	// 不存在记录（首次失败前）→ Open=false + ErrorCount=0 + nil。
	GetCircuitSnapshot(ctx context.Context, tokenID int64) (CircuitSnapshot, error)
}

// =============================================================================
// Sentinel errors（Throttle 路径专属；middleware MapError 时 errors.Is 匹配）
// =============================================================================

var (
	// ErrSingleRechargeExceeded 单笔充值超阀门（HTTP 429）。
	ErrSingleRechargeExceeded = errors.New("single recharge amount exceeds limit")

	// ErrDailyRechargeExceeded 当日累计充值超阀门（HTTP 429）。
	ErrDailyRechargeExceeded = errors.New("daily recharge quota exceeded")

	// ErrSingleRefundExceeded 单笔退款超阀门（HTTP 429）。
	ErrSingleRefundExceeded = errors.New("single refund amount exceeds limit")

	// ErrDailyRefundExceeded 当日累计退款超阀门（HTTP 429）。
	ErrDailyRefundExceeded = errors.New("daily refund quota exceeded")

	// ErrDailyCreateExceeded 当日创建账户数超阀门（HTTP 429）。
	ErrDailyCreateExceeded = errors.New("daily account create limit exceeded")

	// ErrRPMExceeded RPM 限速触发（HTTP 429）。
	ErrRPMExceeded = errors.New("requests per minute exceeded")

	// ErrCircuitOpen 熔断器跳闸中（HTTP 429）。
	ErrCircuitOpen = errors.New("circuit breaker open")
)

// =============================================================================
// 配置 / 调优常量（计划 Unit 3 决策）
// =============================================================================

// circuitErrorThreshold 熔断器跳闸阈值：当 1h 滚动窗口内 error_count 累加到 100 → 跳闸 1h（决策 D2）。
// 当前不暴露为配置（P0 一刀切；P1 看实际告警再调）。
const circuitErrorThreshold = 100

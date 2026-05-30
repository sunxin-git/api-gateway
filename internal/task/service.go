package task

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sunxin-git/api-gateway/internal/channel"
	"github.com/sunxin-git/api-gateway/internal/db"
	"github.com/sunxin-git/api-gateway/internal/ledger"
	"github.com/sunxin-git/api-gateway/internal/relay/video"
	"github.com/sunxin-git/api-gateway/internal/storage"
)

// referenceTypeVideoTask ledger reserve/commit/release 的 reference_type（运维按此 SUM 对账）。
const referenceTypeVideoTask = "video_task"

// ErrConcurrencyLimit 提交时账户×模型并发 claim 占不到位（R15 上限）；handler 映射 429。
var ErrConcurrencyLimit = errors.New("task: 账户×模型并发已达上限")

// CredentialResolver 解密上游凭据的最小依赖（channel.Service 满足；测试可 fake）。
type CredentialResolver interface {
	GetCredentialsForUpstream(ctx context.Context, id int64) (*channel.ChannelCredentials, error)
}

// Service 异步视频任务服务：提交流程 + 状态机 CAS 封装 + settle 编排（plan §Unit 6）。
//
// 持久依赖经构造注入（DIP）；不持隐式全局 state。所有状态变更走带 from 条件的 CAS。
type Service struct {
	pool     *pgxpool.Pool
	q        *db.Queries
	ledger   ledger.Service
	adapter  video.AsyncProviderAdapter
	catalog  video.VideoCatalog
	creds    CredentialResolver
	enqueuer Enqueuer
	logger   *slog.Logger

	// cap R15 静态默认并发上限（limits==nil 时回退；6a 兼容）。
	cap int32
	// limits R15 并发上限值解析器（默认 + per-(account,model) 覆写，Unit 8）；nil 回退静态 cap。
	//   实际并发硬上限由 DB 原子 claim 实施（ClaimConcurrencySlot 以本值为 cap），非 Asynq 队列。
	limits *video.ConcurrencyLimits
	// callbackBaseURL 回调入口 base URL（如 https://gw.example.com）；空 = 不注册回调、纯轮询兜底（6a）。
	//   submit worker 据此 + per-task token 构造交给上游的回调 URL（含 token 的 URL 绝不入日志）。
	callbackBaseURL string

	// --- Unit 9 结果转存 ---
	// objectStoreFactory 由 per-channel TOS 凭据构造结果对象存储；nil → 结果转存禁用（store job no-op）。
	objectStoreFactory func(storage.TOSConfig) (storage.ObjectStore, error)
	// resultHTTPClient 拉取上游产物的 HTTP 客户端（带超时）。
	resultHTTPClient *http.Client
	// maxResultBytes 产物最大字节（防 OOM；超限转人工对账）。
	maxResultBytes int64
	// allowPrivateResultHost 仅测试置 true（httptest 走 127.0.0.1）；生产 false → 产物 URL SSRF 私网拒绝。
	allowPrivateResultHost bool
	// submitLeaseTTL submit worker 抢占 lease 时长（recover 据此判过期，6b）。
	submitLeaseTTL time.Duration
	// settleTO settle 的 ledger commit/release 独立 ctx 超时。
	settleTO time.Duration
	// pollTO settle 内 Poll 反查上游 usage 的独立超时。
	pollTO time.Duration
	// workerID 标识本进程（写入 submit_locked_by，便于排查）。
	workerID string

	// --- 6b sweep 阈值/批量（fetch reconciler / recover / expire / orphan 周期兜底）---
	// submittedNoJobAge SUBMITTED 滞留多久判「入队丢失」→ 幂等重投 submit。
	submittedNoJobAge time.Duration
	// stuckUpstreamAge UPSTREAM_SUBMITTED 多久未终态 → 主动 Poll 上游兜底（回调缺失）。
	stuckUpstreamAge time.Duration
	// stuckSettlingAge SETTLING 多久未终态 → 崩溃恢复（硬崩溃于结算落账后、终态 CAS 前）。
	//   须 ≫ 正常 settle 端到端耗时，避免抢「正在结算中」的任务。
	stuckSettlingAge time.Duration
	// maxExecutionAge 任务超最长执行期 → EXPIRED 兜底（seedance execution_expires_after 默认 48h）。
	maxExecutionAge time.Duration
	// orphanReserveMinAge 孤儿 reserve 最小年龄阈值：只回收确陈旧者，防误回收 in-flight 窗口内
	//   reserve 已落、task tx 即将提交的 reserve（须 ≫ reserve→task tx 正常间隔）。
	orphanReserveMinAge time.Duration
	// storeNeedingStoreAge SETTLED（COMPLETED 来源）多久仍无 oss_object_meta 判「store job 丢失」→ 恢复重投（Unit 9）。
	storeNeedingStoreAge time.Duration
	// sweepBatchSize 各 sweep 单轮扫描批量上限（防一轮处理过多阻塞）。
	sweepBatchSize int32
}

// Config 构造 Service 的参数。
type Config struct {
	Pool     *pgxpool.Pool
	Ledger   ledger.Service
	Adapter  video.AsyncProviderAdapter
	Catalog  video.VideoCatalog
	Creds    CredentialResolver
	Enqueuer Enqueuer
	Logger   *slog.Logger

	ConcurrencyCap int32
	// Limits R15 并发上限值解析器（默认 + 覆写，Unit 8）；nil → 回退静态 ConcurrencyCap。
	Limits *video.ConcurrencyLimits
	// CallbackBaseURL 回调入口 base URL；空 → 纯轮询兜底（不注册回调，6a）。
	CallbackBaseURL string

	// ObjectStoreFactory 由 per-channel TOS 凭据构造结果对象存储（Unit 9）；nil → 结果转存禁用。
	ObjectStoreFactory func(storage.TOSConfig) (storage.ObjectStore, error)
	// ResultHTTPClient 拉取上游产物的 HTTP 客户端（Unit 9）；nil → 默认带超时。
	ResultHTTPClient *http.Client
	// MaxResultBytes 产物最大字节（Unit 9）；<=0 回落默认。
	MaxResultBytes int64
	// AllowPrivateResultHost 仅测试置 true（放行 httptest 环回）；生产留 false（SSRF 私网拒绝）。
	AllowPrivateResultHost bool

	SubmitLeaseTTL time.Duration
	SettleTimeout  time.Duration
	PollTimeout    time.Duration
	WorkerID       string

	// 6b sweep 阈值/批量（零值回落默认；测试可注入小值）。
	SubmittedNoJobAge    time.Duration
	StuckUpstreamAge     time.Duration
	StuckSettlingAge     time.Duration
	MaxExecutionAge      time.Duration
	OrphanReserveMinAge  time.Duration
	StoreNeedingStoreAge time.Duration
	SweepBatchSize       int32
}

// 默认时长（Config 未给时回落）。
const (
	defaultSubmitLeaseTTL = 2 * time.Minute
	defaultSettleTimeout  = 5 * time.Second
	defaultPollTimeout    = 10 * time.Second
	defaultConcurrencyCap = 5

	// 6b sweep 默认阈值/批量。
	defaultSubmittedNoJobAge = 2 * time.Minute
	defaultStuckUpstreamAge  = 2 * time.Minute
	// stuckSettlingAge 须安全 ≫ 单次 settle 最坏端到端耗时（pollTO 10s + commit/release 重试
	// settleTO 5s×退避 + finalizeSettle 重试），留足余量避免抢「正在结算中」的任务（ce-review：
	// 2m 余量偏窄，提至 5m）。代价仅：硬崩溃残留的 SETTLING 恢复延迟 5min（罕见，可接受）。
	defaultStuckSettlingAge    = 5 * time.Minute
	defaultMaxExecutionAge     = 48 * time.Hour // seedance execution_expires_after 默认 48h
	defaultOrphanReserveMinAge = 15 * time.Minute
	defaultSweepBatchSize      = 100

	// Unit 9 结果转存默认值。
	defaultMaxResultBytes     = 512 << 20       // 512 MiB 产物上限（防 OOM；text_to_video 实际远小）
	defaultResultFetchTimeout = 5 * time.Minute // 拉取大产物总超时
	// storeNeedingStoreAge SETTLED 后多久仍无 oss_object_meta 判「store 丢失」→ 恢复重投（6b）。
	defaultStoreNeedingStoreAge = 3 * time.Minute
	// resultURLValidWindow 上游产物 URL 有效窗口（ADR-0006：seedance video_url 仅 24h）；超窗不再重投转存。
	resultURLValidWindow = 24 * time.Hour
)

// NewService 构造 Service + fail-fast 必填校验。
func NewService(cfg Config) (*Service, error) {
	if cfg.Pool == nil {
		return nil, errors.New("task.NewService: Pool 不能为 nil")
	}
	if cfg.Ledger == nil || cfg.Adapter == nil || cfg.Catalog == nil || cfg.Creds == nil || cfg.Enqueuer == nil {
		return nil, errors.New("task.NewService: Ledger/Adapter/Catalog/Creds/Enqueuer 均不能为 nil")
	}
	if cfg.Logger == nil {
		return nil, errors.New("task.NewService: Logger 不能为 nil")
	}
	s := &Service{
		pool:                   cfg.Pool,
		q:                      db.New(cfg.Pool),
		ledger:                 cfg.Ledger,
		adapter:                cfg.Adapter,
		catalog:                cfg.Catalog,
		creds:                  cfg.Creds,
		enqueuer:               cfg.Enqueuer,
		logger:                 cfg.Logger,
		cap:                    cfg.ConcurrencyCap,
		limits:                 cfg.Limits,
		callbackBaseURL:        cfg.CallbackBaseURL,
		objectStoreFactory:     cfg.ObjectStoreFactory,
		resultHTTPClient:       cfg.ResultHTTPClient,
		maxResultBytes:         cfg.MaxResultBytes,
		allowPrivateResultHost: cfg.AllowPrivateResultHost,
		submitLeaseTTL:         cfg.SubmitLeaseTTL,
		settleTO:               cfg.SettleTimeout,
		pollTO:                 cfg.PollTimeout,
		workerID:               cfg.WorkerID,

		submittedNoJobAge:    cfg.SubmittedNoJobAge,
		stuckUpstreamAge:     cfg.StuckUpstreamAge,
		stuckSettlingAge:     cfg.StuckSettlingAge,
		maxExecutionAge:      cfg.MaxExecutionAge,
		orphanReserveMinAge:  cfg.OrphanReserveMinAge,
		storeNeedingStoreAge: cfg.StoreNeedingStoreAge,
		sweepBatchSize:       cfg.SweepBatchSize,
	}
	if s.cap <= 0 {
		s.cap = defaultConcurrencyCap
	}
	if s.submitLeaseTTL <= 0 {
		s.submitLeaseTTL = defaultSubmitLeaseTTL
	}
	if s.settleTO <= 0 {
		s.settleTO = defaultSettleTimeout
	}
	if s.pollTO <= 0 {
		s.pollTO = defaultPollTimeout
	}
	if s.workerID == "" {
		s.workerID = "task-worker"
	}
	if s.submittedNoJobAge <= 0 {
		s.submittedNoJobAge = defaultSubmittedNoJobAge
	}
	if s.stuckUpstreamAge <= 0 {
		s.stuckUpstreamAge = defaultStuckUpstreamAge
	}
	if s.stuckSettlingAge <= 0 {
		s.stuckSettlingAge = defaultStuckSettlingAge
	}
	if s.maxExecutionAge <= 0 {
		s.maxExecutionAge = defaultMaxExecutionAge
	}
	if s.orphanReserveMinAge <= 0 {
		s.orphanReserveMinAge = defaultOrphanReserveMinAge
	}
	if s.storeNeedingStoreAge <= 0 {
		s.storeNeedingStoreAge = defaultStoreNeedingStoreAge
	}
	if s.sweepBatchSize <= 0 {
		s.sweepBatchSize = defaultSweepBatchSize
	}
	if s.resultHTTPClient == nil {
		s.resultHTTPClient = &http.Client{Timeout: defaultResultFetchTimeout}
	}
	if s.maxResultBytes <= 0 {
		s.maxResultBytes = defaultMaxResultBytes
	}
	return s, nil
}

// =============================================================================
// 提交流程
// =============================================================================

// SubmitParams 提交入参（鉴权/entitlement/能力校验/billing 由调用方 Unit 10 handler 完成）。
type SubmitParams struct {
	BusinessAccountID string
	// ActorTokenID 业务 API Key 自增 ID（写 task.token_id；nil = 不记）。
	ActorTokenID *int64
	// ChannelID 绑定的 channel id（caller 已由 ChannelName 解析；nil = 不绑）。
	ChannelID *int64
	// Entry catalog 条目（路由 + pricing）。
	Entry *video.VideoModelEntry
	// Request 已校验+规范化请求（Unit 4）。
	Request *video.ValidatedRequest

	// --- billing（caller 经 Unit 7 估算）---
	ReserveMinor  int64 // 预占金额（> 0）
	ReserveTokens int64 // token 上界
	MinTokenFloor int64 // 最低 token 计费下限

	// CallbackToken 回调 token（Unit 8 生成；6a 为空 = 纯轮询）。
	CallbackToken string
}

// Submit 执行提交流程（plan §Unit 6 Approach 事务边界）：
//
//	reserve（独立 ledger tx） → 单 tx{claim 占位 + 落 task} → 入队 submit job → 返 task_id
//
// 失败处理：claim/落库失败 → Release reserve（无 orphan）；claim 占不到 → ErrConcurrencyLimit(429)；
// 入队失败非致命（task 已 SUBMITTED，reconciler 6b 重投）。reserve 错误（余额/冻结）原样上抛
// （含 ledger sentinel，handler errors.Is 映射）。
func (s *Service) Submit(ctx context.Context, p SubmitParams) (string, error) {
	if p.Entry == nil || p.Request == nil {
		return "", errors.New("task.Submit: Entry / Request 不能为 nil")
	}
	if p.ReserveMinor <= 0 {
		return "", errors.New("task.Submit: ReserveMinor 必须 > 0")
	}
	tier, ok := p.Entry.Pricing.Tier(p.Request.Resolution)
	if !ok {
		return "", fmt.Errorf("task.Submit: 分辨率 %q 无定价（catalog 不一致）", p.Request.Resolution)
	}

	taskID := newTaskID()
	correlation := taskID // 钉死 reserve↔commit/release 同 correlation（plan §Unit 7）
	// 回调 token（Unit 8）：仅在配置了回调 base URL 时生成（纯轮询模式无需）；caller 显式传值则尊重
	//（测试覆写）。token 是独立于 task_id 的秘密，只嵌入交给上游的回调 URL，业务方不可见。
	if p.CallbackToken == "" && s.callbackBaseURL != "" {
		p.CallbackToken = newCallbackToken()
	}
	snap := TaskFinancialSnapshot{
		GatewayModel:               p.Entry.GatewayModelName,
		UpstreamModel:              p.Entry.UpstreamModelName,
		ProviderType:               p.Entry.UpstreamProviderType,
		ChannelName:                p.Entry.ChannelName,
		TaskType:                   string(p.Request.TaskType),
		Prompt:                     p.Request.Prompt,
		Duration:                   p.Request.Duration,
		Resolution:                 p.Request.Resolution,
		Ratio:                      p.Request.Ratio,
		Fps:                        p.Request.Fps,
		ReservationCorrelationID:   correlation,
		ReserveMinor:               p.ReserveMinor,
		ReserveTokens:              p.ReserveTokens,
		PricePerMillionTokensMinor: tier.PricePerMillionTokensMinor,
		BillingMultiplierBP:        p.Entry.Pricing.BillingMultiplierBP,
		MinTokenFloor:              p.MinTokenFloor,
	}
	snapBytes, err := snap.Marshal()
	if err != nil {
		return "", fmt.Errorf("task.Submit: 组装 financial_snapshot 失败: %w", err)
	}

	actor := ledger.Actor{Type: ledger.ActorTypeTask, ID: taskID}

	// 1. Reserve（独立 ledger tx）
	if _, err := s.ledger.Reserve(ctx, actor, ledger.ReserveParams{
		AccountID:     p.BusinessAccountID,
		Amount:        p.ReserveMinor,
		CorrelationID: correlation,
		ReferenceType: referenceTypeVideoTask,
		ReferenceID:   taskID,
	}); err != nil {
		return "", fmt.Errorf("task.Submit reserve: %w", err) // 保留 ledger sentinel
	}

	// 2. 单 tx：claim 占位 + 落 task。任一失败 → Release reserve（无 orphan）。
	if err := s.claimAndInsert(ctx, p, taskID, snapBytes); err != nil {
		s.releaseReserveBestEffort(actor, p.BusinessAccountID, correlation, p.ReserveMinor, "submit_tx_failed")
		return "", err
	}

	// 3. 入队 submit（失败非致命：task 已 SUBMITTED，6b reconciler 扫 ScanSubmittedNoJob 重投）。
	if err := s.enqueuer.EnqueueSubmit(ctx, taskID); err != nil {
		s.logger.Error("task: 入队 submit job 失败；task 已落库 SUBMITTED，待 reconciler 重投",
			slog.String("task_id", taskID), slog.String("err", err.Error()))
	}
	return taskID, nil
}

// claimAndInsert 单 tx 内原子占并发位 + 落 task。
func (s *Service) claimAndInsert(ctx context.Context, p SubmitParams, taskID string, snapBytes []byte) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("task.Submit begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // 成功 Commit 后再 Rollback 无害

	q := s.q.WithTx(tx)

	// claim 占位：占不到（pgx.ErrNoRows）= 已达 cap = 429。
	// cap 值由 limits 按 (account, model) 解析（覆写优先），nil 回退静态默认（6a 兼容）。
	capLimit := s.cap
	if s.limits != nil {
		capLimit = s.limits.Cap(p.BusinessAccountID, p.Entry.GatewayModelName)
	}
	if _, err := q.ClaimConcurrencySlot(ctx, db.ClaimConcurrencySlotParams{
		BusinessAccountID: p.BusinessAccountID,
		Model:             p.Entry.GatewayModelName,
		CapLimit:          capLimit,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrConcurrencyLimit
		}
		return fmt.Errorf("task.Submit claim: %w", err)
	}

	if _, err := q.InsertTask(ctx, db.InsertTaskParams{
		ID:                taskID,
		BusinessAccountID: p.BusinessAccountID,
		TokenID:           pgInt8OrNull(p.ActorTokenID),
		ChannelID:         pgInt8OrNull(p.ChannelID),
		ProviderType:      p.Entry.UpstreamProviderType,
		Model:             p.Entry.GatewayModelName,
		FinancialSnapshot: snapBytes,
		AccountingMonth:   accountingMonth(time.Now()),
		CallbackToken:     pgTextOrNull(p.CallbackToken),
	}); err != nil {
		return fmt.Errorf("task.Submit insert: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("task.Submit commit tx: %w", err)
	}
	return nil
}

// =============================================================================
// 状态机 CAS 封装
// =============================================================================

// markUpstreamTerminal 把任务 CAS 进上游终态（COMPLETED/FAILED/CANCELLED/EXPIRED），
// **同事务释放并发 claim**，CAS 赢家唯一入队 settle（plan §Unit 6 / ADR-0006 决策 2）。
//
// 被 submit worker（提交失败）、6b reconciler/recover/expire 复用。CAS 输（他人已推进）→
// 不释放 claim（防 double-release）、不入队 settle，返回 (false, nil)。
func (s *Service) markUpstreamTerminal(
	ctx context.Context, taskID string, from, to db.TaskStatus,
	errCode, errMsg, accountID, model string,
) (won bool, err error) {
	if !isUpstreamTerminal(to) {
		return false, fmt.Errorf("task.markUpstreamTerminal: %q 非上游终态", to)
	}
	if !canTransition(from, to) {
		return false, fmt.Errorf("task.markUpstreamTerminal: 非法转移 %q→%q", from, to)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	affected, err := q.MarkTaskUpstreamTerminal(ctx, db.MarkTaskUpstreamTerminalParams{
		ToStatus:     to,
		ErrorCode:    pgTextOrNull(errCode),
		ErrorMessage: pgTextOrNull(errMsg),
		ID:           taskID,
		FromStatus:   from,
	})
	if err != nil {
		return false, err
	}
	if affected == 0 {
		return false, nil // CAS 输：他人已推进，幂等放弃
	}
	// 同事务释放 claim（进上游终态即释放并发槽）
	if _, err := q.ReleaseConcurrencySlot(ctx, db.ReleaseConcurrencySlotParams{
		BusinessAccountID: accountID,
		Model:             model,
	}); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	// 赢家唯一入队 settle（去抖；失败非致命，6b reconciler 兜底重投）
	if err := s.enqueuer.EnqueueSettle(ctx, taskID); err != nil {
		s.logger.Error("task: 入队 settle 失败；待 reconciler 重投",
			slog.String("task_id", taskID), slog.String("err", err.Error()))
	}
	return true, nil
}

// resolveCreds 解密 channel 凭据并映射为上游 adapter 凭据（即用即弃，绝不入日志）。
func (s *Service) resolveCreds(ctx context.Context, channelID pgtype.Int8) (video.UpstreamCredentials, error) {
	if !channelID.Valid {
		return video.UpstreamCredentials{}, errors.New("task: 任务无 channel_id，无法取上游凭据")
	}
	cc, err := s.creds.GetCredentialsForUpstream(ctx, channelID.Int64)
	if err != nil {
		return video.UpstreamCredentials{}, err
	}
	return video.UpstreamCredentials{APIKey: cc.APIKey}, nil
}

// =============================================================================
// helpers
// =============================================================================

// CallbackPathPrefix 回调 ingress 路由前缀（main.go 注册路由 + buildCallbackURL 构造 URL 同源，
// 保持一致）。完整路由：POST {CallbackPathPrefix}/:task_id/:token——token 置于路径不可枚举段（非 query）。
const CallbackPathPrefix = "/v1/callbacks/video"

// buildCallbackURL 由 callbackBaseURL + per-task token 构造交给上游的回调 URL（submit worker 用）。
//
// base 未配 / token 缺失 → 返空串（纯轮询模式，不注册回调）。
// **含 token 的完整 URL 绝不入日志/审计/span**（token 是回调鉴权秘密，ADR-0006 决策 5）。
func (s *Service) buildCallbackURL(taskID string, token pgtype.Text) string {
	if s.callbackBaseURL == "" || !token.Valid || token.String == "" {
		return ""
	}
	return strings.TrimRight(s.callbackBaseURL, "/") + CallbackPathPrefix + "/" + taskID + "/" + token.String
}

// newTaskID 生成全局唯一 task_id（"vtask_" + 16 字节随机 hex）。
func newTaskID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "vtask_" + hex.EncodeToString(b[:])
}

// accountingMonth 取计费归属月（UTC YYYY-MM）。
func accountingMonth(now time.Time) string {
	return now.UTC().Format("2006-01")
}

func pgInt8OrNull(p *int64) pgtype.Int8 {
	if p == nil {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: *p, Valid: true}
}

func pgTextOrNull(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}

func nullTime(t time.Time) sql.NullTime {
	return sql.NullTime{Time: t, Valid: true}
}

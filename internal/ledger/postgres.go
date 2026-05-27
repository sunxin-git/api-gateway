package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sunxin-git/api-gateway/internal/db"
)

// PostgreSQL SQLSTATE 常量（pgx 不自带常量集，自行声明以提高可读性）。
const (
	pgSQLStateUniqueViolation = "23505"
)

// 财务事件 outbox 保留期（1 年）；非财务事件 5 分钟。
const (
	financialRetention    = 365 * 24 * time.Hour
	nonFinancialRetention = 5 * time.Minute
)

// PostgresService 是 Service 接口的 Postgres 实现（计划 Unit 5）。
type PostgresService struct {
	pool    *pgxpool.Pool
	queries *db.Queries
	outbox  OutboxPublisher
	log     *slog.Logger
}

// 编译期断言实现接口。
var _ Service = (*PostgresService)(nil)

// NewPostgresService 构造 service。
//
// 入参：
//   - pool：pgxpool.Pool（启动时 fail-fast Ping 过的）
//   - outbox：OutboxPublisher 实现（一般 outbox.NewPostgresPublisher()）
//   - log：slog logger（不能为 nil）
//
// 不接受 nil pool / nil outbox / nil log；调用方传 nil 直接 panic。
func NewPostgresService(pool *pgxpool.Pool, outbox OutboxPublisher, log *slog.Logger) *PostgresService {
	if pool == nil {
		panic("ledger.NewPostgresService: pool 不能为 nil")
	}
	if outbox == nil {
		panic("ledger.NewPostgresService: outbox 不能为 nil")
	}
	if log == nil {
		panic("ledger.NewPostgresService: log 不能为 nil")
	}
	return &PostgresService{
		pool:    pool,
		queries: db.New(pool),
		outbox:  outbox,
		log:     log,
	}
}

// =============================================================================
// CreateAccount
// =============================================================================

func (s *PostgresService) CreateAccount(ctx context.Context, actor Actor, params CreateAccountParams) (*Account, error) {
	if err := actor.Validate(); err != nil {
		return nil, fmt.Errorf("invalid actor: %w", err)
	}
	if params.ID == "" {
		return nil, errors.New("CreateAccountParams.ID 不能为空")
	}
	meta := params.Metadata
	if len(meta) == 0 {
		meta = []byte("{}")
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("BeginTx 失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := s.queries.WithTx(tx)
	row, err := q.CreateBusinessAccount(ctx, db.CreateBusinessAccountParams{
		ID:                params.ID,
		IsolationRequired: params.IsolationRequired,
		Metadata:          meta,
	})
	if err != nil {
		// 检测 UNIQUE 冲突（SQLSTATE 23505 + pk_business_account 约束）→ 显式 sentinel。
		// 决策 D12（D-min document-review）：让 handler 能用 errors.Is 匹配为 409。
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.SQLState() == pgSQLStateUniqueViolation && pgErr.ConstraintName == "pk_business_account" {
			return nil, ErrAccountAlreadyExists
		}
		return nil, fmt.Errorf("CreateBusinessAccount 失败: %w", err)
	}
	if _, err := q.CreateBusinessAccountBalanceZero(ctx, params.ID); err != nil {
		return nil, fmt.Errorf("CreateBusinessAccountBalanceZero 失败: %w", err)
	}

	now := time.Now().UTC()
	payload, _ := json.Marshal(AccountCreatedPayload{
		BusinessAccountID: params.ID,
		IsolationRequired: params.IsolationRequired,
		OccurredAt:        now,
	})
	evt := Event{
		Type:                   EventTypeAccountCreated,
		BusinessAccountID:      params.ID,
		Payload:                payload,
		IsFinancial:            false,
		RetentionUntil:         now.Add(nonFinancialRetention),
		DeliveryIdempotencyKey: fmt.Sprintf("%s:%s:create", EventTypeAccountCreated, params.ID),
	}
	if err := s.outbox.PublishInTx(ctx, tx, evt); err != nil {
		return nil, fmt.Errorf("outbox.Publish 失败: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("Commit 失败: %w", err)
	}

	return rowToAccount(row), nil
}

// =============================================================================
// Recharge
// =============================================================================

func (s *PostgresService) Recharge(ctx context.Context, actor Actor, params RechargeParams) (*LedgerEntry, WriteOutcome, error) {
	if err := actor.Validate(); err != nil {
		return nil, 0, fmt.Errorf("invalid actor: %w", err)
	}
	if params.Amount <= 0 {
		return nil, 0, ErrInvalidAmount
	}
	if params.AccountID == "" {
		return nil, 0, errors.New("RechargeParams.AccountID 不能为空")
	}

	// 计算 canonical body sha256（用作幂等冲突时的内容一致性校验）。
	var bodyHash []byte
	if params.CanonicalBody != nil {
		h, err := canonicalizeBody(params.CanonicalBody)
		if err != nil {
			return nil, 0, fmt.Errorf("canonicalizeBody 失败: %w", err)
		}
		bodyHash = h
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, 0, fmt.Errorf("BeginTx 失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := s.queries.WithTx(tx)

	// 幂等检查：(entry_type='recharge', idempotency_key) 命中比对 body sha256。
	if params.IdempotencyKey != "" {
		existing, err := q.FindLedgerEntryByIdempotencyKey(ctx, db.FindLedgerEntryByIdempotencyKeyParams{
			EntryType:      db.LedgerEntryTypeRecharge,
			IdempotencyKey: pgtype.Text{String: params.IdempotencyKey, Valid: true},
		})
		switch {
		case err == nil:
			// 命中：比对 body sha256
			if !bytesEqual(existing.CanonicalBodySha256, bodyHash) {
				s.log.Error("Recharge 幂等冲突：idempotency_key 重用但 body 不一致",
					slog.String("account_id", params.AccountID),
					slog.String("idempotency_key", params.IdempotencyKey),
					slog.Int64("existing_ledger_id", existing.ID),
					slog.String("actor", actor.String()))
				return nil, 0, ErrIdempotencyConflict
			}
			// body 一致：幂等成功返原 entry（IdempotentReplay，调用方不应累加配额）
			return idempotencyRowToEntry(existing), WriteOutcomeIdempotentReplay, nil
		case errors.Is(err, pgx.ErrNoRows):
			// 未命中，继续走 CTE
		default:
			return nil, 0, fmt.Errorf("FindLedgerEntryByIdempotencyKey 失败: %w", err)
		}
	}

	// 读 balance 拿当前 version（用作 CAS expected_version）。
	bal, err := q.GetBalanceInTx(ctx, params.AccountID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, 0, ErrAccountNotFound
		}
		return nil, 0, fmt.Errorf("GetBalanceInTx 失败: %w", err)
	}

	// CTE 单语句原子写。
	//
	// correlation_id 取自参数或 idempotency_key；都为空时用 ""（不撞 correlation UNIQUE，
	// 因为 partial UNIQUE 索引 WHERE correlation_id <> ''）。
	correlationID := params.CorrelationID
	if correlationID == "" {
		correlationID = params.IdempotencyKey
	}
	res, err := q.RechargeAtomic(ctx, db.RechargeAtomicParams{
		BusinessAccountID:   params.AccountID,
		Amount:              params.Amount,
		CorrelationID:       correlationID,
		IdempotencyKey:      nullableText(params.IdempotencyKey),
		Snapshot:            []byte("{}"),
		ReferenceType:       nullableText(params.ReferenceType),
		ReferenceID:         nullableText(params.ReferenceID),
		Metadata:            mustMetadata(params.Metadata),
		ActorType:           toDBActorType(actor.Type),
		ActorID:             actor.ID,
		CanonicalBodySha256: bodyHash,
		ExpectedVersion:     bal.Version,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// CAS 失败：同 tx 内 fresh-read 判错（frozen / version）。
			return nil, 0, s.classifyRechargeError(ctx, q, params.AccountID, bal.Version)
		}
		return nil, 0, fmt.Errorf("RechargeAtomic 失败: %w", err)
	}

	// outbox event。
	now := time.Now().UTC()
	payload, _ := json.Marshal(AccountRechargedPayload{
		BusinessAccountID: params.AccountID,
		Amount:            params.Amount,
		NewAvailable:      res.Available,
		NewRechargeTotal:  res.RechargeTotal,
		LedgerEntryID:     res.NewLedgerID,
		OccurredAt:        now,
	})
	evt := Event{
		Type:              EventTypeAccountRecharged,
		BusinessAccountID: params.AccountID,
		Payload:           payload,
		IsFinancial:       true,
		RetentionUntil:    now.Add(financialRetention),
		DeliveryIdempotencyKey: fmt.Sprintf("%s:%s:%d",
			EventTypeAccountRecharged, params.AccountID, res.NewLedgerID),
	}
	if err := s.outbox.PublishInTx(ctx, tx, evt); err != nil {
		return nil, 0, fmt.Errorf("outbox.Publish 失败: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, 0, fmt.Errorf("Commit 失败: %w", err)
	}

	return &LedgerEntry{
		ID:                res.NewLedgerID,
		BusinessAccountID: params.AccountID,
		EntryType:         string(db.LedgerEntryTypeRecharge),
		Amount:            params.Amount,
		AvailableDelta:    params.Amount,
		ReservedDelta:     0,
		UsedDelta:         0,
		CorrelationID:     correlationID,
		IdempotencyKey:    params.IdempotencyKey,
		ReferenceType:     params.ReferenceType,
		ReferenceID:       params.ReferenceID,
		Metadata:          params.Metadata,
		Snapshot:          []byte("{}"),
		ActorType:         string(actor.Type),
		ActorID:           actor.ID,
		CreatedAt:         res.NewCreatedAt,
	}, WriteOutcomeFreshlyWritten, nil
}

// =============================================================================
// Reserve
// =============================================================================

func (s *PostgresService) Reserve(ctx context.Context, actor Actor, params ReserveParams) (*LedgerEntry, error) {
	if err := actor.Validate(); err != nil {
		return nil, fmt.Errorf("invalid actor: %w", err)
	}
	if params.Amount <= 0 {
		return nil, ErrInvalidAmount
	}
	if params.AccountID == "" || params.CorrelationID == "" {
		return nil, errors.New("ReserveParams.AccountID/CorrelationID 不能为空")
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("BeginTx 失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := s.queries.WithTx(tx)

	// 幂等检查：(account, correlation, 'reserve') 命中返原 entry（不重复扣余额）。
	if existing, err := q.FindLedgerEntryByCorrelationAndType(ctx, db.FindLedgerEntryByCorrelationAndTypeParams{
		BusinessAccountID: params.AccountID,
		CorrelationID:     params.CorrelationID,
		EntryType:         db.LedgerEntryTypeReserve,
	}); err == nil {
		return correlationRowToEntry(existing), nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("FindLedgerEntryByCorrelationAndType 失败: %w", err)
	}

	bal, err := q.GetBalanceInTx(ctx, params.AccountID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrAccountNotFound
		}
		return nil, fmt.Errorf("GetBalanceInTx 失败: %w", err)
	}

	res, err := q.ReserveAtomic(ctx, db.ReserveAtomicParams{
		BusinessAccountID: params.AccountID,
		Amount:            params.Amount,
		CorrelationID:     params.CorrelationID,
		Snapshot:          []byte("{}"),
		ReferenceType:     nullableText(params.ReferenceType),
		ReferenceID:       nullableText(params.ReferenceID),
		Metadata:          mustMetadata(params.Metadata),
		ActorType:         toDBActorType(actor.Type),
		ActorID:           actor.ID,
		ExpectedVersion:   bal.Version,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, s.classifyReserveError(ctx, q, params.AccountID, bal.Version, params.Amount)
		}
		return nil, fmt.Errorf("ReserveAtomic 失败: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("Commit 失败: %w", err)
	}

	return &LedgerEntry{
		ID:                res.NewLedgerID,
		BusinessAccountID: params.AccountID,
		EntryType:         string(db.LedgerEntryTypeReserve),
		Amount:            params.Amount,
		AvailableDelta:    -params.Amount,
		ReservedDelta:     params.Amount,
		UsedDelta:         0,
		CorrelationID:     params.CorrelationID,
		ReferenceType:     params.ReferenceType,
		ReferenceID:       params.ReferenceID,
		Metadata:          params.Metadata,
		Snapshot:          []byte("{}"),
		ActorType:         string(actor.Type),
		ActorID:           actor.ID,
		CreatedAt:         res.NewCreatedAt,
	}, nil
}

// =============================================================================
// Commit
// =============================================================================

func (s *PostgresService) Commit(ctx context.Context, actor Actor, params CommitParams) ([]LedgerEntry, error) {
	if err := actor.Validate(); err != nil {
		return nil, fmt.Errorf("invalid actor: %w", err)
	}
	if params.ActualCost <= 0 {
		return nil, ErrInvalidAmount
	}
	if params.AccountID == "" || params.CorrelationID == "" {
		return nil, errors.New("CommitParams.AccountID/CorrelationID 不能为空")
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("BeginTx 失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := s.queries.WithTx(tx)

	// 幂等检查：commit 已存在 → 直接返原（同 correlation_id）。
	if existing, err := q.FindLedgerEntryByCorrelationAndType(ctx, db.FindLedgerEntryByCorrelationAndTypeParams{
		BusinessAccountID: params.AccountID,
		CorrelationID:     params.CorrelationID,
		EntryType:         db.LedgerEntryTypeCommit,
	}); err == nil {
		return []LedgerEntry{*correlationRowToEntry(existing)}, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("FindLedgerEntryByCorrelationAndType 失败: %w", err)
	}

	// 前置：找同 correlation 的 active reserve。
	reserve, err := q.FindActiveReserveByCorrelation(ctx, db.FindActiveReserveByCorrelationParams{
		BusinessAccountID: params.AccountID,
		CorrelationID:     params.CorrelationID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// 不存在 active reserve：可能是从未 reserve，或 reserve 已 commit/release。
			// 用 FindAnyReserveByCorrelation 区分。
			if _, err2 := q.FindAnyReserveByCorrelation(ctx, db.FindAnyReserveByCorrelationParams{
				BusinessAccountID: params.AccountID,
				CorrelationID:     params.CorrelationID,
			}); err2 == nil {
				return nil, ErrAlreadySettled
			}
			return nil, ErrReserveNotFound
		}
		return nil, fmt.Errorf("FindActiveReserveByCorrelation 失败: %w", err)
	}

	// 入口校验 actualCost ≤ reserve.amount（CLAUDE.md §四 #6 显式优于隐式）。
	if params.ActualCost > reserve.Amount {
		return nil, ErrCommitExceedsReserved
	}
	releaseAmount := reserve.Amount - params.ActualCost

	bal, err := q.GetBalanceInTx(ctx, params.AccountID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrAccountNotFound
		}
		return nil, fmt.Errorf("GetBalanceInTx 失败: %w", err)
	}

	releaseCorrID := params.CorrelationID + ":release"
	entries := make([]LedgerEntry, 0, 2)

	if releaseAmount > 0 {
		// 有残余：调 CommitWithReleaseAtomic（commit + release 两条 entry）。
		res, err := q.CommitWithReleaseAtomic(ctx, db.CommitWithReleaseAtomicParams{
			BusinessAccountID:    params.AccountID,
			ActualCost:           params.ActualCost,
			CorrelationID:        params.CorrelationID,
			Snapshot:             []byte("{}"),
			ReferenceType:        nullableText(params.ReferenceType),
			ReferenceID:          nullableText(params.ReferenceID),
			Metadata:             mustMetadata(params.Metadata),
			ActorType:            toDBActorType(actor.Type),
			ActorID:              actor.ID,
			ReleaseAmount:        releaseAmount,
			ReleaseCorrelationID: releaseCorrID,
			ReserveAmount:        reserve.Amount,
			ExpectedVersion:      bal.Version,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, s.classifyCommitError(ctx, q, params.AccountID, bal.Version, reserve.Amount)
			}
			return nil, fmt.Errorf("CommitWithReleaseAtomic 失败: %w", err)
		}
		entries = append(entries, LedgerEntry{
			ID:                res.CommitLedgerID,
			BusinessAccountID: params.AccountID,
			EntryType:         string(db.LedgerEntryTypeCommit),
			Amount:            params.ActualCost,
			AvailableDelta:    0,
			ReservedDelta:     -params.ActualCost,
			UsedDelta:         params.ActualCost,
			CorrelationID:     params.CorrelationID,
			ReferenceType:     params.ReferenceType,
			ReferenceID:       params.ReferenceID,
			Metadata:          params.Metadata,
			Snapshot:          []byte("{}"),
			ActorType:         string(actor.Type),
			ActorID:           actor.ID,
			CreatedAt:         res.CommitCreatedAt,
		})
		entries = append(entries, LedgerEntry{
			ID:                res.ReleaseLedgerID,
			BusinessAccountID: params.AccountID,
			EntryType:         string(db.LedgerEntryTypeRelease),
			Amount:            releaseAmount,
			AvailableDelta:    releaseAmount,
			ReservedDelta:     -releaseAmount,
			UsedDelta:         0,
			CorrelationID:     releaseCorrID,
			ReferenceType:     params.ReferenceType,
			ReferenceID:       params.ReferenceID,
			Metadata:          params.Metadata,
			Snapshot:          []byte("{}"),
			ActorType:         string(actor.Type),
			ActorID:           actor.ID,
			CreatedAt:         res.ReleaseCreatedAt,
		})
	} else {
		// 无残余：CommitAtomic 只 INSERT commit。
		res, err := q.CommitAtomic(ctx, db.CommitAtomicParams{
			BusinessAccountID: params.AccountID,
			ActualCost:        params.ActualCost,
			CorrelationID:     params.CorrelationID,
			Snapshot:          []byte("{}"),
			ReferenceType:     nullableText(params.ReferenceType),
			ReferenceID:       nullableText(params.ReferenceID),
			Metadata:          mustMetadata(params.Metadata),
			ActorType:         toDBActorType(actor.Type),
			ActorID:           actor.ID,
			ReserveAmount:     reserve.Amount,
			ExpectedVersion:   bal.Version,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, s.classifyCommitError(ctx, q, params.AccountID, bal.Version, reserve.Amount)
			}
			return nil, fmt.Errorf("CommitAtomic 失败: %w", err)
		}
		entries = append(entries, LedgerEntry{
			ID:                res.NewLedgerID,
			BusinessAccountID: params.AccountID,
			EntryType:         string(db.LedgerEntryTypeCommit),
			Amount:            params.ActualCost,
			AvailableDelta:    0,
			ReservedDelta:     -params.ActualCost,
			UsedDelta:         params.ActualCost,
			CorrelationID:     params.CorrelationID,
			ReferenceType:     params.ReferenceType,
			ReferenceID:       params.ReferenceID,
			Metadata:          params.Metadata,
			Snapshot:          []byte("{}"),
			ActorType:         string(actor.Type),
			ActorID:           actor.ID,
			CreatedAt:         res.NewCreatedAt,
		})
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("Commit 失败: %w", err)
	}
	return entries, nil
}

// =============================================================================
// Release
// =============================================================================

func (s *PostgresService) Release(ctx context.Context, actor Actor, params ReleaseParams) (*LedgerEntry, error) {
	if err := actor.Validate(); err != nil {
		return nil, fmt.Errorf("invalid actor: %w", err)
	}
	if params.Amount <= 0 {
		return nil, ErrInvalidAmount
	}
	if params.AccountID == "" || params.CorrelationID == "" {
		return nil, errors.New("ReleaseParams.AccountID/CorrelationID 不能为空")
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("BeginTx 失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := s.queries.WithTx(tx)

	// 幂等检查：(account, correlation+":release", 'release') 命中返原。
	releaseCorrID := params.CorrelationID + ":release"
	if existing, err := q.FindLedgerEntryByCorrelationAndType(ctx, db.FindLedgerEntryByCorrelationAndTypeParams{
		BusinessAccountID: params.AccountID,
		CorrelationID:     releaseCorrID,
		EntryType:         db.LedgerEntryTypeRelease,
	}); err == nil {
		return correlationRowToEntry(existing), nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("FindLedgerEntryByCorrelationAndType 失败: %w", err)
	}

	// 前置：找 active reserve。
	reserve, err := q.FindActiveReserveByCorrelation(ctx, db.FindActiveReserveByCorrelationParams{
		BusinessAccountID: params.AccountID,
		CorrelationID:     params.CorrelationID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			if _, err2 := q.FindAnyReserveByCorrelation(ctx, db.FindAnyReserveByCorrelationParams{
				BusinessAccountID: params.AccountID,
				CorrelationID:     params.CorrelationID,
			}); err2 == nil {
				return nil, ErrAlreadySettled
			}
			return nil, ErrReserveNotFound
		}
		return nil, fmt.Errorf("FindActiveReserveByCorrelation 失败: %w", err)
	}

	if params.Amount != reserve.Amount {
		// Release 按设计是「全额释放原 reserve」；不允许部分。如需部分用 Commit + actualCost。
		return nil, fmt.Errorf("Release.Amount(%d) 必须等于 reserve.Amount(%d)：%w",
			params.Amount, reserve.Amount, ErrInvalidAmount)
	}

	bal, err := q.GetBalanceInTx(ctx, params.AccountID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrAccountNotFound
		}
		return nil, fmt.Errorf("GetBalanceInTx 失败: %w", err)
	}

	res, err := q.ReleaseAtomic(ctx, db.ReleaseAtomicParams{
		BusinessAccountID: params.AccountID,
		Amount:            params.Amount,
		CorrelationID:     releaseCorrID,
		Snapshot:          []byte("{}"),
		ReferenceType:     nullableText(params.ReferenceType),
		ReferenceID:       nullableText(params.ReferenceID),
		Metadata:          mustMetadata(params.Metadata),
		ActorType:         toDBActorType(actor.Type),
		ActorID:           actor.ID,
		ExpectedVersion:   bal.Version,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, s.classifyReleaseError(ctx, q, params.AccountID, bal.Version, params.Amount)
		}
		return nil, fmt.Errorf("ReleaseAtomic 失败: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("Commit 失败: %w", err)
	}

	return &LedgerEntry{
		ID:                res.NewLedgerID,
		BusinessAccountID: params.AccountID,
		EntryType:         string(db.LedgerEntryTypeRelease),
		Amount:            params.Amount,
		AvailableDelta:    params.Amount,
		ReservedDelta:     -params.Amount,
		UsedDelta:         0,
		CorrelationID:     releaseCorrID,
		ReferenceType:     params.ReferenceType,
		ReferenceID:       params.ReferenceID,
		Metadata:          params.Metadata,
		Snapshot:          []byte("{}"),
		ActorType:         string(actor.Type),
		ActorID:           actor.ID,
		CreatedAt:         res.NewCreatedAt,
	}, nil
}

// =============================================================================
// Refund
// =============================================================================

func (s *PostgresService) Refund(ctx context.Context, actor Actor, params RefundParams) (*LedgerEntry, WriteOutcome, error) {
	if err := actor.Validate(); err != nil {
		return nil, 0, fmt.Errorf("invalid actor: %w", err)
	}
	if params.Amount <= 0 {
		return nil, 0, ErrInvalidAmount
	}
	if params.AccountID == "" || params.CorrelationID == "" {
		return nil, 0, errors.New("RefundParams.AccountID/CorrelationID 不能为空")
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, 0, fmt.Errorf("BeginTx 失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := s.queries.WithTx(tx)

	// 幂等检查：(account, correlation, 'refund')。
	if existing, err := q.FindLedgerEntryByCorrelationAndType(ctx, db.FindLedgerEntryByCorrelationAndTypeParams{
		BusinessAccountID: params.AccountID,
		CorrelationID:     params.CorrelationID,
		EntryType:         db.LedgerEntryTypeRefund,
	}); err == nil {
		// 幂等命中：返原 entry + IdempotentReplay outcome（调用方不累加 refund 配额）
		return correlationRowToEntry(existing), WriteOutcomeIdempotentReplay, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, fmt.Errorf("FindLedgerEntryByCorrelationAndType 失败: %w", err)
	}

	bal, err := q.GetBalanceInTx(ctx, params.AccountID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, 0, ErrAccountNotFound
		}
		return nil, 0, fmt.Errorf("GetBalanceInTx 失败: %w", err)
	}

	res, err := q.RefundAtomic(ctx, db.RefundAtomicParams{
		BusinessAccountID: params.AccountID,
		Amount:            params.Amount,
		CorrelationID:     params.CorrelationID,
		Snapshot:          []byte("{}"),
		ReferenceType:     nullableText(params.ReferenceType),
		ReferenceID:       nullableText(params.ReferenceID),
		Metadata:          mustMetadata(params.Metadata),
		ActorType:         toDBActorType(actor.Type),
		ActorID:           actor.ID,
		ExpectedVersion:   bal.Version,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, 0, s.classifyRefundError(ctx, q, params.AccountID, bal.Version, params.Amount)
		}
		return nil, 0, fmt.Errorf("RefundAtomic 失败: %w", err)
	}

	now := time.Now().UTC()
	payload, _ := json.Marshal(AccountRefundedPayload{
		BusinessAccountID: params.AccountID,
		Amount:            params.Amount,
		NewAvailable:      res.Available,
		NewUsedTotal:      res.UsedTotal,
		NewRefundTotal:    res.RefundTotal,
		LedgerEntryID:     res.NewLedgerID,
		ReferenceType:     params.ReferenceType,
		ReferenceID:       params.ReferenceID,
		OccurredAt:        now,
	})
	evt := Event{
		Type:                   EventTypeAccountRefunded,
		BusinessAccountID:      params.AccountID,
		Payload:                payload,
		IsFinancial:            true,
		RetentionUntil:         now.Add(financialRetention),
		DeliveryIdempotencyKey: fmt.Sprintf("%s:%s:%d", EventTypeAccountRefunded, params.AccountID, res.NewLedgerID),
	}
	if err := s.outbox.PublishInTx(ctx, tx, evt); err != nil {
		return nil, 0, fmt.Errorf("outbox.Publish 失败: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, 0, fmt.Errorf("Commit 失败: %w", err)
	}

	return &LedgerEntry{
		ID:                res.NewLedgerID,
		BusinessAccountID: params.AccountID,
		EntryType:         string(db.LedgerEntryTypeRefund),
		Amount:            params.Amount,
		AvailableDelta:    params.Amount,
		ReservedDelta:     0,
		UsedDelta:         -params.Amount,
		CorrelationID:     params.CorrelationID,
		ReferenceType:     params.ReferenceType,
		ReferenceID:       params.ReferenceID,
		Metadata:          params.Metadata,
		Snapshot:          []byte("{}"),
		ActorType:         string(actor.Type),
		ActorID:           actor.ID,
		CreatedAt:         res.NewCreatedAt,
	}, WriteOutcomeFreshlyWritten, nil
}

// =============================================================================
// GetBalance
// =============================================================================

func (s *PostgresService) GetBalance(ctx context.Context, accountID string) (*Balance, error) {
	if accountID == "" {
		return nil, errors.New("accountID 不能为空")
	}
	row, err := s.queries.GetBalance(ctx, accountID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrAccountNotFound
		}
		return nil, fmt.Errorf("GetBalance 失败: %w", err)
	}
	return balanceRowToBalance(row), nil
}

// =============================================================================
// Freeze / Unfreeze
// =============================================================================

// Freeze 公开版本：自带 tx 包装 freezeInTx。
func (s *PostgresService) Freeze(ctx context.Context, actor Actor, accountID string, reasonCode ReasonCode) error {
	if err := actor.Validate(); err != nil {
		return fmt.Errorf("invalid actor: %w", err)
	}
	if accountID == "" {
		return errors.New("accountID 不能为空")
	}
	if reasonCode == "" {
		return errors.New("reasonCode 不能为空")
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("BeginTx 失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := s.freezeInTx(ctx, tx, actor, accountID, reasonCode); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("Commit 失败: %w", err)
	}
	return nil
}

// Unfreeze 公开版本：自带 tx 包装 unfreezeInTx。
func (s *PostgresService) Unfreeze(ctx context.Context, actor Actor, accountID string, reasonCode ReasonCode) error {
	if err := actor.Validate(); err != nil {
		return fmt.Errorf("invalid actor: %w", err)
	}
	if accountID == "" {
		return errors.New("accountID 不能为空")
	}
	if reasonCode == "" {
		return errors.New("reasonCode 不能为空")
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("BeginTx 失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := s.unfreezeInTx(ctx, tx, actor, accountID, reasonCode); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("Commit 失败: %w", err)
	}
	return nil
}

// =============================================================================
// RebuildBalance —— 实装在 rebuild.go（3 个独立 tx：freeze / read / replace+unfreeze）
// =============================================================================

// =============================================================================
// 公开转换 helper（reconciler 等同包外可见 caller 可能复用）
// =============================================================================

// rowToAccount 把 db.BusinessAccount 转 service 层 Account。
func rowToAccount(row db.BusinessAccount) *Account {
	out := &Account{
		ID:                row.ID,
		Status:            string(row.Status),
		IsolationRequired: row.IsolationRequired,
		Metadata:          row.Metadata,
		CreatedAt:         row.CreatedAt,
		UpdatedAt:         row.UpdatedAt,
	}
	if row.BreakGlassUntil.Valid {
		t := row.BreakGlassUntil.Time
		out.BreakGlassUntil = &t
	}
	return out
}

// balanceRowToBalance 把 db.BusinessAccountBalance 转 service 层 Balance。
func balanceRowToBalance(row db.BusinessAccountBalance) *Balance {
	out := &Balance{
		BusinessAccountID: row.BusinessAccountID,
		Available:         row.Available,
		Reserved:          row.Reserved,
		UsedTotal:         row.UsedTotal,
		RechargeTotal:     row.RechargeTotal,
		RefundTotal:       row.RefundTotal,
		Version:           row.Version,
		Frozen:            row.Frozen,
		UpdatedAt:         row.UpdatedAt,
		LastLedgerID:      row.LastLedgerID,
	}
	if row.FrozenReason.Valid {
		out.FrozenReason = row.FrozenReason.String
	}
	if row.FrozenAt.Valid {
		t := row.FrozenAt.Time
		out.FrozenAt = &t
	}
	return out
}

// idempotencyRowToEntry 把幂等查询命中行转 LedgerEntry。
func idempotencyRowToEntry(row db.FindLedgerEntryByIdempotencyKeyRow) *LedgerEntry {
	return &LedgerEntry{
		ID:                row.ID,
		BusinessAccountID: row.BusinessAccountID,
		EntryType:         string(row.EntryType),
		Amount:            row.Amount,
		AvailableDelta:    row.AvailableDelta,
		ReservedDelta:     row.ReservedDelta,
		UsedDelta:         row.UsedDelta,
		CorrelationID:     row.CorrelationID,
		IdempotencyKey:    textOrEmpty(row.IdempotencyKey),
		ReferenceType:     textOrEmpty(row.ReferenceType),
		ReferenceID:       textOrEmpty(row.ReferenceID),
		Metadata:          row.Metadata,
		Snapshot:          row.Snapshot,
		ActorType:         string(row.ActorType),
		ActorID:           row.ActorID,
		CreatedAt:         row.CreatedAt,
	}
}

// correlationRowToEntry 把 correlation_id 反查行转 LedgerEntry。
func correlationRowToEntry(row db.FindLedgerEntryByCorrelationAndTypeRow) *LedgerEntry {
	return &LedgerEntry{
		ID:                row.ID,
		BusinessAccountID: row.BusinessAccountID,
		EntryType:         string(row.EntryType),
		Amount:            row.Amount,
		AvailableDelta:    row.AvailableDelta,
		ReservedDelta:     row.ReservedDelta,
		UsedDelta:         row.UsedDelta,
		CorrelationID:     row.CorrelationID,
		IdempotencyKey:    textOrEmpty(row.IdempotencyKey),
		ReferenceType:     textOrEmpty(row.ReferenceType),
		ReferenceID:       textOrEmpty(row.ReferenceID),
		Metadata:          row.Metadata,
		Snapshot:          row.Snapshot,
		ActorType:         string(row.ActorType),
		ActorID:           row.ActorID,
		CreatedAt:         row.CreatedAt,
	}
}

// 抑制 unused 警告（database/sql 在 helpers 文件用，但 postgres.go 也间接需要 import 一致性）。
var _ = sql.ErrNoRows

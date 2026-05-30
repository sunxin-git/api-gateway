package operator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"github.com/sunxin-git/api-gateway/internal/db"
)

// 用户名 / 口令长度约束（ADR-0008：最简模型，仅长度 + 字符集，不做复杂策略）。
const (
	minUsernameLen = 3
	maxUsernameLen = 64
	minPasswordLen = 8 // 字符数下限（按 rune 计；多字节口令不被字节数高估为"够长"）
	// bcrypt 只取口令前 72 字节；超过会被静默截断（旧版）或 GenerateFromPassword 报错（新版）。
	// 显式上限给清晰错误，避免「>72 字节口令尾部改了也能登录」的反直觉。
	maxPasswordBytes = 72

	// defaultBcryptCost 默认 bcrypt cost = 12（OWASP 2023 建议 ≥12，~400ms）。
	// 管理后台低频登录，更高 cost 几乎不影响体验却显著提高离线爆破成本；测试用 MinCost 提速。
	defaultBcryptCost = 12
)

// PostgresService 是 Service 的 Postgres 实现（计划 Unit 3）。
//
// 持有 pgxpool + sqlc Queries + bcrypt cost + 一个 dummy bcrypt 哈希（用户名不存在时
// 仍做一次 bcrypt 比对，近似常量时间，弱化「账户是否存在」的时序侧信道）。
type PostgresService struct {
	pool       *pgxpool.Pool
	queries    *db.Queries
	log        *slog.Logger
	bcryptCost int
	// dummyHash 防枚举：不存在账户时对其做 CompareHashAndPassword（近似常量时间）。
	// 残留风险：极精确时序 + 大量采样下理论仍可区分账户存在性；商业内部后台可接受。
	dummyHash []byte
}

// 编译期断言实现接口。
var _ Service = (*PostgresService)(nil)

// NewPostgresService 构造 service。
//
// 不接受 nil pool / nil log（启动期 fail-fast panic）。bcryptCost <= 0 时用 bcrypt.DefaultCost。
func NewPostgresService(pool *pgxpool.Pool, log *slog.Logger) *PostgresService {
	return NewPostgresServiceWithCost(pool, log, defaultBcryptCost)
}

// NewPostgresServiceWithCost 同 NewPostgresService 但显式指定 bcrypt cost（测试用低 cost 提速）。
func NewPostgresServiceWithCost(pool *pgxpool.Pool, log *slog.Logger, cost int) *PostgresService {
	if pool == nil {
		panic("operator.NewPostgresService: pool 不能为 nil")
	}
	if log == nil {
		panic("operator.NewPostgresService: log 不能为 nil")
	}
	if cost < bcrypt.MinCost || cost > bcrypt.MaxCost {
		cost = bcrypt.DefaultCost
	}
	// dummy 哈希：固定明文，仅用于「不存在账户」分支做等价 bcrypt 比对。
	dummy, err := bcrypt.GenerateFromPassword([]byte("operator-enumeration-guard"), cost)
	if err != nil {
		// cost 已校验在合法区间，理论不会失败；失败即配置异常，fail-fast。
		panic(fmt.Sprintf("operator.NewPostgresService: 生成 dummy 哈希失败: %v", err))
	}
	return &PostgresService{
		pool:       pool,
		queries:    db.New(pool),
		log:        log,
		bcryptCost: cost,
		dummyHash:  dummy,
	}
}

// =============================================================================
// Create
// =============================================================================

func (s *PostgresService) Create(ctx context.Context, params CreateParams) (*OperatorAccount, error) {
	username := strings.TrimSpace(params.Username)
	if err := validateUsername(username); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidParam, err.Error())
	}
	if err := validatePassword(params.Password); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidParam, err.Error())
	}
	if strings.TrimSpace(params.CreatedBy) == "" {
		return nil, fmt.Errorf("%w: created_by 不能为空", ErrInvalidParam)
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(params.Password), s.bcryptCost)
	if err != nil {
		return nil, fmt.Errorf("bcrypt 生成口令哈希失败: %w", err)
	}

	row, err := s.queries.InsertOperatorAccount(ctx, db.InsertOperatorAccountParams{
		Username:     username,
		PasswordHash: string(hash),
		Enabled:      true,
		CreatedBy:    params.CreatedBy,
	})
	if err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: %q", ErrUsernameExists, username)
		}
		return nil, fmt.Errorf("InsertOperatorAccount 失败: %w", err)
	}
	return &OperatorAccount{
		ID:        row.ID,
		Username:  row.Username,
		Enabled:   row.Enabled,
		CreatedBy: row.CreatedBy,
		CreatedAt: row.CreatedAt,
		UpdatedAt: row.UpdatedAt,
	}, nil
}

// =============================================================================
// Authenticate
// =============================================================================

func (s *PostgresService) Authenticate(ctx context.Context, username, password string) (*OperatorAccount, error) {
	username = strings.TrimSpace(username)

	row, err := s.queries.GetOperatorAccountByUsername(ctx, username)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// 账户不存在：仍做一次 bcrypt 比对，弱化时序侧信道，再统一返 ErrAuthFailed。
			_ = bcrypt.CompareHashAndPassword(s.dummyHash, []byte(password))
			return nil, ErrAuthFailed
		}
		return nil, fmt.Errorf("GetOperatorAccountByUsername 失败: %w", err)
	}

	// 先比对口令（无论是否禁用都执行，避免按 enabled 短路泄露时序）。
	cmpErr := bcrypt.CompareHashAndPassword([]byte(row.PasswordHash), []byte(password))
	if cmpErr != nil || !row.Enabled {
		return nil, ErrAuthFailed
	}

	return &OperatorAccount{
		ID:        row.ID,
		Username:  row.Username,
		Enabled:   row.Enabled,
		CreatedBy: row.CreatedBy,
		CreatedAt: row.CreatedAt,
		UpdatedAt: row.UpdatedAt,
	}, nil
}

// =============================================================================
// GetByID / List / SetEnabled / SetPassword / Count
// =============================================================================

func (s *PostgresService) GetByID(ctx context.Context, id int64) (*OperatorAccount, error) {
	row, err := s.queries.GetOperatorAccountByID(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("GetOperatorAccountByID 失败: %w", err)
	}
	return &OperatorAccount{
		ID:        row.ID,
		Username:  row.Username,
		Enabled:   row.Enabled,
		CreatedBy: row.CreatedBy,
		CreatedAt: row.CreatedAt,
		UpdatedAt: row.UpdatedAt,
	}, nil
}

func (s *PostgresService) List(ctx context.Context) ([]*OperatorAccount, error) {
	rows, err := s.queries.ListOperatorAccounts(ctx)
	if err != nil {
		return nil, fmt.Errorf("ListOperatorAccounts 失败: %w", err)
	}
	out := make([]*OperatorAccount, 0, len(rows))
	for _, r := range rows {
		out = append(out, &OperatorAccount{
			ID:        r.ID,
			Username:  r.Username,
			Enabled:   r.Enabled,
			CreatedBy: r.CreatedBy,
			CreatedAt: r.CreatedAt,
			UpdatedAt: r.UpdatedAt,
		})
	}
	return out, nil
}

func (s *PostgresService) SetEnabled(ctx context.Context, id int64, enabled bool) (*OperatorAccount, error) {
	row, err := s.queries.SetOperatorAccountEnabled(ctx, db.SetOperatorAccountEnabledParams{
		Enabled: enabled,
		ID:      id,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("SetOperatorAccountEnabled 失败: %w", err)
	}
	return &OperatorAccount{
		ID:        row.ID,
		Username:  row.Username,
		Enabled:   row.Enabled,
		CreatedBy: row.CreatedBy,
		CreatedAt: row.CreatedAt,
		UpdatedAt: row.UpdatedAt,
	}, nil
}

func (s *PostgresService) SetPassword(ctx context.Context, id int64, newPassword string) error {
	if err := validatePassword(newPassword); err != nil {
		return fmt.Errorf("%w: %s", ErrInvalidParam, err.Error())
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), s.bcryptCost)
	if err != nil {
		return fmt.Errorf("bcrypt 生成口令哈希失败: %w", err)
	}
	n, err := s.queries.UpdateOperatorPassword(ctx, db.UpdateOperatorPasswordParams{
		PasswordHash: string(hash),
		ID:           id,
	})
	if err != nil {
		return fmt.Errorf("UpdateOperatorPassword 失败: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgresService) Count(ctx context.Context) (int64, error) {
	n, err := s.queries.CountOperatorAccounts(ctx)
	if err != nil {
		return 0, fmt.Errorf("CountOperatorAccounts 失败: %w", err)
	}
	return n, nil
}

// =============================================================================
// 内部 helpers
// =============================================================================

// validateUsername 用户名校验：长度 [min,max] + 仅 [a-zA-Z0-9._-]（不面向公众，约束从严无碍）。
func validateUsername(username string) error {
	if username == "" {
		return errors.New("username 不能为空")
	}
	if len(username) < minUsernameLen || len(username) > maxUsernameLen {
		return fmt.Errorf("username 长度须在 %d–%d 之间", minUsernameLen, maxUsernameLen)
	}
	for _, r := range username {
		if r > unicode.MaxASCII || (!isASCIILetterDigit(r) && r != '.' && r != '_' && r != '-') {
			return errors.New("username 仅允许字母 / 数字 / . _ -")
		}
	}
	return nil
}

// validatePassword 口令校验：长度下限 + bcrypt 72 字节上限（防静默截断）。
func validatePassword(password string) error {
	if utf8.RuneCountInString(password) < minPasswordLen {
		return fmt.Errorf("口令字符数须 ≥ %d", minPasswordLen)
	}
	if len(password) > maxPasswordBytes {
		return fmt.Errorf("口令字节数须 ≤ %d（bcrypt 上限）", maxPasswordBytes)
	}
	return nil
}

func isASCIILetterDigit(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

// isUniqueViolation 判断是否 PG 唯一约束冲突（SQLSTATE 23505）。
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

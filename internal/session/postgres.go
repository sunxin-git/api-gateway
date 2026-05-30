package session

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sunxin-git/api-gateway/internal/db"
)

// tokenBytes session / csrf token 的 CSPRNG 字节数（与 admintoken / businesskey 一致）。
const tokenBytes = 32

// minPepperBytes 与 admintoken / businesskey 一致（HMAC 主密钥 ≥ 32 字节）。
const minPepperBytes = 32

// PostgresService 是 Service 的 Postgres 实现。
//
// 持有 pgxpool + sqlc Queries + pepper（HMAC session token；复用 GATEWAY_TOKEN_PEPPER）+ ttl。
type PostgresService struct {
	pool    *pgxpool.Pool
	queries *db.Queries
	pepper  []byte // **绝不**记 log
	ttl     time.Duration
	log     *slog.Logger
}

var _ Service = (*PostgresService)(nil)

// NewPostgresService 构造 service。
//
// 不接受 nil pool / nil log / pepper < 32 字节 / ttl <= 0（启动期 fail-fast panic）。
// pepper 通常注入 config.TokenPepperBytes（与 admin token / business key 同源）。
func NewPostgresService(pool *pgxpool.Pool, pepper []byte, ttl time.Duration, log *slog.Logger) *PostgresService {
	if pool == nil {
		panic("session.NewPostgresService: pool 不能为 nil")
	}
	if len(pepper) < minPepperBytes {
		panic(fmt.Sprintf("session.NewPostgresService: pepper 长度 %d < %d 字节", len(pepper), minPepperBytes))
	}
	if ttl <= 0 {
		panic(fmt.Sprintf("session.NewPostgresService: ttl 必须 > 0（当前 %v）", ttl))
	}
	if log == nil {
		panic("session.NewPostgresService: log 不能为 nil")
	}
	p := make([]byte, len(pepper))
	copy(p, pepper)
	return &PostgresService{
		pool:    pool,
		queries: db.New(pool),
		pepper:  p,
		ttl:     ttl,
		log:     log,
	}
}

func (s *PostgresService) Create(ctx context.Context, operatorID int64) (string, string, time.Time, error) {
	token, err := randToken()
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("生成 session token 失败: %w", err)
	}
	csrf, err := randToken()
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("生成 csrf token 失败: %w", err)
	}
	expiresAt := time.Now().UTC().Add(s.ttl)

	if _, err := s.queries.InsertAdminSession(ctx, db.InsertAdminSessionParams{
		SessionTokenHash: s.hash(token),
		OperatorID:       operatorID,
		CsrfToken:        csrf,
		ExpiresAt:        expiresAt,
	}); err != nil {
		return "", "", time.Time{}, fmt.Errorf("InsertAdminSession 失败: %w", err)
	}
	return token, csrf, expiresAt, nil
}

func (s *PostgresService) Lookup(ctx context.Context, token string) (*SessionContext, error) {
	if token == "" {
		return nil, ErrSessionInvalid
	}
	row, err := s.queries.GetActiveAdminSessionByTokenHash(ctx, s.hash(token))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrSessionInvalid
		}
		return nil, fmt.Errorf("GetActiveAdminSessionByTokenHash 失败: %w", err)
	}
	return &SessionContext{
		SessionID:  row.SessionID,
		OperatorID: row.OperatorID,
		Username:   row.Username,
		CSRFToken:  row.CsrfToken,
		ExpiresAt:  row.ExpiresAt,
	}, nil
}

func (s *PostgresService) Delete(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	if _, err := s.queries.DeleteAdminSessionByTokenHash(ctx, s.hash(token)); err != nil {
		return fmt.Errorf("DeleteAdminSessionByTokenHash 失败: %w", err)
	}
	return nil
}

func (s *PostgresService) DeleteByOperator(ctx context.Context, operatorID int64) (int64, error) {
	n, err := s.queries.DeleteAdminSessionsByOperator(ctx, operatorID)
	if err != nil {
		return 0, fmt.Errorf("DeleteAdminSessionsByOperator 失败: %w", err)
	}
	return n, nil
}

func (s *PostgresService) DeleteExpired(ctx context.Context) (int64, error) {
	n, err := s.queries.DeleteExpiredAdminSessions(ctx)
	if err != nil {
		return 0, fmt.Errorf("DeleteExpiredAdminSessions 失败: %w", err)
	}
	return n, nil
}

// =============================================================================
// 内部 helpers
// =============================================================================

// hash 算法：HMAC-SHA-256(pepper, token) hex（与 admintoken / businesskey 同算法）。
func (s *PostgresService) hash(token string) string {
	mac := hmac.New(sha256.New, s.pepper)
	mac.Write([]byte(token))
	return hex.EncodeToString(mac.Sum(nil))
}

// randToken 32 字节 CSPRNG → base64url（≈ 43 字符，无填充）。
func randToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

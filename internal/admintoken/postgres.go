package admintoken

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
	"net/netip"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sunxin-git/api-gateway/internal/db"
)

// PostgresService 是 Service 接口的 Postgres 实现（计划 Unit 2）。
//
// 持有 pgxpool（与 LedgerService 一致）+ pepper bytes 用于 HMAC token hash（决策 D1）。
// 无状态：多 goroutine 并发安全。
type PostgresService struct {
	pool    *pgxpool.Pool
	queries *db.Queries
	// pepper HMAC 主密钥（≥ 32 字节）；从 config.TokenPepper 注入；NewPostgresService 校验长度。
	// **绝不**记 log / 暴露给上层。
	pepper []byte
	log    *slog.Logger
}

// 编译期断言实现接口。
var _ Service = (*PostgresService)(nil)

// NewPostgresService 构造 service。
//
// 入参：
//   - pool：pgxpool.Pool（启动时 fail-fast Ping 过的）
//   - pepper：HMAC 主密钥原始字节（≥ 32 字节），通常从 config.TokenPepper 注入
//   - log：slog logger（不能为 nil）
//
// 不接受 nil pool / nil log / pepper < 32 字节；不合法直接 panic（启动期 fail-fast）。
func NewPostgresService(pool *pgxpool.Pool, pepper []byte, log *slog.Logger) *PostgresService {
	if pool == nil {
		panic("admintoken.NewPostgresService: pool 不能为 nil")
	}
	if len(pepper) < 32 {
		panic(fmt.Sprintf("admintoken.NewPostgresService: pepper 长度 %d < 32 字节（决策 D1）", len(pepper)))
	}
	if log == nil {
		panic("admintoken.NewPostgresService: log 不能为 nil")
	}
	// 复制 pepper 避免上层 zeroize（pepper 是 secret，但既然存内存就放心持有）
	p := make([]byte, len(pepper))
	copy(p, pepper)
	return &PostgresService{
		pool:    pool,
		queries: db.New(pool),
		pepper:  p,
		log:     log,
	}
}

// =============================================================================
// Create
// =============================================================================

// tokenPlaintextBytes 32 字节 CSPRNG 长度（决策 D1）；base64url 编码 ≈ 43 字符。
const tokenPlaintextBytes = 32

func (s *PostgresService) Create(ctx context.Context, params CreateParams) (*Token, string, error) {
	if err := validateCreateParams(params); err != nil {
		return nil, "", fmt.Errorf("%w: %s", ErrInvalidParam, err.Error())
	}

	// 生成 32 字节随机 + base64url 编码作 plaintext
	plaintextBytes := make([]byte, tokenPlaintextBytes)
	if _, err := rand.Read(plaintextBytes); err != nil {
		return nil, "", fmt.Errorf("生成随机 token 失败: %w", err)
	}
	plaintext := base64.RawURLEncoding.EncodeToString(plaintextBytes)

	hashHex := s.hashToken(plaintext)

	row, err := s.queries.InsertAdminToken(ctx, db.InsertAdminTokenParams{
		TokenHash:               hashHex,
		Description:             params.Description,
		Scopes:                  params.Scopes,
		IpAllowlist:             params.AllowedCIDRs,
		SingleRechargeMax:       pgInt8(params.SingleRechargeMax),
		DailyRechargeQuotaLimit: pgInt8(params.DailyRechargeQuotaLimit),
		SingleRefundMax:         pgInt8(params.SingleRefundMax),
		DailyRefundQuotaLimit:   pgInt8(params.DailyRefundQuotaLimit),
		DailyAccountCreateLimit: pgInt4(params.DailyAccountCreateLimit),
		RequestsPerMinute:       pgInt4(params.RequestsPerMinute),
		CircuitBreakerEnabled:   params.CircuitBreakerEnabled,
		CreatedBy:               params.CreatedBy,
		ExpiresAt:               nullTime(params.ExpiresAt),
	})
	if err != nil {
		return nil, "", fmt.Errorf("InsertAdminToken 失败: %w", err)
	}

	token := insertRowToToken(row)
	// 返出的 Token 也清掉 hash，与 ValidationResult 风格一致；调用方需要 hash 时单独查
	token.TokenHash = ""
	return token, plaintext, nil
}

// =============================================================================
// ValidateByPlaintext
// =============================================================================

func (s *PostgresService) ValidateByPlaintext(ctx context.Context, plaintext string, clientIP netip.Addr) (*ValidationResult, error) {
	if plaintext == "" {
		return nil, ErrTokenNotFound
	}
	hashHex := s.hashToken(plaintext)

	row, err := s.queries.FindActiveAdminTokenByHash(ctx, hashHex)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrTokenNotFound
		}
		return nil, fmt.Errorf("FindActiveAdminTokenByHash 失败: %w", err)
	}

	token := findRowToToken(row)
	token.TokenHash = "" // 不暴露 hash 给上层

	// IP allowlist 校验（决策 D2 + Unit 2 Approach）
	if len(token.AllowedCIDRs) == 0 {
		// fail-closed：空 allowlist = 拒全部
		s.log.Warn("admin token has empty ip_allowlist; rejecting all requests",
			slog.Int64("token_id", token.ID),
		)
		return nil, ErrIPNotAllowed
	}

	if !clientIP.IsValid() {
		// 调用方传入无效 IP 视为 fail-closed
		return nil, ErrIPNotAllowed
	}

	for _, prefix := range token.AllowedCIDRs {
		if prefix.Contains(clientIP) {
			return &ValidationResult{Token: token}, nil
		}
	}
	return nil, ErrIPNotAllowed
}

// =============================================================================
// CheckScope
// =============================================================================

func (s *PostgresService) CheckScope(token *Token, requiredScope string) bool {
	if token == nil || requiredScope == "" || len(token.Scopes) == 0 {
		return false
	}
	for _, sc := range token.Scopes {
		if sc == requiredScope {
			return true
		}
	}
	return false
}

// =============================================================================
// Revoke
// =============================================================================

func (s *PostgresService) Revoke(ctx context.Context, id int64) (bool, error) {
	// 查 revoke 前状态（用于区分"首次 revoke" vs "已 revoked"）
	before, err := s.queries.FindAdminTokenByID(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, ErrTokenNotFound
		}
		return false, fmt.Errorf("FindAdminTokenByID 失败: %w", err)
	}
	alreadyRevoked := before.RevokedAt.Valid

	// 执行 revoke（COALESCE 保证 revoked_at 不被覆盖）
	row, err := s.queries.RevokeAdminToken(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// 极端情况：FindAdminTokenByID 后被外部删除（理论上 schema CASCADE 不允许，但防御）
			return false, ErrTokenNotFound
		}
		return false, fmt.Errorf("RevokeAdminToken 失败: %w", err)
	}
	_ = row // 仅 sanity
	return alreadyRevoked, nil
}

// =============================================================================
// List
// =============================================================================

func (s *PostgresService) List(ctx context.Context) ([]*Token, error) {
	rows, err := s.queries.ListActiveAdminTokens(ctx)
	if err != nil {
		return nil, fmt.Errorf("ListActiveAdminTokens 失败: %w", err)
	}
	out := make([]*Token, 0, len(rows))
	for _, r := range rows {
		t := listRowToToken(r)
		t.TokenHash = "" // 双保险（query 已不 SELECT hash）
		out = append(out, t)
	}
	return out, nil
}

// =============================================================================
// GetByID
// =============================================================================

func (s *PostgresService) GetByID(ctx context.Context, id int64) (*Token, error) {
	row, err := s.queries.FindAdminTokenByID(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrTokenNotFound
		}
		return nil, fmt.Errorf("FindAdminTokenByID 失败: %w", err)
	}
	t := byIDRowToToken(row)
	t.TokenHash = ""
	return t, nil
}

// =============================================================================
// 内部 helpers
// =============================================================================

// hashToken 算法：HMAC-SHA-256(pepper, plaintext) hex（决策 D1）。
func (s *PostgresService) hashToken(plaintext string) string {
	mac := hmac.New(sha256.New, s.pepper)
	mac.Write([]byte(plaintext))
	return hex.EncodeToString(mac.Sum(nil))
}

// validateCreateParams 入口校验；返回错误用 fmt.Errorf 即可（外层会 wrap ErrInvalidParam）。
func validateCreateParams(p CreateParams) error {
	if strings.TrimSpace(p.Description) == "" {
		return errors.New("description 不能为空")
	}
	if len(p.Scopes) == 0 {
		return errors.New("scopes 至少 1 个")
	}
	for _, sc := range p.Scopes {
		if strings.TrimSpace(sc) == "" {
			return errors.New("scopes 中含空字符串")
		}
	}
	if len(p.AllowedCIDRs) == 0 {
		return errors.New("ip_allowlist 至少 1 个 CIDR（fail-closed 决策）")
	}
	if strings.TrimSpace(p.CreatedBy) == "" {
		return errors.New("created_by 不能为空")
	}
	if p.ExpiresAt != nil && !p.ExpiresAt.After(time.Now()) {
		return errors.New("expires_at 必须晚于当前时间")
	}
	return nil
}

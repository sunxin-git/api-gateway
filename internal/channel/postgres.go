package channel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sunxin-git/api-gateway/internal/crypto"
	"github.com/sunxin-git/api-gateway/internal/db"
)

// PostgresService 是 Service 的 Postgres 实现（计划 Unit 3）。
//
// 持有 sqlc queries + crypto.Keyring（多版本 KEK）。凭据明文只在
// Create/Update（入参已是明文）与 GetCredentialsForUpstream（解密后即用即弃）的
// 最小作用域内出现；**绝不**写日志、**绝不**随视图回显。
type PostgresService struct {
	queries *db.Queries
	keyring *crypto.Keyring
	log     *slog.Logger
}

var _ Service = (*PostgresService)(nil)

// NewPostgresService 构造 service。
//
// 不接受 nil pool / nil keyring / nil log；不合法直接 panic（启动期 fail-fast）。
func NewPostgresService(pool *pgxpool.Pool, keyring *crypto.Keyring, log *slog.Logger) *PostgresService {
	if pool == nil {
		panic("channel.NewPostgresService: pool 不能为 nil")
	}
	if keyring == nil {
		panic("channel.NewPostgresService: keyring 不能为 nil")
	}
	if log == nil {
		panic("channel.NewPostgresService: log 不能为 nil")
	}
	return &PostgresService{
		queries: db.New(pool),
		keyring: keyring,
		log:     log,
	}
}

// =============================================================================
// Create / UpdateCredentials（入参已是明文 → 加密入库；视图直接掩码入参，无需解密）
// =============================================================================

func (s *PostgresService) Create(ctx context.Context, params CreateParams) (*Channel, error) {
	if strings.TrimSpace(params.Name) == "" {
		return nil, fmt.Errorf("%w: name 不能为空", ErrInvalidParam)
	}
	if strings.TrimSpace(params.ProviderType) == "" {
		return nil, fmt.Errorf("%w: provider_type 不能为空", ErrInvalidParam)
	}

	ct, keyVersion, err := s.encryptCreds(params.Credentials)
	if err != nil {
		return nil, err
	}

	restricted := params.RestrictedBusinessAccounts
	if restricted == nil {
		restricted = []string{}
	}

	row, err := s.queries.InsertChannel(ctx, db.InsertChannelParams{
		Name:                       params.Name,
		ProviderType:               params.ProviderType,
		Enabled:                    params.Enabled,
		RestrictedBusinessAccounts: restricted,
		ChannelPurpose:             pgText(params.ChannelPurpose),
		CredentialsEncrypted:       ct,
		KeyVersion:                 keyVersion,
		OtherSettings:              []byte("{}"),
	})
	if err != nil {
		return nil, fmt.Errorf("InsertChannel 失败: %w", err)
	}
	// 入参即明文，直接掩码（无需解密回读）。
	return rowToChannel(row, params.Credentials.Masked()), nil
}

func (s *PostgresService) UpdateCredentials(ctx context.Context, id int64, creds ChannelCredentials) (*Channel, error) {
	ct, keyVersion, err := s.encryptCreds(creds)
	if err != nil {
		return nil, err
	}
	row, err := s.queries.UpdateChannelCredentials(ctx, db.UpdateChannelCredentialsParams{
		CredentialsEncrypted: ct,
		KeyVersion:           keyVersion,
		ID:                   id,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrChannelNotFound
		}
		return nil, fmt.Errorf("UpdateChannelCredentials 失败: %w", err)
	}
	return rowToChannel(row, creds.Masked()), nil
}

// =============================================================================
// GetByID / List（密文 → 解密以产出掩码视图；解密失败标记「解密失败」不暴露明文）
// =============================================================================

func (s *PostgresService) GetByID(ctx context.Context, id int64) (*Channel, error) {
	row, err := s.queries.GetChannelByID(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrChannelNotFound
		}
		return nil, fmt.Errorf("GetChannelByID 失败: %w", err)
	}
	return s.rowToChannelMasked(row), nil
}

func (s *PostgresService) ListActive(ctx context.Context) ([]*Channel, error) {
	rows, err := s.queries.ListActiveChannels(ctx)
	if err != nil {
		return nil, fmt.Errorf("ListActiveChannels 失败: %w", err)
	}
	out := make([]*Channel, 0, len(rows))
	for _, r := range rows {
		out = append(out, s.rowToChannelMasked(r))
	}
	return out, nil
}

func (s *PostgresService) ListActiveByProvider(ctx context.Context, providerType string) ([]*Channel, error) {
	rows, err := s.queries.ListActiveChannelsByProvider(ctx, providerType)
	if err != nil {
		return nil, fmt.Errorf("ListActiveChannelsByProvider 失败: %w", err)
	}
	out := make([]*Channel, 0, len(rows))
	for _, r := range rows {
		out = append(out, s.rowToChannelMasked(r))
	}
	return out, nil
}

// =============================================================================
// SetEnabled / Delete
// =============================================================================

func (s *PostgresService) SetEnabled(ctx context.Context, id int64, enabled bool) error {
	_, err := s.queries.SetChannelEnabled(ctx, db.SetChannelEnabledParams{Enabled: enabled, ID: id})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrChannelNotFound
		}
		return fmt.Errorf("SetChannelEnabled 失败: %w", err)
	}
	return nil
}

func (s *PostgresService) Delete(ctx context.Context, id int64) (bool, error) {
	n, err := s.queries.DeleteChannel(ctx, id)
	if err != nil {
		return false, fmt.Errorf("DeleteChannel 失败: %w", err)
	}
	return n > 0, nil
}

// =============================================================================
// GetCredentialsForUpstream（唯一返明文；解密失败 fail-closed）
// =============================================================================

func (s *PostgresService) GetCredentialsForUpstream(ctx context.Context, id int64) (*ChannelCredentials, error) {
	row, err := s.queries.GetChannelByID(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrChannelNotFound
		}
		return nil, fmt.Errorf("GetChannelByID 失败: %w", err)
	}
	creds, err := s.decryptCreds(row)
	if err != nil {
		// fail-closed：不返明文；记 warn（不含明文 / 不含底层 GCM 细节）。
		s.log.Warn("渠道凭据解密失败（fail-closed）",
			slog.Int64("channel_id", id),
			slog.Int("key_version", int(row.KeyVersion)),
		)
		return nil, ErrDecryptFailed
	}
	return &creds, nil
}

// =============================================================================
// 内部 helpers
// =============================================================================

// encryptCreds marshal 凭据为 JSON 后整体加密；返回密文 + 所用 KEK 版本。
func (s *PostgresService) encryptCreds(creds ChannelCredentials) ([]byte, int32, error) {
	blob, err := json.Marshal(creds)
	if err != nil {
		return nil, 0, fmt.Errorf("序列化凭据失败: %w", err)
	}
	ct, ver, err := s.keyring.Encrypt(blob)
	if err != nil {
		return nil, 0, fmt.Errorf("加密凭据失败: %w", err)
	}
	return ct, ver, nil
}

// decryptCreds 解密密文 → 反序列化为 ChannelCredentials；任何失败均视为解密失败（fail-closed）。
func (s *PostgresService) decryptCreds(row db.Channel) (ChannelCredentials, error) {
	blob, err := s.keyring.Decrypt(row.CredentialsEncrypted, row.KeyVersion)
	if err != nil {
		return ChannelCredentials{}, err
	}
	var creds ChannelCredentials
	if err := json.Unmarshal(blob, &creds); err != nil {
		// 解密成功但 JSON 损坏：仍按失败处理（不返回半截明文）。
		return ChannelCredentials{}, fmt.Errorf("%w: 凭据 JSON 损坏", crypto.ErrDecryptFailed)
	}
	return creds, nil
}

// rowToChannelMasked 把 db.Channel 映射为掩码视图（解密以产出 mask；失败标记「解密失败」）。
func (s *PostgresService) rowToChannelMasked(row db.Channel) *Channel {
	creds, err := s.decryptCreds(row)
	if err != nil {
		s.log.Warn("渠道凭据解密失败（掩码视图降级为「解密失败」）",
			slog.Int64("channel_id", row.ID),
			slog.Int("key_version", int(row.KeyVersion)),
		)
		return rowToChannel(row, maskedDecryptFailed())
	}
	return rowToChannel(row, creds.Masked())
}

// rowToChannel 组装对外视图（掩码凭据由调用方传入；本函数不接触明文）。
func rowToChannel(row db.Channel, masked MaskedCredentials) *Channel {
	return &Channel{
		ID:                         row.ID,
		Name:                       row.Name,
		ProviderType:               row.ProviderType,
		Enabled:                    row.Enabled,
		RestrictedBusinessAccounts: row.RestrictedBusinessAccounts,
		ChannelPurpose:             textToString(row.ChannelPurpose),
		KeyVersion:                 row.KeyVersion,
		Credentials:                masked,
		CreatedAt:                  row.CreatedAt,
		UpdatedAt:                  row.UpdatedAt,
	}
}

func pgText(s string) pgtype.Text {
	s = strings.TrimSpace(s)
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}

func textToString(t pgtype.Text) string {
	if t.Valid {
		return t.String
	}
	return ""
}

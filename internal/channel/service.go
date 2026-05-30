package channel

import (
	"context"
	"errors"
	"time"
)

// Sentinel errors。
var (
	// ErrChannelNotFound 按 id 查无渠道。
	ErrChannelNotFound = errors.New("channel: 渠道不存在")
	// ErrInvalidParam 创建/更新参数非法。
	ErrInvalidParam = errors.New("channel: 参数非法")
	// ErrDecryptFailed 凭据解密失败（fail-closed）；上游调用方据此拒绝提交。
	ErrDecryptFailed = errors.New("channel: 凭据解密失败（fail-closed）")
)

// Channel 是渠道的对外视图：**不含明文凭据**，仅含掩码视图（MaskedCredentials）。
type Channel struct {
	ID                         int64
	Name                       string
	ProviderType               string
	Enabled                    bool
	RestrictedBusinessAccounts []string
	ChannelPurpose             string
	KeyVersion                 int32
	// Credentials 永远是掩码视图；明文只经 Service.GetCredentialsForUpstream 返回。
	Credentials MaskedCredentials
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// CreateParams 创建渠道入参（Credentials 为明文，service 内加密后入库）。
type CreateParams struct {
	Name                       string
	ProviderType               string
	Enabled                    bool
	RestrictedBusinessAccounts []string
	ChannelPurpose             string
	Credentials                ChannelCredentials
}

// Service 渠道服务接口（计划 Unit 3；DIP：上层依赖接口，便于测试注入）。
//
// 安全契约：
//   - Create / GetByID / ListActive* / UpdateCredentials 返回的 Channel **绝不**含明文凭据
//     （仅掩码视图）；
//   - 明文凭据**只**经 GetCredentialsForUpstream 返回，调用即用即弃，绝不入日志/不持久化；
//   - 解密失败一律 fail-closed：GetCredentialsForUpstream 返 ErrDecryptFailed；
//     掩码视图标记「解密失败」而不暴露任何明文。
type Service interface {
	// Create 加密 params.Credentials（用当前最高版本 KEK）并入库；返回掩码视图。
	// 错误：ErrInvalidParam / wrapped DB error（如 channel name UNIQUE 冲突）。
	Create(ctx context.Context, params CreateParams) (*Channel, error)

	// GetByID 按 id 查渠道（掩码视图）。错误：ErrChannelNotFound。
	GetByID(ctx context.Context, id int64) (*Channel, error)

	// ResolveActiveChannelID 按渠道名解析启用渠道的 id（Unit 10 提交流程：catalog 绑定名 → channel_id）。
	// 不存在 / 已停用 → ErrChannelNotFound（fail-closed）。
	ResolveActiveChannelID(ctx context.Context, name string) (int64, error)

	// ListActive 列出所有启用渠道（掩码视图）。
	ListActive(ctx context.Context) ([]*Channel, error)

	// ListActiveByProvider 按 provider_type 列出启用渠道（掩码视图；provider 工厂用）。
	ListActiveByProvider(ctx context.Context, providerType string) ([]*Channel, error)

	// UpdateCredentials 用当前最高版本 KEK 重新加密整组凭据并更新（凭据轮换 / KEK 重加密）。
	// 返回掩码视图。错误：ErrChannelNotFound。
	UpdateCredentials(ctx context.Context, id int64, creds ChannelCredentials) (*Channel, error)

	// SetEnabled 启用/停用渠道（软下线）。错误：ErrChannelNotFound。
	SetEnabled(ctx context.Context, id int64, enabled bool) error

	// Delete 硬删除渠道；返回 existed=false 表示原本不存在。
	Delete(ctx context.Context, id int64) (existed bool, err error)

	// GetCredentialsForUpstream 是**唯一**返回明文凭据的方法（adapter 注入上游用）。
	// 调用即用即弃：拿到后立即注入请求、用完丢弃引用，**绝不**入日志/不持久化。
	// 解密失败 → ErrDecryptFailed（调用方 fail-closed 拒绝提交）。
	GetCredentialsForUpstream(ctx context.Context, id int64) (*ChannelCredentials, error)
}

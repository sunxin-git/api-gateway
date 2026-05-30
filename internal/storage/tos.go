// Package storage 实现结果对象存储（Phase 2 / Unit 9：火山 TOS 转存 + 受限签名 URL）。
//
// 计划：docs/plans/2026-05-28-001-feat-async-video-relay-mvp-plan.md Unit 9
// 决策：docs/adr/0006-async-execution-asynq-redis.md 决策 3（官方 SDK ve-tos-golang-sdk/v2）
//
// 抽象（DIP）：ObjectStore 接口对上游隐藏 TOS SDK；task 包 store worker 依赖接口（测试用 fake），
// 生产用 TOSObjectStore（SDK 实现）。凭据来自 channel（per-channel TOS AK/SK/bucket/endpoint/region），
// 由 store worker 即用即弃地构造 ObjectStore，**绝不**入日志；签名 URL **不入 audit/日志/span**。
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/volcengine/ve-tos-golang-sdk/v2/tos"
	"github.com/volcengine/ve-tos-golang-sdk/v2/tos/enum"
)

// presign TTL 边界（TOS PreSignedURL 约束：[1, 604800] 秒 = 最长 7 天）。
const (
	minPresignSeconds int64 = 1
	maxPresignSeconds int64 = 604800
)

// ErrObjectExists 表示同 key 对象已存在（ForbidOverwrite 命中 TOS 409）。
//
// 调用方（store worker）据此把「重试时对象已传过」视作幂等成功（确定性 key + ForbidOverwrite：
// 上次 Put 成功但写 meta 前崩溃/失败，重投再 Put 命中 409 → 不重传、直接补写 meta）。
var ErrObjectExists = errors.New("storage: object already exists (ForbidOverwrite)")

// ObjectStore 是结果对象存储的最小抽象（计划 §Unit 9）。
//
// 实现不可变、可安全并发（TOSObjectStore 持只读配置 + 线程安全 SDK client）。
type ObjectStore interface {
	// Put 上传对象。size 为内容字节数（精确 Content-Length）；contentType 为 MIME。
	// 实现须开启 ForbidOverwrite（防同 key 覆盖，幂等安全网）。
	Put(ctx context.Context, key string, body io.Reader, size int64, contentType string) error
	// PresignGet 生成单对象 GET 只读的受限时长签名 URL（ttl 截断到 [1s, 7d]）。
	// **本地计算，无网络调用**；返回的 URL 含签名秘密，**绝不**入日志/审计。
	PresignGet(key string, ttl time.Duration) (string, error)
	// Bucket / Region / Endpoint 暴露绑定配置（供 store worker 记录 oss_object_meta）。
	Bucket() string
	Region() string
	Endpoint() string
}

// TOSConfig 是构造 TOSObjectStore 的 per-channel 配置（来自解密后的 ChannelCredentials）。
//
// 即用即弃：store worker 每任务从 channel 凭据构造，用完丢弃；AccessKey/SecretKey **绝不**入日志。
type TOSConfig struct {
	AccessKey string
	SecretKey string
	Bucket    string
	Endpoint  string
	Region    string
}

// TOSObjectStore 是 ObjectStore 的火山 TOS SDK 实现（ADR-0006 决策 3）。
type TOSObjectStore struct {
	cli    *tos.ClientV2
	bucket string
	region string
	// endpoint 仅记录用（client 已持有；保留以填 oss_object_meta）。
	endpoint string
}

var _ ObjectStore = (*TOSObjectStore)(nil)

// NewTOSObjectStore 构造 TOS 客户端 + fail-fast 必填校验（返回接口便于注入工厂）。
//
// NewClientV2 仅构造 client（不发网络）；endpoint 已显式给定，不触发 region 自动解析。
func NewTOSObjectStore(cfg TOSConfig) (ObjectStore, error) {
	if strings.TrimSpace(cfg.AccessKey) == "" || strings.TrimSpace(cfg.SecretKey) == "" {
		return nil, errors.New("storage: TOS AccessKey/SecretKey 不能为空（渠道未配 TOS 凭据）")
	}
	if strings.TrimSpace(cfg.Bucket) == "" {
		return nil, errors.New("storage: TOS Bucket 不能为空")
	}
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return nil, errors.New("storage: TOS Endpoint 不能为空")
	}
	if strings.TrimSpace(cfg.Region) == "" {
		return nil, errors.New("storage: TOS Region 不能为空")
	}
	cli, err := tos.NewClientV2(
		cfg.Endpoint,
		tos.WithRegion(cfg.Region),
		tos.WithCredentials(tos.NewStaticCredentials(cfg.AccessKey, cfg.SecretKey)),
	)
	if err != nil {
		return nil, fmt.Errorf("storage: 构造 TOS client 失败: %w", err)
	}
	return &TOSObjectStore{cli: cli, bucket: cfg.Bucket, region: cfg.Region, endpoint: cfg.Endpoint}, nil
}

// Put 上传对象（ForbidOverwrite 防覆盖；body 流式传给 SDK，size 作精确 Content-Length）。
func (s *TOSObjectStore) Put(ctx context.Context, key string, body io.Reader, size int64, contentType string) error {
	_, err := s.cli.PutObjectV2(ctx, &tos.PutObjectV2Input{
		PutObjectBasicInput: tos.PutObjectBasicInput{
			Bucket:          s.bucket,
			Key:             key,
			ContentLength:   size,
			ContentType:     contentType,
			ForbidOverwrite: true, // 防同 key 覆盖（幂等安全网，ADR-0006 决策 3）
		},
		Content: body,
	})
	if err != nil {
		// ForbidOverwrite 命中已存在对象 → TOS 409 → 归一为 ErrObjectExists（调用方幂等处理）。
		if tos.StatusCode(err) == http.StatusConflict {
			return ErrObjectExists
		}
		return fmt.Errorf("storage: TOS PutObjectV2 失败: %w", err)
	}
	return nil
}

// PresignGet 生成 GET 只读签名 URL（ttl 截断到 [1s, 7d]）。本地计算，无网络。
func (s *TOSObjectStore) PresignGet(key string, ttl time.Duration) (string, error) {
	out, err := s.cli.PreSignedURL(&tos.PreSignedURLInput{
		HTTPMethod: enum.HttpMethodGet,
		Bucket:     s.bucket,
		Key:        key,
		Expires:    clampPresignSeconds(ttl),
	})
	if err != nil {
		return "", fmt.Errorf("storage: TOS PreSignedURL 失败: %w", err)
	}
	return out.SignedUrl, nil
}

func (s *TOSObjectStore) Bucket() string   { return s.bucket }
func (s *TOSObjectStore) Region() string   { return s.region }
func (s *TOSObjectStore) Endpoint() string { return s.endpoint }

// clampPresignSeconds 把 ttl 截断到 TOS 允许的 [1, 604800] 秒。
func clampPresignSeconds(ttl time.Duration) int64 {
	sec := int64(ttl / time.Second)
	if sec < minPresignSeconds {
		return minPresignSeconds
	}
	if sec > maxPresignSeconds {
		return maxPresignSeconds
	}
	return sec
}

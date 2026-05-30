package task

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/sunxin-git/api-gateway/internal/db"
	"github.com/sunxin-git/api-gateway/internal/ledger"
	"github.com/sunxin-git/api-gateway/internal/storage"
)

// 业务只读层（Unit 10）：业务对外 API（GET 任务 / 查余额 / 结果签名 URL）所需的读路径。
//
// 跨租户隔离硬约束（CLAUDE.md ISP / 失败优先）：按 id 查任务**必带 business_account_id 归属**
// （GetTaskForAccount），不匹配返 ErrTaskNotFound → handler 映射 404（不泄露资源存在性）。

// ErrTaskNotFound 业务按 id 查任务未命中（含跨租户归属不符）→ handler 映射 404（不可枚举）。
var ErrTaskNotFound = errors.New("task: 任务不存在或不属于该账户")

// TaskView 业务可见的任务视图（GET 响应源）。
//
// **不含**凭据 / 内部 lease / callback_token / 快照原文 / 签名 URL（签名 URL 读时现签，见 PresignResult）。
type TaskView struct {
	ID           string
	Status       db.TaskStatus
	Model        string
	TaskType     string
	ErrorCode    string
	ErrorMessage string
	SubmittedAt  time.Time
	UpdatedAt    time.Time
}

// GetForAccount 业务只读查询：强制归属（id + business_account_id），不匹配 → ErrTaskNotFound（404）。
//
// 这是跨租户隔离的权威落点（SQL WHERE business_account_id 由 GetTaskForAccount 承载）：
// A 用自己的 key 查 B 的 task_id → 0 行 → ErrTaskNotFound，不泄露 B 任务的存在性。
func (s *Service) GetForAccount(ctx context.Context, accountID, taskID string) (*TaskView, error) {
	row, err := s.q.GetTaskForAccount(ctx, db.GetTaskForAccountParams{
		ID:                taskID,
		BusinessAccountID: accountID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrTaskNotFound
		}
		return nil, fmt.Errorf("task.GetForAccount: %w", err)
	}
	return taskRowToView(row), nil
}

// taskRowToView 把 db.Task 映射为业务视图（task_type 从快照取；快照损坏不致命，留空）。
func taskRowToView(row db.Task) *TaskView {
	v := &TaskView{
		ID:           row.ID,
		Status:       row.Status,
		Model:        row.Model,
		ErrorCode:    pgTextString(row.ErrorCode),
		ErrorMessage: pgTextString(row.ErrorMessage),
		SubmittedAt:  row.SubmittedAt,
		UpdatedAt:    row.UpdatedAt,
	}
	if snap, err := ParseSnapshot(row.FinancialSnapshot); err == nil {
		v.TaskType = snap.TaskType
	}
	return v
}

// GetBalance 读账户当前余额（委托 ledger；handler 暴露「余额/用量」：available=余额、used_total=累计用量）。
func (s *Service) GetBalance(ctx context.Context, accountID string) (*ledger.Balance, error) {
	return s.ledger.GetBalance(ctx, accountID)
}

// CheckEntitlement 账户×模型授权校验（提交前置；行存在=已授权）。handler false→403。
func (s *Service) CheckEntitlement(ctx context.Context, accountID, gatewayModel string) (bool, error) {
	return s.q.CheckEntitlement(ctx, db.CheckEntitlementParams{
		BusinessAccountID: accountID,
		GatewayModel:      gatewayModel,
	})
}

// PresignResult 为已转存的任务产物现签一个受限时长 GET URL（Unit 10 GET 读路径，配合 Unit 9 转存）。
//
// **归属强制（结构性）**：用 accountID + taskID 走 GetTaskForAccount——跨租户/未知 task → ErrTaskNotFound
// （与 GetForAccount 同口径），不依赖"调用方先验归属"的约定（CLAUDE.md §四.6 显式优于隐式）。
// 返回 ""（无 error）表示「产物尚未转存 / 结果转存未启用」——业务稍后重试 GET。
//
// 安全（ADR-0006 决策 3 / 计划 R17）：签名 URL 含签名秘密，**绝不**入日志/审计/span；本方法不记 URL，
// 调用方亦不得记。bucket/region/endpoint 取自 oss_object_meta（产物实际所在），AK/SK 取自 channel 凭据
// （即用即弃），按本对象现签 → 不可越权访问其他对象。
func (s *Service) PresignResult(ctx context.Context, accountID, taskID string, ttl time.Duration) (string, error) {
	if s.objectStoreFactory == nil {
		return "", nil // 结果转存未启用
	}
	// 归属强制（结构性，不依赖调用约定）：按 (accountID, taskID) 取任务行，跨租户/未知 → ErrTaskNotFound。
	t, err := s.q.GetTaskForAccount(ctx, db.GetTaskForAccountParams{
		ID:                taskID,
		BusinessAccountID: accountID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrTaskNotFound
		}
		return "", fmt.Errorf("task.PresignResult get task: %w", err)
	}
	meta, err := s.q.GetOSSObjectMetaByTask(ctx, taskID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil // 尚未转存
		}
		return "", fmt.Errorf("task.PresignResult get meta: %w", err)
	}
	if !t.ChannelID.Valid {
		return "", fmt.Errorf("task.PresignResult: 任务无 channel_id，无法取 TOS 凭据")
	}
	cc, err := s.creds.GetCredentialsForUpstream(ctx, t.ChannelID.Int64)
	if err != nil {
		return "", fmt.Errorf("task.PresignResult creds: %w", err)
	}
	store, err := s.objectStoreFactory(storage.TOSConfig{
		AccessKey: cc.TOSAccessKey,
		SecretKey: cc.TOSSecretKey,
		Bucket:    meta.Bucket, // 对象实际所在 bucket（不取 channel 当前配置，防转存后改配置导致签错桶）
		Endpoint:  meta.Endpoint,
		Region:    meta.Region,
	})
	if err != nil {
		return "", fmt.Errorf("task.PresignResult store: %w", err)
	}
	url, err := store.PresignGet(meta.ObjectKey, ttl)
	if err != nil {
		return "", fmt.Errorf("task.PresignResult presign: %w", err) // 不含 url
	}
	return url, nil
}

// pgTextString 取 pgtype.Text 值；无效返空串。
func pgTextString(t pgtype.Text) string {
	if t.Valid {
		return t.String
	}
	return ""
}

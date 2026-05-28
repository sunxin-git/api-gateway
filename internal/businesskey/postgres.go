package businesskey

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
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sunxin-git/api-gateway/internal/db"
)

// PostgresService 是 Service 接口的 Postgres 实现（计划 Unit 2）。
//
// 持有 pgxpool + pepper bytes（HMAC key hash；与 admintoken 共享 GATEWAY_TOKEN_PEPPER，
// F-min 决策 D4）+ 异步 last_used_at flush 状态。
//
// last_used_at 异步更新策略（plan §Approach）：
//   - 鉴权热路径命中 → markTouched(key.ID) 写 sync.Map（非阻塞）
//   - 后台 goroutine 每 flushInterval 扫 sync.Map → 逐 key 调 TouchBusinessKeyLastUsed
//     （UPDATE NOW()；MVP 精度 ~5min，足够"长期未用 key"运维查询）
//   - Close 触发最终 flush + 停 goroutine
type PostgresService struct {
	pool    *pgxpool.Pool
	queries *db.Queries
	pepper  []byte // HMAC 主密钥（≥ 32 字节）；**绝不**记 log
	log     *slog.Logger

	// pendingTouches 待 flush 的 key.id 集合；sync.Map[int64]struct{}{}
	pendingTouches sync.Map

	flushInterval time.Duration
	flushStopCh   chan struct{}
	flushDoneCh   chan struct{} // nil 表示无 goroutine 启动（测试构造路径）
}

// 编译期断言实现接口。
var _ Service = (*PostgresService)(nil)

// keyPlaintextBytes 32 字节 CSPRNG 长度（与 admintoken 一致 D1）；base64url 编码 ≈ 43 字符。
const keyPlaintextBytes = 32

// defaultFlushInterval 异步 last_used_at flush 周期；MVP 5min 平衡精度 vs DB 压力。
const defaultFlushInterval = 5 * time.Minute

// NewPostgresService 构造 service 并启动 last_used_at flush goroutine。
//
// 入参：
//   - pool：pgxpool.Pool（启动时 fail-fast Ping 过的）
//   - pepper：HMAC 主密钥原始字节（≥ 32 字节）；通常从 config.TokenPepperBytes 注入
//     （F-min 决策 D4：与 admintoken 共享同 pepper）
//   - log：slog logger，不能为 nil
//
// 不接受 nil pool / nil log / pepper < 32 字节；不合法直接 panic（启动期 fail-fast）。
// 返回的 service 必须在进程退出时调 Close（触发最终 flush + 停 goroutine）。
func NewPostgresService(pool *pgxpool.Pool, pepper []byte, log *slog.Logger) *PostgresService {
	s := newPostgresServiceBase(pool, pepper, log)
	s.flushDoneCh = make(chan struct{})
	go s.flushLoop()
	return s
}

// newPostgresServiceBase 构造 service 但**不**启动 flush goroutine。
//
// 仅供包内测试用：避免 flush goroutine 与测试主线对 flushInterval 等字段 race（与
// admintoken InProcessRPM 同思路）。测试需要 flush 时显式调 s.flushOnce(ctx)。
// flushDoneCh 留 nil；Close 检测 nil 跳过等待。
func newPostgresServiceBase(pool *pgxpool.Pool, pepper []byte, log *slog.Logger) *PostgresService {
	if pool == nil {
		panic("businesskey.NewPostgresService: pool 不能为 nil")
	}
	if len(pepper) < 32 {
		panic(fmt.Sprintf("businesskey.NewPostgresService: pepper 长度 %d < 32 字节（与 admintoken D1 同要求）", len(pepper)))
	}
	if log == nil {
		panic("businesskey.NewPostgresService: log 不能为 nil")
	}
	// 复制 pepper 避免外部 zeroize
	p := make([]byte, len(pepper))
	copy(p, pepper)
	return &PostgresService{
		pool:          pool,
		queries:       db.New(pool),
		pepper:        p,
		log:           log,
		flushInterval: defaultFlushInterval,
		flushStopCh:   make(chan struct{}),
		// flushDoneCh 不分配；NewPostgresService 启动 goroutine 时才分配
	}
}

// Close 触发最终 flush + 停 goroutine；幂等。
func (s *PostgresService) Close() error {
	// 安全幂等关 channel
	select {
	case <-s.flushStopCh:
		// 已 close
	default:
		close(s.flushStopCh)
	}
	if s.flushDoneCh != nil {
		<-s.flushDoneCh
	}
	// 最终 flush（即使 goroutine 已退出也执行；测试路径无 goroutine 但仍需手动 flush）
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.flushOnce(ctx)
	return nil
}

// =============================================================================
// Create
// =============================================================================

func (s *PostgresService) Create(ctx context.Context, params CreateParams) (*Key, string, error) {
	if err := validateCreateParams(params); err != nil {
		return nil, "", fmt.Errorf("%w: %s", ErrInvalidParam, err.Error())
	}

	// 32 字节 CSPRNG → base64url
	plaintextBytes := make([]byte, keyPlaintextBytes)
	if _, err := rand.Read(plaintextBytes); err != nil {
		return nil, "", fmt.Errorf("生成随机 key 失败: %w", err)
	}
	plaintext := base64.RawURLEncoding.EncodeToString(plaintextBytes)

	hashHex := s.hashKey(plaintext)

	row, err := s.queries.InsertBusinessKey(ctx, db.InsertBusinessKeyParams{
		BusinessAccountID: params.BusinessAccountID,
		Description:       params.Description,
		KeyHash:           hashHex,
		RequestsPerMinute: pgInt4(params.RequestsPerMinute),
		CreatedBy:         params.CreatedBy,
	})
	if err != nil {
		// FK 违反（business_account 不存在）→ PG 23503；保留 wrap 便于上层判
		return nil, "", fmt.Errorf("InsertBusinessKey 失败: %w", err)
	}

	key := modelToKey(row)
	key.KeyHash = "" // 不暴露 hash
	return key, plaintext, nil
}

// =============================================================================
// ValidateByPlaintext
// =============================================================================

func (s *PostgresService) ValidateByPlaintext(ctx context.Context, plaintext string) (*ValidationResult, error) {
	if plaintext == "" {
		return nil, ErrKeyNotFound
	}
	hashHex := s.hashKey(plaintext)

	row, err := s.queries.FindActiveBusinessKeyByHash(ctx, hashHex)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrKeyNotFound
		}
		return nil, fmt.Errorf("FindActiveBusinessKeyByHash 失败: %w", err)
	}

	key := modelToKey(row)
	key.KeyHash = "" // 不暴露给上层

	// 异步标记 last_used_at（不阻塞热路径）
	s.markTouched(key.ID)

	return &ValidationResult{Key: key}, nil
}

// =============================================================================
// Revoke
// =============================================================================

func (s *PostgresService) Revoke(ctx context.Context, id int64) (bool, error) {
	// 先查原状态（用于区分"首次 revoke" vs "已 revoked"）
	before, err := s.queries.FindBusinessKeyByID(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, ErrKeyNotFound
		}
		return false, fmt.Errorf("FindBusinessKeyByID 失败: %w", err)
	}
	alreadyRevoked := before.RevokedAt.Valid

	row, err := s.queries.RevokeBusinessKey(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, ErrKeyNotFound
		}
		return false, fmt.Errorf("RevokeBusinessKey 失败: %w", err)
	}
	_ = row
	return alreadyRevoked, nil
}

// =============================================================================
// List
// =============================================================================

func (s *PostgresService) ListByAccount(ctx context.Context, businessAccountID string) ([]*Key, error) {
	rows, err := s.queries.ListActiveBusinessKeysByAccount(ctx, businessAccountID)
	if err != nil {
		return nil, fmt.Errorf("ListActiveBusinessKeysByAccount 失败: %w", err)
	}
	out := make([]*Key, 0, len(rows))
	for _, r := range rows {
		out = append(out, listByAccountRowToKey(r))
	}
	return out, nil
}

func (s *PostgresService) ListAll(ctx context.Context) ([]*Key, error) {
	rows, err := s.queries.ListAllActiveBusinessKeys(ctx)
	if err != nil {
		return nil, fmt.Errorf("ListAllActiveBusinessKeys 失败: %w", err)
	}
	out := make([]*Key, 0, len(rows))
	for _, r := range rows {
		out = append(out, listAllRowToKey(r))
	}
	return out, nil
}

// =============================================================================
// GetByID
// =============================================================================

func (s *PostgresService) GetByID(ctx context.Context, id int64) (*Key, error) {
	row, err := s.queries.FindBusinessKeyByID(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrKeyNotFound
		}
		return nil, fmt.Errorf("FindBusinessKeyByID 失败: %w", err)
	}
	k := modelToKey(row)
	k.KeyHash = ""
	return k, nil
}

// =============================================================================
// TouchLastUsed + 异步 flush
// =============================================================================

// TouchLastUsed 同步更新单 key 的 last_used_at = NOW()；best-effort。
//
// 通常**不**直接调用；ValidateByPlaintext 命中后会自动 markTouched + 异步 flush。
// 暴露此方法给：(a) 测试手动验证 flush 写入；(b) 极端场景手工 touch。
func (s *PostgresService) TouchLastUsed(ctx context.Context, id int64) error {
	if err := s.queries.TouchBusinessKeyLastUsed(ctx, id); err != nil {
		return fmt.Errorf("TouchBusinessKeyLastUsed 失败: %w", err)
	}
	return nil
}

// markTouched 把 key.ID 加到 pendingTouches（非阻塞 sync.Map）。
//
// 调用方：ValidateByPlaintext 成功后调用。
// 实际 DB UPDATE 由后台 flush goroutine 每 flushInterval 扫描执行。
func (s *PostgresService) markTouched(id int64) {
	s.pendingTouches.Store(id, struct{}{})
}

// flushLoop 后台周期性 flush；defer close(flushDoneCh) 让 Close 可等待退出。
func (s *PostgresService) flushLoop() {
	defer close(s.flushDoneCh)
	ticker := time.NewTicker(s.flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.flushStopCh:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			s.flushOnce(ctx)
			cancel()
		}
	}
}

// flushOnce 扫描 pendingTouches → 逐 key UPDATE；失败仅 warn 不返错。
//
// 测试可直接调用以验证 flush 行为。
//
// 设计选择：逐 key UPDATE 而非批量 SQL（IN (...)）—— 当前 key 数 ≤ 几十；性能足够，
// 且单 row UPDATE 失败不影响其他 row（partial flush 容错）。
func (s *PostgresService) flushOnce(ctx context.Context) {
	var touched, failed int
	s.pendingTouches.Range(func(key, _ any) bool {
		id, ok := key.(int64)
		if !ok {
			return true
		}
		// 先从 map 中 delete，避免 flush 期间被 markTouched 二次添加导致循环
		// （Range 已声明可在迭代时 Store/Delete 同 key，但用 Delete 后再 UPDATE
		//  保证至少触发一次 UPDATE；若 UPDATE 期间被 markTouched 再次，下轮 flush 处理）
		s.pendingTouches.Delete(id)
		if err := s.queries.TouchBusinessKeyLastUsed(ctx, id); err != nil {
			failed++
			s.log.Warn("flush last_used_at 失败（best-effort，不影响鉴权主路径）",
				slog.Int64("key_id", id),
				slog.String("err", err.Error()),
			)
			return true
		}
		touched++
		return true
	})
	if touched > 0 || failed > 0 {
		s.log.Debug("businesskey last_used_at flushed",
			slog.Int("touched", touched),
			slog.Int("failed", failed),
		)
	}
}

// =============================================================================
// 内部 helpers
// =============================================================================

// hashKey 算法：HMAC-SHA-256(pepper, plaintext) hex（F-min D4：与 admintoken 共享 pepper + 同算法）。
func (s *PostgresService) hashKey(plaintext string) string {
	mac := hmac.New(sha256.New, s.pepper)
	mac.Write([]byte(plaintext))
	return hex.EncodeToString(mac.Sum(nil))
}

// validateCreateParams 入口校验；外层会 wrap ErrInvalidParam。
func validateCreateParams(p CreateParams) error {
	if strings.TrimSpace(p.BusinessAccountID) == "" {
		return errors.New("business_account_id 不能为空")
	}
	if strings.TrimSpace(p.Description) == "" {
		return errors.New("description 不能为空")
	}
	if strings.TrimSpace(p.CreatedBy) == "" {
		return errors.New("created_by 不能为空")
	}
	if p.RequestsPerMinute != nil && *p.RequestsPerMinute <= 0 {
		return errors.New("requests_per_minute 必须 > 0；NULL = 不限速")
	}
	return nil
}

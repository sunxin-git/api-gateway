package task

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sunxin-git/api-gateway/internal/db"
	"github.com/sunxin-git/api-gateway/internal/relay/video"
	"github.com/sunxin-git/api-gateway/internal/storage"
)

// 结果转存（Unit 9）：成功结算后下载上游产物 → 上传企业 TOS → 写 oss_object_meta。
// 独立 video:store job（非 settle 内联）：多 MB 下载+上传不阻塞动钱的 settle 关键队列。
// 签名 URL 读时（Unit 10 GET）按 meta 现签，不在此生成 / 不落库（含签名秘密）。

// defaultResultContentType 上游未给 Content-Type 时的兜底（seedance text_to_video 产物为 mp4）。
const defaultResultContentType = "video/mp4"

// settleSucceeded 推进 SETTLED 终态成功后触发结果转存（Unit 9）；commit 成功路径专用。
func (s *Service) settleSucceeded(taskID string) error {
	if err := s.casSettled(taskID); err != nil {
		return err
	}
	s.enqueueStoreBestEffort(taskID)
	return nil
}

// enqueueStoreBestEffort 触发结果转存（best-effort：入队失败仅记录，6b recoverMissingStore 兜底）。
// objectStoreFactory 未配（结果转存禁用）→ no-op。
func (s *Service) enqueueStoreBestEffort(taskID string) {
	if s.objectStoreFactory == nil {
		return
	}
	if err := s.enqueuer.EnqueueStore(context.Background(), taskID); err != nil {
		s.logger.Error("task: 入队 store 失败；结果转存延迟（待 recoverMissingStore 兜底）",
			slog.String("task_id", taskID), slog.String("err", err.Error()))
	}
}

// storeResult 转存一个 COMPLETED 来源已结算任务的产物（store worker / 恢复 sweep 调用）。
//
// 幂等：已有 oss_object_meta → 跳过；确定性 object key + ForbidOverwrite + ON CONFLICT 三重幂等。
// 失败分类（金钱已结算，转存失败不影响账目）：
//   - 瞬时（DB / Poll / fetch 网络 / TOS 网络）→ 返 error 让 asynq 重试。
//   - 永久（无 TOS 凭据 / catalog 缺 / 无 upstream_task_id / 上游无产物 URL / URL 失效 / 产物超限 /
//     快照损坏）→ 返 nil + 告警转人工对账（不无限重试）。
func (s *Service) storeResult(ctx context.Context, taskID string) error {
	if s.objectStoreFactory == nil {
		return nil // 结果转存未启用
	}
	t, err := s.q.GetTaskByID(ctx, taskID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("store get task: %w", err)
	}
	// 仅 COMPLETED 来源（error_code 空）的已结算任务有产物。
	if t.Status != db.TaskStatusSETTLED || (t.ErrorCode.Valid && t.ErrorCode.String != "") {
		return nil
	}
	// 超 24h URL 有效窗（ADR-0006：seedance video_url 仅 24h）：产物 URL 已失效，再 Poll/fetch 无望 →
	// 永久失败转人工对账（防瞬时错误在窗口边界附近无限重试成毒丸，ce-review reliability）。
	if t.TerminalAt.Valid && time.Since(t.TerminalAt.Time) > resultURLValidWindow {
		s.logger.Error("store: 超 24h URL 有效窗，产物 URL 已失效（人工对账）",
			slog.String("task_id", taskID))
		return nil
	}
	// 幂等：已转存 → 跳过。
	if _, err := s.q.GetOSSObjectMetaByTask(ctx, taskID); err == nil {
		return nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("store check meta: %w", err)
	}

	snap, err := ParseSnapshot(t.FinancialSnapshot)
	if err != nil {
		s.logger.Error("store: 快照损坏，无法转存（人工对账）", slog.String("task_id", taskID), slog.String("err", err.Error()))
		return nil
	}
	if !t.ChannelID.Valid {
		s.logger.Error("store: 任务无 channel_id，无法取 TOS 凭据（人工对账）", slog.String("task_id", taskID))
		return nil
	}
	cc, err := s.creds.GetCredentialsForUpstream(ctx, t.ChannelID.Int64)
	if err != nil {
		return fmt.Errorf("store creds: %w", err) // 解密/DB 瞬时 → 重试
	}
	store, err := s.objectStoreFactory(storage.TOSConfig{
		AccessKey: cc.TOSAccessKey, SecretKey: cc.TOSSecretKey,
		Bucket: cc.TOSBucket, Endpoint: cc.TOSEndpoint, Region: cc.TOSRegion,
	})
	if err != nil {
		s.logger.Error("store: 渠道 TOS 凭据不全/非法，无法转存（人工对账）",
			slog.String("task_id", taskID), slog.String("err", err.Error()))
		return nil // 配置问题 → 不重试
	}
	entry, ok := s.catalog.Lookup(snap.GatewayModel)
	if !ok || entry == nil {
		s.logger.Error("store: catalog 未命中（人工对账）", slog.String("task_id", taskID))
		return nil
	}
	if !t.UpstreamTaskID.Valid || t.UpstreamTaskID.String == "" {
		s.logger.Error("store: 无 upstream_task_id（人工对账）", slog.String("task_id", taskID))
		return nil
	}

	// 再 Poll 上游取产物 URL（从 task 行重建，不依赖 settle 时 URL；可恢复）。
	resURL, err := s.pollResultURL(ctx, entry, video.UpstreamCredentials{APIKey: cc.APIKey}, t.UpstreamTaskID.String)
	if err != nil {
		s.logger.Warn("store: Poll 取产物 URL 失败（重试）", slog.String("task_id", taskID), slog.String("err", err.Error()))
		return fmt.Errorf("store poll: %w", err) // 瞬时 → 重试
	}
	if resURL == "" {
		s.logger.Error("store: 上游无产物 URL（人工对账）", slog.String("task_id", taskID))
		return nil // 永久 → 不重试
	}
	// SSRF 防护：产物 URL 由上游（半可信）返回，转存前校验 scheme + 拒解析到内网/环回地址
	//（防被劫持的上游用回调把网关当跳板打内网 / 元数据端点）。拒绝是永久失败（恶意/误配 URL 不会自愈）。
	if err := s.validateResultURL(resURL); err != nil {
		s.logger.Error("store: 产物 URL 未通过 SSRF 校验（人工对账）",
			slog.String("task_id", taskID), slog.String("err", err.Error()))
		return nil
	}

	// 下载产物（带超时 + 大小上限）。
	body, size, contentType, retryable, err := s.fetchResult(ctx, resURL)
	if err != nil {
		if retryable {
			s.logger.Warn("store: 下载产物失败（重试）", slog.String("task_id", taskID), slog.String("err", err.Error()))
			return fmt.Errorf("store fetch: %w", err)
		}
		s.logger.Error("store: 下载产物永久失败（人工对账：URL 失效 / 超限 / 缺 Content-Length）",
			slog.String("task_id", taskID), slog.String("err", err.Error()))
		return nil
	}
	defer func() { _ = body.Close() }()

	objectKey := buildResultObjectKey(cc.ProjectID, taskID)
	if err := store.Put(ctx, objectKey, body, size, contentType); err != nil {
		if !errors.Is(err, storage.ErrObjectExists) {
			s.logger.Warn("store: TOS 上传失败（重试）", slog.String("task_id", taskID), slog.String("err", err.Error()))
			return fmt.Errorf("store put: %w", err) // 瞬时 → 重试
		}
		// 对象已存在（上次传成功但写 meta 前失败）→ 确定性 key 幂等，续写 meta。
		// **TOS 单次 PUT 原子**（同 S3）：被取消的 PUT 不产生对象，故 ErrObjectExists ⟹ 上次是一次
		// **完整**上传（绝非残片），续写 meta 安全（ce-review reliability：无「残片记成完整」风险）。
		s.logger.Info("store: 对象已存在（确定性 key 幂等），续写 meta", slog.String("task_id", taskID))
	}

	if _, err := s.q.InsertOSSObjectMeta(ctx, db.InsertOSSObjectMetaParams{
		TaskID:            taskID,
		BusinessAccountID: t.BusinessAccountID,
		Bucket:            store.Bucket(),
		ObjectKey:         objectKey,
		Region:            store.Region(),
		Endpoint:          store.Endpoint(),
		ContentType:       contentType,
		SizeBytes:         size,
	}); err != nil {
		return fmt.Errorf("store insert meta: %w", err) // 瞬时 DB → 重试（对象已传，再投命中 ErrObjectExists 安全）
	}
	s.logger.Info("store: 产物转存 TOS 完成", slog.String("task_id", taskID), slog.Int64("size_bytes", size))
	return nil
}

// pollResultURL settle 外独立 ctx Poll 取产物 URL。
func (s *Service) pollResultURL(ctx context.Context, entry *video.VideoModelEntry, creds video.UpstreamCredentials, upstreamTaskID string) (string, error) {
	pollCtx, cancel := context.WithTimeout(ctx, s.pollTO)
	defer cancel()
	res, err := s.adapter.Poll(pollCtx, entry, creds, upstreamTaskID)
	if err != nil {
		return "", err
	}
	return res.ResultURL, nil
}

// fetchResult 下载上游产物。返回 (body, size, contentType, retryable, err)；body 由调用方 Close。
//
// retryable=true：瞬时网络 / 5xx；false：永久（4xx URL 失效 / 缺 Content-Length / 超 maxResultBytes）。
// body 经 LimitReader 封顶 size 字节（防 Content-Length 谎报后写入超量）。
func (s *Service) fetchResult(ctx context.Context, url string) (io.ReadCloser, int64, string, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, "", false, err // URL 非法 → 永久
	}
	resp, err := s.resultHTTPClient.Do(req)
	if err != nil {
		return nil, 0, "", true, err // 网络 / 超时 → 瞬时
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		// 5xx + 429（限流）瞬时可重试；其余 4xx（404 URL 失效 / 403 鉴权 等）永久。
		retryable := resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests
		return nil, 0, "", retryable, fmt.Errorf("上游产物 GET 返回 %d", resp.StatusCode)
	}
	size := resp.ContentLength
	if size <= 0 {
		_ = resp.Body.Close()
		return nil, 0, "", false, errors.New("上游产物缺 Content-Length（无法确定大小）")
	}
	if size > s.maxResultBytes {
		_ = resp.Body.Close()
		return nil, 0, "", false, fmt.Errorf("产物大小 %d 超上限 %d", size, s.maxResultBytes)
	}
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = defaultResultContentType
	}
	rc := struct {
		io.Reader
		io.Closer
	}{io.LimitReader(resp.Body, size), resp.Body}
	return rc, size, contentType, false, nil
}

// validateResultURL SSRF 防护：校验产物 URL scheme 合法 + host 不解析到内网/环回/链路本地地址。
//
// allowPrivateResultHost=true（仅测试，因 httptest 走 127.0.0.1）时跳过私网检查。
// **残留**：先解析后由 http client 重新解析连接，存在 DNS rebinding 窗口（半可信上游下可接受，
// 生产可加 DNS pinning 强化）。
func (s *Service) validateResultURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("产物 URL 解析失败: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("产物 URL scheme 必须 http|https（当前 %q）", u.Scheme)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("产物 URL 缺 host")
	}
	if s.allowPrivateResultHost {
		return nil
	}
	ips, err := net.LookupIP(u.Hostname())
	if err != nil {
		return fmt.Errorf("解析产物 URL host 失败: %w", err)
	}
	for _, ip := range ips {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
			return fmt.Errorf("产物 URL host 解析到内网/环回地址（SSRF 防护拒绝）")
		}
	}
	return nil
}

// buildResultObjectKey 生成 TOS 对象 key（确定性 + 不可枚举 + 防注入）。
//
// key = "{projectID}/video/{taskID}.mp4"（projectID 净化后为空则省略前缀）。task_id（vtask_+128bit
// 随机，由网关生成，字符集安全）提供不可枚举随机段；确定性使重投 Put 命中 ForbidOverwrite 幂等。
// projectID 来自渠道凭据（运维设），经 sanitizeKeySegment 净化防路径穿越（'../'）/ 注入。
func buildResultObjectKey(projectID, taskID string) string {
	key := "video/" + taskID + ".mp4"
	if p := sanitizeKeySegment(projectID); p != "" {
		key = p + "/" + key
	}
	return key
}

// sanitizeKeySegment 把不可信段净化为安全的 TOS key 段：仅留 [A-Za-z0-9_-]，剔除 '/'、'.'、空白、
// 控制符等（杜绝 '../' 路径穿越与跨 project 越界）。全被剔除则返空（调用方省略该前缀）。
func sanitizeKeySegment(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '-' || r == '_',
			r >= '0' && r <= '9',
			r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		}
	}
	return b.String()
}

// recoverMissingStore（6b fetch reconciler ④）：扫 COMPLETED 来源已结算但超阈值仍无 oss_object_meta
// 的任务（store job 丢失 / 入队失败）→ 幂等重投 store。仅 24h URL 窗内（超窗转存无望，转人工对账）。
func (s *Service) recoverMissingStore(ctx context.Context) error {
	if s.objectStoreFactory == nil {
		return nil // 结果转存未启用
	}
	now := time.Now()
	rows, err := s.q.ScanSettledNeedingStore(ctx, db.ScanSettledNeedingStoreParams{
		UrlValidAfter: nullTime(now.Add(-resultURLValidWindow)),
		StaleBefore:   now.Add(-s.storeNeedingStoreAge),
		BatchSize:     s.sweepBatchSize,
	})
	if err != nil {
		return err
	}
	for _, t := range rows {
		if err := s.enqueuer.EnqueueStore(ctx, t.ID); err != nil {
			s.logger.Error("fetch reconciler: 重投 store 失败（下一轮重试）",
				slog.String("task_id", t.ID), slog.String("err", err.Error()))
			continue
		}
		s.logger.Warn("fetch reconciler: SETTLED 无 oss_object_meta（store 丢失）→ 重投 store",
			slog.String("task_id", t.ID))
	}
	return nil
}

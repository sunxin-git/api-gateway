package video

import "strings"

// =============================================================================
// 上游状态多别名收敛 + 响应解析（计划 §Unit 5：upstream_status.go）
//
// 与 seedance wire 形态（seedanceSubmitResponse / seedanceGetResponse）colocate，便于
// 一处看清「上游返回什么 → 规范化成什么」。HTTP 机制在 seedance_adapter.go。
// =============================================================================

// 上游状态别名集（小写比较）。参照生产参考实现 storyboard-assistant 的别名集 + ADR-0006
// 官方 6 态（queued/running/cancelled/succeeded/failed/expired）。
//
// **Running 不列举**：除下列终态别名外一律归 UpstreamRunning（含 queued/running/pending/
// processing/in_progress 及未知/空），不提前终态（fail-safe，由 Unit 6 expire 兜底）。
var (
	upstreamSucceededAliases = map[string]struct{}{
		"succeeded": {}, "success": {}, "completed": {}, "complete": {}, "done": {},
	}
	upstreamFailedAliases = map[string]struct{}{
		"failed": {}, "fail": {}, "error": {},
	}
	upstreamCancelledAliases = map[string]struct{}{
		"cancelled": {}, "canceled": {},
	}
	// 仅官方 6 态的 "expired"（ce-review：不收 "timeout"/"timed_out" 等投机别名——非 ADR-0006
	// 官方态，且 "timeout" 语义易与请求超时混淆；未知态归 Running 由 expire worker 兜底更安全）。
	upstreamExpiredAliases = map[string]struct{}{
		"expired": {},
	}
)

// normalizeUpstreamStatus 把上游原始 status 字符串收敛到 UpstreamStatus 5 值。
//
// 未知 / 空 → UpstreamRunning（不提前终态；继续轮询，超期由 Unit 6 expire worker 兜底）。
func normalizeUpstreamStatus(raw string) UpstreamStatus {
	s := strings.ToLower(strings.TrimSpace(raw))
	switch {
	case has(upstreamSucceededAliases, s):
		return UpstreamSucceeded
	case has(upstreamFailedAliases, s):
		return UpstreamFailed
	case has(upstreamCancelledAliases, s):
		return UpstreamCancelled
	case has(upstreamExpiredAliases, s):
		return UpstreamExpired
	default:
		return UpstreamRunning
	}
}

func has(set map[string]struct{}, k string) bool {
	_, ok := set[k]
	return ok
}

// =============================================================================
// seedance wire 结构 + 规范化
// =============================================================================

// seedanceSubmitResponse 是 `POST /contents/generations/tasks` 的响应（创建任务）。
//
// Ark 返回 {"id":"cgt-..."}；兼容 task_id 别名（参考实现 _extract_task_id 三键 task_id/request_id/id）。
type seedanceSubmitResponse struct {
	ID     string `json:"id"`
	TaskID string `json:"task_id"`
}

// taskID 取上游 task_id（id 优先，回退 task_id）；都空返空串（调用方判畸形）。
func (r seedanceSubmitResponse) taskID() string {
	if id := strings.TrimSpace(r.ID); id != "" {
		return id
	}
	return strings.TrimSpace(r.TaskID)
}

// seedanceGetResponse 是 `GET /contents/generations/tasks/{id}` 的响应（查询任务）。
//
// 形态（ADR-0006 + 参考实现）：顶层 status；content.video_url（mp4，24h 有效）；
// usage.completion_tokens（视频模型 total_tokens == completion_tokens）；error.message 失败原因。
type seedanceGetResponse struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Content struct {
		VideoURL string `json:"video_url"`
	} `json:"content"`
	Usage *struct {
		CompletionTokens int64 `json:"completion_tokens"`
		TotalTokens      int64 `json:"total_tokens"`
	} `json:"usage"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// toPollResult 把上游响应规范化为 PollResult。
//
//   - status：normalizeUpstreamStatus 收敛。
//   - Usage：仅 Succeeded 且 usage 存在且 completion_tokens > 0 时非 nil；否则 nil（交 settle 兜底）。
//   - ResultURL：仅 Succeeded 时取 content.video_url（空属异常，原样返回交 Unit 6/9 对账）。
//   - FailureMessage：非成功终态时取 error.message（供 audit/告警）。
func (r seedanceGetResponse) toPollResult() *PollResult {
	status := normalizeUpstreamStatus(r.Status)
	res := &PollResult{Status: status}

	switch status {
	case UpstreamSucceeded:
		res.ResultURL = strings.TrimSpace(r.Content.VideoURL)
		// usage 缺失 / completion_tokens ≤ 0 → 视为缺 usage（nil），不猜扣额（ADR-0006）。
		if r.Usage != nil && r.Usage.CompletionTokens > 0 {
			res.Usage = &UpstreamUsage{CompletionTokens: r.Usage.CompletionTokens}
		}
	case UpstreamFailed, UpstreamCancelled, UpstreamExpired:
		if r.Error != nil {
			res.FailureMessage = strings.TrimSpace(r.Error.Message)
			res.FailureCode = strings.TrimSpace(r.Error.Code)
		}
	}
	return res
}

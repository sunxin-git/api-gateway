package video

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// SeedanceAdapter 火山方舟 seedance 异步视频 adapter（计划 §Unit 5）。
//
// OpenAI **不**兼容形态：`POST /contents/generations/tasks` 提交、`GET .../{id}` 查询；
// body 用 content[] 文本（非 messages）。与同步 relay 的 OpenAICompatAdapter 平行不复用。
type SeedanceAdapter struct {
	client *http.Client
}

// 编译期断言实现接口。
var _ AsyncProviderAdapter = (*SeedanceAdapter)(nil)

// NewSeedanceAdapter 构造 adapter；HTTP 客户端由调用方注入（共享单实例）。
// 单次请求超时由调用方经 ctx deadline 控制（Submit/Poll 各自短超时）。
//
// 注入 client 的装配方（Unit 10 main.go）建议设：
//   - Timeout：兜底总超时（即便某次调用忘传 ctx deadline 也不无限挂起）。
//   - CheckRedirect：禁止/限制重定向（上游被劫持/DNS 污染时防二次 SSRF 到内网；
//     上游为可信 Ark endpoint，但 defense-in-depth）。
func NewSeedanceAdapter(client *http.Client) *SeedanceAdapter {
	if client == nil {
		panic("video.NewSeedanceAdapter: client 不能为 nil")
	}
	return &SeedanceAdapter{client: client}
}

const (
	// seedanceCreateTaskPath 创建任务子路径（Ark）。
	seedanceCreateTaskPath = "/contents/generations/tasks"
	// seedanceGetTaskPathFmt 查询任务子路径模板（%s = 上游 task_id）。
	seedanceGetTaskPathFmt = "/contents/generations/tasks/%s"
	// maxUpstreamRespBytes 上游响应 body 读取上限（视频任务响应均 < 1 MiB，留余量防恶意巨响应）。
	maxUpstreamRespBytes = 4 * 1024 * 1024
	// errBodySnippetMax 错误响应 body 截断长度（入 error 供排查；上游侧消息，非我方敏感内容）。
	errBodySnippetMax = 256
)

// Submit 实现 AsyncProviderAdapter（提交生成任务）。
func (a *SeedanceAdapter) Submit(
	ctx context.Context,
	entry *VideoModelEntry,
	creds UpstreamCredentials,
	req *ValidatedRequest,
	callbackURL string,
) (string, error) {
	if entry == nil || req == nil {
		return "", errors.New("video.SeedanceAdapter.Submit: entry / req 不能为 nil")
	}
	if strings.TrimSpace(creds.APIKey) == "" {
		// fail-closed：空凭据（解密失败/未配）不发空 Bearer 给上游（否则 401 被误判 Rejected）。
		return "", errors.New("video.SeedanceAdapter.Submit: creds.APIKey 不能为空（凭据缺失/解密失败）")
	}

	bodyBytes, err := json.Marshal(buildSeedanceSubmitBody(entry.UpstreamModelName, req, callbackURL))
	if err != nil {
		return "", fmt.Errorf("video.SeedanceAdapter.Submit: 构造上游 body 失败: %w", err)
	}

	endpoint := strings.TrimRight(entry.UpstreamBaseURL, "/") + seedanceCreateTaskPath
	statusCode, respBody, err := a.doRequest(ctx, http.MethodPost, endpoint, creds.APIKey, bodyBytes)
	if err != nil {
		return "", err
	}
	if err := classifyUpstreamStatusCode(statusCode, respBody); err != nil {
		return "", err
	}

	var parsed seedanceSubmitResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("%w: submit 响应非合法 JSON: %v", ErrUpstreamMalformed, err)
	}
	taskID := parsed.taskID()
	if taskID == "" {
		return "", fmt.Errorf("%w: submit 响应缺 task_id", ErrUpstreamMalformed)
	}
	return taskID, nil
}

// Poll 实现 AsyncProviderAdapter（按上游 task_id 查询）。
func (a *SeedanceAdapter) Poll(
	ctx context.Context,
	entry *VideoModelEntry,
	creds UpstreamCredentials,
	upstreamTaskID string,
) (*PollResult, error) {
	if entry == nil {
		return nil, errors.New("video.SeedanceAdapter.Poll: entry 不能为 nil")
	}
	if strings.TrimSpace(upstreamTaskID) == "" {
		return nil, errors.New("video.SeedanceAdapter.Poll: upstreamTaskID 不能为空")
	}
	if strings.TrimSpace(creds.APIKey) == "" {
		return nil, errors.New("video.SeedanceAdapter.Poll: creds.APIKey 不能为空（凭据缺失/解密失败）")
	}

	endpoint := strings.TrimRight(entry.UpstreamBaseURL, "/") +
		fmt.Sprintf(seedanceGetTaskPathFmt, url.PathEscape(upstreamTaskID))
	statusCode, respBody, err := a.doRequest(ctx, http.MethodGet, endpoint, creds.APIKey, nil)
	if err != nil {
		return nil, err
	}
	// Poll 特有：404 = 上游未知 task_id（Submit 不应 404，故只在此判）。
	if statusCode == http.StatusNotFound {
		return nil, ErrUpstreamTaskNotFound
	}
	if err := classifyUpstreamStatusCode(statusCode, respBody); err != nil {
		return nil, err
	}

	var parsed seedanceGetResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("%w: poll 响应非合法 JSON: %v", ErrUpstreamMalformed, err)
	}
	return parsed.toPollResult(), nil
}

// buildSeedanceSubmitBody 把已校验请求改写为 seedance 原生提交 body。
//
// MVP text_to_video：content 仅文本 prompt；ratio/duration/resolution/fps 透传；watermark/
// generate_audio 取生产参考实现默认（去水印 + 生成音频，避免迁移后业务体验退化）。
// callbackURL 非空时放入 body（字段名 callback_url **待官方文档核对**；空 = 纯轮询兜底）。
//
// ⚠️ generate_audio=true 可能使上游 token 略高于按 W×H×duration×fps 估的视频部分；reserve
// 可证上界依赖 Unit 7 安全系数覆盖此增量（与长边²上界同属待集成验证项，见 resolutionLongSidePx 注释）。
func buildSeedanceSubmitBody(upstreamModel string, req *ValidatedRequest, callbackURL string) map[string]any {
	body := map[string]any{
		"model": upstreamModel,
		"content": []map[string]any{
			{"type": "text", "text": req.Prompt},
		},
		"ratio":          req.Ratio,
		"duration":       req.Duration,
		"resolution":     req.Resolution,
		"fps":            req.Fps,
		"watermark":      false,
		"generate_audio": true,
	}
	if strings.TrimSpace(callbackURL) != "" {
		body["callback_url"] = callbackURL
	}
	return body
}

// doRequest 执行单次上游 HTTP 调用：构造请求 + 注入 Bearer + 限读 body + 错误分类。
//
// 返回 (statusCode, respBody, err)；err 仅为传输层 sentinel（timeout/unreachable），
// HTTP 状态码语义由调用方经 classifyUpstreamStatusCode 判定。
func (a *SeedanceAdapter) doRequest(
	ctx context.Context, method, endpoint, apiKey string, body []byte,
) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return 0, nil, fmt.Errorf("video.SeedanceAdapter: 构造 HTTP request 失败: %w", err)
	}
	// 凭据注入 Authorization header（**绝不**入日志）。
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	if body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return 0, nil, classifyUpstreamClientErr(err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamRespBytes))
	if err != nil {
		// body 读取中超时也归 timeout（ResponseHeaderTimeout 已返但 body stall）。
		return 0, nil, classifyUpstreamClientErr(err)
	}
	return resp.StatusCode, respBody, nil
}

// classifyUpstreamStatusCode 把 HTTP 状态码映射为本包 sentinel（2xx 返 nil）。
//
// Poll 的 404→ErrUpstreamTaskNotFound 由 Poll 自身在调用本函数前内联处理（Submit 不应 404）。
func classifyUpstreamStatusCode(statusCode int, body []byte) error {
	switch {
	case statusCode >= 200 && statusCode < 300:
		return nil
	case statusCode >= 500:
		return fmt.Errorf("%w: status %d: %s", ErrUpstreamServer, statusCode, bodySnippet(body))
	default: // 4xx
		return fmt.Errorf("%w: status %d: %s", ErrUpstreamRejected, statusCode, bodySnippet(body))
	}
}

// classifyUpstreamClientErr 把 http.Client.Do 的 error 分类为传输层 sentinel
// （平行 sync relay 的 classifyClientErr，不复用）。
func classifyUpstreamClientErr(err error) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return fmt.Errorf("%w: %v", ErrUpstreamTimeout, err)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return fmt.Errorf("%w: %v", ErrUpstreamTimeout, err)
	}
	return fmt.Errorf("%w: %v", ErrUpstreamUnreachable, err)
}

// bodySnippet 截断上游错误 body 供 error 排查（上游侧消息，非我方凭据/敏感内容）。
func bodySnippet(body []byte) string {
	s := strings.TrimSpace(string(body))
	if len(s) > errBodySnippetMax {
		return s[:errBodySnippetMax] + "…"
	}
	return s
}

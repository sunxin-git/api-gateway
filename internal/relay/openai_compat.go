package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// OpenAICompatAdapter OpenAI 兼容协议 adapter（plan §Unit 3 唯一 MVP 实现）。
//
// 支持的 provider 示例（同一 adapter 实例）：
//   - 火山引擎 ARK（豆包 1.5/1.6/seedance）— MVP 首选
//   - DeepSeek API / 阿里通义 DashScope / Moonshot Kimi / OpenAI native
//
// 协议透传规则（plan §决策 D3）：
//   - 业务 request body 改写 model 字段为 entry.UpstreamModelName；其他字段
//     （messages / temperature / tools / response_format / etc.）原样转发
//   - 上游 Authorization 用 entry.UpstreamAPIKey 重写；业务侧任何 header 不转发
//   - 上游响应 body 整体透传给业务（含 usage / id / choices / model / etc.）
type OpenAICompatAdapter struct {
	client *http.Client
}

// 编译期断言。
var _ ProviderAdapter = (*OpenAICompatAdapter)(nil)

// NewOpenAICompatAdapter 构造 adapter；HTTP 客户端由调用方注入（共享单实例）。
func NewOpenAICompatAdapter(client *http.Client) *OpenAICompatAdapter {
	if client == nil {
		panic("relay.NewOpenAICompatAdapter: client 不能为 nil")
	}
	return &OpenAICompatAdapter{client: client}
}

// upstreamPath OpenAI 兼容 chat completions endpoint 子路径。
const upstreamPath = "/chat/completions"

// ChatCompletion 实现 ProviderAdapter 接口（plan §Unit 3 Approach 完整流程）。
func (a *OpenAICompatAdapter) ChatCompletion(
	ctx context.Context,
	entry *ModelEntry,
	reqBody map[string]any,
) (*UpstreamResponse, error) {
	if entry == nil {
		return nil, errors.New("relay.OpenAICompatAdapter: entry 不能为 nil")
	}

	// 1. 构造上游请求 body（克隆 + 改 model 字段）
	upstreamBody, err := buildUpstreamBody(reqBody, entry.UpstreamModelName)
	if err != nil {
		return nil, fmt.Errorf("relay.OpenAICompatAdapter: 构造上游 body 失败: %w", err)
	}

	// 2. 构造 HTTP 请求
	url := strings.TrimRight(entry.UpstreamBaseURL, "/") + upstreamPath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(upstreamBody))
	if err != nil {
		return nil, fmt.Errorf("relay.OpenAICompatAdapter: 构造 HTTP request 失败: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+entry.UpstreamAPIKey)
	httpReq.Header.Set("Content-Type", "application/json")

	// 3. 调用上游 + 错误分类
	start := time.Now()
	httpResp, err := a.client.Do(httpReq)
	duration := time.Since(start)
	if err != nil {
		return nil, classifyClientErr(err)
	}
	defer httpResp.Body.Close()

	// 4. 读 body（限 16 MiB 防上游恶意巨响应；ARK 实际响应 < 1 MiB）
	bodyBytes, err := io.ReadAll(io.LimitReader(httpResp.Body, 16*1024*1024))
	if err != nil {
		// 读 body 过程中超时也归入 timeout（部分 ResponseHeaderTimeout 已经返了但 body stall）
		return nil, classifyClientErr(err)
	}

	resp := &UpstreamResponse{
		StatusCode: httpResp.StatusCode,
		Body:       bodyBytes,
		Duration:   duration,
	}

	// 5. 仅 200 时尝试解析 usage；非 200 透传 body 不做解析
	if httpResp.StatusCode == http.StatusOK {
		usage, parseErr := parseUsageFromBody(bodyBytes)
		if parseErr != nil {
			// 200 但 body 非 JSON / 严重畸形 → ErrUpstreamMalformed
			return nil, fmt.Errorf("%w: %v", ErrUpstreamMalformed, parseErr)
		}
		// usage 缺失（nil）不算 error；handler 兜底 commit reserve（plan §决策 D2）
		resp.Usage = usage
	}

	return resp, nil
}

// =============================================================================
// 内部 helpers
// =============================================================================

// buildUpstreamBody 克隆业务 request body → 改 model 字段 → marshal。
//
// 设计选择（plan §决策 D3）：
//   - 用 map[string]any 透传所有 OpenAI 协议字段（不发明 typed struct，让 OpenAI
//     新增字段 tool_use / json_mode / parallel_tool_calls 等自动透传）
//   - 仅改写 model 字段为上游真实 model 名；保留其他字段
//   - 输入 nil map → 仍构造 {"model": upstream_model_name} 的最小 body
func buildUpstreamBody(reqBody map[string]any, upstreamModel string) ([]byte, error) {
	// 浅克隆（map 元素引用共享，但顶层 key 改 model 不污染调用方）
	cloned := make(map[string]any, len(reqBody)+1)
	for k, v := range reqBody {
		cloned[k] = v
	}
	cloned["model"] = upstreamModel
	return json.Marshal(cloned)
}

// parseUsageFromBody 从上游 JSON 响应中解析 usage 字段。
//
// 返回：
//   - *Usage, nil：成功解析（含 usage 字段且字段类型正确）
//   - nil, nil：body 是合法 JSON 但**无** usage 字段（handler 兜底 commit reserve）
//   - nil, error：body 非合法 JSON（ErrUpstreamMalformed 信号）
//
// 不强制 usage 三字段全填（避免 provider 偶发缺 total_tokens 等导致 false alarm）。
func parseUsageFromBody(body []byte) (*Usage, error) {
	var resp struct {
		Usage *Usage `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("非合法 JSON: %w", err)
	}
	return resp.Usage, nil
}

// classifyClientErr 把 http.Client.Do 返回的 error 分类为本包 sentinel。
//
// 分类规则（plan §决策 D2）：
//   - context.DeadlineExceeded → ErrUpstreamTimeout（业务侧 ctx 取消 + adapter timeout 共用此分支）
//   - context.Canceled → ErrUpstreamTimeout（业务断开；handler 仍 Release reserve）
//   - net.Error.Timeout() → ErrUpstreamTimeout（Transport-level timeout）
//   - 其他网络错（connection refused / DNS / TLS）→ ErrUpstreamUnreachable
//   - url.Error 包装的 timeout（Go 1.18+ 标准库行为）→ ErrUpstreamTimeout
func classifyClientErr(err error) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return fmt.Errorf("%w: %v", ErrUpstreamTimeout, err)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return fmt.Errorf("%w: %v", ErrUpstreamTimeout, err)
	}
	return fmt.Errorf("%w: %v", ErrUpstreamUnreachable, err)
}

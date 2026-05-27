package relay

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// ProviderAdapter 上游 provider 协议适配器接口（plan §决策 D3 + Unit 3）。
//
// MVP 唯一实现 OpenAICompatAdapter；P1+ 扩展（异步 task / streaming SSE）时实现同接口。
//
// 设计：
//   - 入参 reqBody 是 map[string]any（业务请求 body bind 出来的 raw 形式）；
//     adapter 内部克隆 + 改 model 字段 + marshal 上游 body（plan §决策 D3：透传规则）
//   - 返 UpstreamResponse 含 status / body / parsed Usage；上游 4xx/5xx 不作为 error
//   - 仅"网络层 / 协议层"异常（timeout / unreachable / 上游 200 body 非 JSON）返 error
type ProviderAdapter interface {
	// ChatCompletion 执行一次同步 chat completion；MVP 仅同步非流式。
	//
	// 入参：
	//   - ctx：调用 ctx（含业务请求 timeout / cancel）；adapter 透传给 http.Client.Do
	//   - entry：字典记录（含上游 base_url / api_key / model_name）
	//   - reqBody：业务请求 body raw map（含 model / messages / temperature / tools / etc.）
	//
	// 返回：
	//   - *UpstreamResponse: status + body 字节 + parsed Usage（status=200 + JSON 时填充）
	//   - error: ErrUpstreamTimeout / ErrUpstreamUnreachable / ErrUpstreamMalformed
	//
	// 不在 adapter 处理的：
	//   - Reserve / Commit / Release（handler 路径管，adapter 仅做 relay）
	//   - audit / metric emit（handler / middleware 管）
	//   - stream=true 拒绝（handler 路径管）
	ChatCompletion(ctx context.Context, entry *ModelEntry, reqBody map[string]any) (*UpstreamResponse, error)
}

// =============================================================================
// 工厂
// =============================================================================

// NewAdapter 按 provider_type 返对应 adapter 实例（plan §Unit 3 工厂）。
//
// MVP 唯一支持 "openai_compat"；未知 provider_type 返 error（fail-fast，与
// catalog validate 校验一致语义）。Unit 7 main.go 装配时按 cfg.UpstreamProviderType 调本函数。
func NewAdapter(providerType string, client *http.Client) (ProviderAdapter, error) {
	if client == nil {
		return nil, fmt.Errorf("relay.NewAdapter: http.Client 不能为 nil")
	}
	switch providerType {
	case "openai_compat":
		return NewOpenAICompatAdapter(client), nil
	default:
		return nil, fmt.Errorf("relay.NewAdapter: 未知 provider_type=%q（MVP 仅支持 openai_compat）", providerType)
	}
}

// =============================================================================
// HTTP 客户端工厂（main.go Unit 7 用）
// =============================================================================

// UpstreamClientTimeout 上游 HTTP 总超时（plan §决策 D10）。
const UpstreamClientTimeout = 60 * time.Second

// NewUpstreamClient 构造 relay 用 HTTP 客户端（plan §决策 D10）。
//
// 配置：
//   - Timeout: 60s 总超时（含 DNS / connect / TLS / write / read）
//   - MaxIdleConns 100 + per-host 20（豆包 ARK 单 host 复用连接池）
//   - IdleConnTimeout 90s（与典型 KAM idle 设置一致）
//   - TLSHandshakeTimeout 10s
//   - ResponseHeaderTimeout 30s（防上游 connect 后 stall）
//
// main.go 在 Unit 7 调本函数构造单实例，注入 OpenAICompatAdapter。
func NewUpstreamClient() *http.Client {
	tr := &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &http.Client{
		Transport: tr,
		Timeout:   UpstreamClientTimeout,
	}
}

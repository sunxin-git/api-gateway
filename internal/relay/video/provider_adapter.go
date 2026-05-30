package video

import (
	"context"
	"errors"
)

// =============================================================================
// 异步视频 provider adapter 契约（计划 §Unit 5）
//
// 与同步 relay 的 ProviderAdapter（ChatCompletion 单请求闭环）**平行不复用**：
// 异步是 Submit→Poll 两段形态。上游无幂等键、不可按我方标识反查（ADR-0006 官方确认），
// 故**不提供** PollByIdempotency；崩溃在「Submit 成功 → 存 upstream_task_id」之间的恢复
// 由 Unit 6 recover **fail-closed** 兜底（不自动重投，超阈值 FAILED+release+告警）。
// =============================================================================

// UpstreamStatus 是上游任务状态的规范化枚举。
//
// adapter 把各 provider 的多别名（queued/running/processing/...）收敛到此小集合，
// Unit 6 task FSM 据此推进 task 状态（Succeeded→COMPLETED / Failed→FAILED / ...）。
// 保留 Cancelled / Expired 为独立终态（ADR-0006：seedance 6 态含 cancelled/expired，
// 与 failed 语义不同：cancelled 仅排队中可取消、expired 为执行超期），不并入 Failed。
type UpstreamStatus string

const (
	// UpstreamRunning 仍在进行（queued/running/pending/processing/... 及未知/空状态）。
	// **未知/空一律归此**（不提前终态，由 Unit 6 expire worker 设最长执行期上界兜底，fail-safe）。
	UpstreamRunning UpstreamStatus = "running"
	// UpstreamSucceeded 上游成功终态 → Unit 6 推 task COMPLETED。
	UpstreamSucceeded UpstreamStatus = "succeeded"
	// UpstreamFailed 上游失败终态。
	UpstreamFailed UpstreamStatus = "failed"
	// UpstreamCancelled 上游取消终态（seedance 仅排队中可取消）。
	UpstreamCancelled UpstreamStatus = "cancelled"
	// UpstreamExpired 上游执行超期终态（seedance execution_expires_after，默认 48h）。
	UpstreamExpired UpstreamStatus = "expired"
)

// IsTerminal 报告该状态是否上游终态（不再变化）。
//
// 语义对应（Unit 6）：终态 ⟺ 任务不再占用上游并发槽 → CAS 进终态的赢家同事务释放 claim
// （ADR-0006）。settle_failed 不是上游状态、不持 claim，不在此枚举。
func (s UpstreamStatus) IsTerminal() bool {
	switch s {
	case UpstreamSucceeded, UpstreamFailed, UpstreamCancelled, UpstreamExpired:
		return true
	default:
		return false
	}
}

// UpstreamUsage 是上游返回的真实用量（结算口径，ADR-0006）。
type UpstreamUsage struct {
	// CompletionTokens 视频模型计费 token。ADR-0006：视频模型 total_tokens == completion_tokens
	// （输入 token 计 0），故只取 completion_tokens；Unit 7 settle 再套最低 token 计费下限。
	CompletionTokens int64
}

// PollResult 是 Poll 的规范化返回（计划 §Unit 5：status + usage + resultURL）。
//
// 合并计划草案的 (status, usage, resultURL, error) 四返回为单结构（Go 惯例：关联字段成组）。
type PollResult struct {
	// Status 规范化上游状态。
	Status UpstreamStatus

	// Usage 上游真实用量；**仅 Succeeded 且上游确返可信 usage 时非 nil**。
	// Succeeded 但缺 usage / completion_tokens ≤ 0 → nil（Unit 7 据此落 settle_failed + 对账，
	// ADR-0006：不按 reserve 上界 commit、不静默 release）。
	Usage *UpstreamUsage

	// ResultURL 产物视频 URL（仅 Succeeded 时可能有值）。
	// ADR-0006：seedance content.video_url mp4 仅 **24h 有效** → Unit 9 须在完成后 24h 内转存 TOS。
	// Succeeded 但 URL 为空属异常，由 Unit 6/9 转人工对账（adapter 不报错，原样返回空）。
	ResultURL string

	// FailureMessage 上游失败原因文本（Failed/Cancelled/Expired 时填，供 audit/告警；不含我方敏感内容）。
	FailureMessage string

	// FailureCode 上游失败错误码（error.code 原文，如 "content_violation"）；供 Unit 6/12
	// 按码分类审计 / 告警（内容违规 vs 参数错 vs 配额）。非失败态为空。
	FailureCode string
}

// UpstreamCredentials 是调用上游所需的最小凭据（即用即弃）。
//
// 设计（解耦）：catalog 的 VideoModelEntry 只持 ChannelName 不持密钥（凭据加密在 channel 表，
// Unit 3）。adapter **不** import channel / 不碰 DB，由调用方（Unit 6）从 channel service
// 解密后映射到本结构传入，用完即弃。MVP text_to_video 走 Bearer，仅需 APIKey；ARK AK/SK
// 与 TOS 凭据分别用于其他接口 / Unit 9，不在此。
type UpstreamCredentials struct {
	// APIKey 火山方舟模型推理 Bearer API Key（注入 Authorization header）。**绝不**入日志。
	APIKey string
}

// AsyncProviderAdapter 异步视频 provider adapter 接口（计划 §Unit 5）。
//
// provider_type 扩 volc_seedance（见 catalog）；新增第二个视频 provider = 加一个实现 + 工厂注册。
type AsyncProviderAdapter interface {
	// Submit 提交生成任务，返回上游 task_id。
	//
	//   - entry：catalog 条目（上游 base URL + 真实 model 名）。
	//   - creds：解密后的上游凭据（即用即弃，绝不入日志）。
	//   - req：已校验+规范化的请求（Unit 4 ValidatedRequest）。
	//   - callbackURL：回调地址（含 per-task token，Unit 6/8 构造）；空 = 不注册回调、纯轮询兜底。
	//     adapter 不解析 token，仅原样放入上游 body。
	//
	// 错误为本包上游 sentinel（ErrUpstream*）。
	Submit(ctx context.Context, entry *VideoModelEntry, creds UpstreamCredentials, req *ValidatedRequest, callbackURL string) (upstreamTaskID string, err error)

	// Poll 按**上游** task_id 查询任务状态 + 用量 + 产物 URL。
	// 无「按我方标识反查」能力（ADR-0006：上游不支持），故只接受上游 task_id。
	Poll(ctx context.Context, entry *VideoModelEntry, creds UpstreamCredentials, upstreamTaskID string) (*PollResult, error)
}

// =============================================================================
// 上游 sentinel errors —— adapter 对外契约
//
// 调用方（Unit 6 workers / Unit 10 handler）用 errors.Is 判断后决定重试 / 终态 / 告警：
//   - Timeout / Unreachable / Server：瞬时。
//   - Rejected：我方请求被拒（参数/鉴权），非瞬时 → FAILED + release，不重试。
//   - Malformed：上游 2xx 但响应不可解析（缺 task_id / 非法 JSON）→ 视作 provider bug，告警。
//   - TaskNotFound：Poll 未知 task_id → FAILED + 告警（不应发生，除非映射错乱）。
//
// ⚠️ **「瞬时可重试」只对幂等的 Poll 成立**（Poll 同 task_id 反复查询无副作用）。
// **Submit 绝不可在 Timeout/Server 上自动重试**：上游无幂等键、不可按我方标识反查（ADR-0006），
// Submit 超时/5xx 后上游**可能已建任务**，盲目重投 = 双任务双扣。Submit 的瞬时错误由 Unit 6
// 走 fail-closed recover（提交前持久化 UPSTREAM_SUBMITTING；lease 过期不自动重投，超阈值
// CAS→FAILED + release + 告警）。adapter 只忠实分类错误，不承担「是否重试」决策。
// =============================================================================
var (
	// ErrUpstreamTimeout 请求超过 ctx/客户端 timeout（含 context cancel）。
	ErrUpstreamTimeout = errors.New("video upstream request timeout")
	// ErrUpstreamUnreachable 连接拒绝 / DNS / TLS / 网络分区。
	ErrUpstreamUnreachable = errors.New("video upstream connection failed")
	// ErrUpstreamMalformed 上游 2xx 但响应非法 JSON / 缺关键字段（如 Submit 无 task_id）。
	ErrUpstreamMalformed = errors.New("video upstream response malformed")
	// ErrUpstreamRejected 上游 4xx（参数错 / 鉴权失败）；非瞬时，不重试。
	ErrUpstreamRejected = errors.New("video upstream rejected request")
	// ErrUpstreamServer 上游 5xx；瞬时，可重试。
	ErrUpstreamServer = errors.New("video upstream server error")
	// ErrUpstreamTaskNotFound Poll 时上游返 404（未知 task_id）。
	ErrUpstreamTaskNotFound = errors.New("video upstream task not found")
)

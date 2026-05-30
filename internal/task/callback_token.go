package task

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sunxin-git/api-gateway/internal/db"
)

// 回调 ingress 的 per-task token 生成 / 常量时间校验 + 回调处理编排（plan §Unit 8 / ADR-0006 决策 5）。
//
// 安全要点：
//   - token 与 task_id **分离**：task_id 会回给业务方（提交响应 / GET 状态），token 是只嵌入「交给
//     上游的回调 URL」里的独立秘密，业务方不可见 → 业务方无法伪造回调。
//   - token 置于回调 URL **路径不可枚举段**（非 query string）；含 token 的 URL **绝不入日志/审计/span**。
//   - 校验用**常量时间比较**（防时序侧信道枚举 token）。
//   - **回调体不可信**（ADR-0006）：回调仅作「去查」触发；终态与 usage 一律由网关 Poll 反查上游，
//     不采信回调体里的 status/usage。
//   - 终态后 token 由 MarkTaskUpstreamTerminal 置 NULL：迟到回调走「非 UPSTREAM_SUBMITTED → 200 忽略」分支。

// callbackTokenBytes per-task 回调 token 的随机字节数（256-bit，hex 编码 64 字符）。
const callbackTokenBytes = 32

// newCallbackToken 生成密码学随机回调 token（hex）。
func newCallbackToken() string {
	var b [callbackTokenBytes]byte
	_, _ = rand.Read(b[:]) // crypto/rand 失败仅在系统熵源故障；忽略 err 与代码库 newTaskID 一致
	return hex.EncodeToString(b[:])
}

// constantTimeTokenMatch 常量时间比较存储 token 与请求 token。
//
// 长度不同直接 false（token 同长，长度非机密）；等长用 subtle.ConstantTimeCompare 防时序枚举。
// 空存储 token（NULL / 纯轮询任务）一律不匹配。
func constantTimeTokenMatch(stored, provided string) bool {
	if stored == "" || len(stored) != len(provided) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(stored), []byte(provided)) == 1
}

// CallbackOutcome 是回调处理结果（HTTP 层据此映射状态码，不泄露任务存在性）。
type CallbackOutcome int

const (
	// CallbackAccepted token 有效且任务在途 → 已触发 Poll 反查 + 推进（HTTP 200）。
	CallbackAccepted CallbackOutcome = iota
	// CallbackIgnoredUnknownTask 任务不存在 → 200 忽略（不泄露存在性、不放大）。
	CallbackIgnoredUnknownTask
	// CallbackIgnoredState 任务非 UPSTREAM_SUBMITTED（提交中 / 已终态 / 结算中 / 已结算）→ 200 忽略（去抖）。
	CallbackIgnoredState
	// CallbackUnauthorized 任务在途但 token 缺/错 → 401，不改状态。
	CallbackUnauthorized
)

func (o CallbackOutcome) String() string {
	switch o {
	case CallbackAccepted:
		return "accepted"
	case CallbackIgnoredUnknownTask:
		return "ignored_unknown_task"
	case CallbackIgnoredState:
		return "ignored_state"
	case CallbackUnauthorized:
		return "unauthorized"
	default:
		return "unknown"
	}
}

// HandleCallback 处理上游回调（plan §Unit 8：校验 token → 去抖 → 触发 Poll 反查推进）。
//
// 流程（顺序刻意）：
//  1. 按 task_id 查任务；不存在 → CallbackIgnoredUnknownTask（200，不泄露/不放大）。
//  2. **状态去抖优先于 token 校验**：仅 UPSTREAM_SUBMITTED 才可能触发 Poll；其余状态（提交中、
//     已终态(token 已 NULL)、SETTLING、SETTLED）一律 CallbackIgnoredState（200）——既实现「已终态 →
//     忽略」（终态 token 已 NULL，本就无法校验），又防「泄露 token 强制重复 Poll」的放大攻击
//     （任务一旦推进出 UPSTREAM_SUBMITTED，后续回调不再 Poll）。
//  3. 常量时间校验 per-task token；缺/错 → CallbackUnauthorized（401，不改状态）。
//  4. 复用 6b pollAndAdvance：**Poll 上游反查真实状态**（忽略回调体）→ CAS 终态（赢家同事务释放
//     claim + 唯一入队 settle）；settle 再 Poll 反查 usage 结算。pollAndAdvance 内部 CAS 幂等，
//     重复/并发回调安全（多并发回调仅一个 CAS 赢家推进 + 入队 settle，claim 仅释放一次）。
//
// **可靠性语义（回调是优化，非真相源）**：本方法同步调 pollAndAdvance（上游 Poll 受 pollTO 上界，
// 默认 10s）；Poll 失败/超时时 pollAndAdvance 不推进、不报错，回调仍返 CallbackAccepted(200)——
// 因真实状态由网关 Poll 负责、非上游回调体，且**周期 fetch sweep（默认 30s）是兜底**：任务终会被
// 推进，最坏多等一个 sweep 周期。故意不返 5xx 让上游重投（避免回调放大重复 Poll；上游回调超时预算
// 待 Unit 5 内网穿透实测校准）。返回 error 仅用于**瞬时内部失败**（DB 抖动）→ HTTP 500 让上游重试；
// 已处理的业务结果一律经 CallbackOutcome 返回（err == nil）。
func (s *Service) HandleCallback(ctx context.Context, taskID, token string) (CallbackOutcome, error) {
	t, err := s.q.GetTaskByID(ctx, taskID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return CallbackIgnoredUnknownTask, nil
		}
		return 0, fmt.Errorf("task.HandleCallback get: %w", err) // 瞬时 → 500（上游重试 / sweep 兜底）
	}

	if t.Status != db.TaskStatusUPSTREAMSUBMITTED {
		return CallbackIgnoredState, nil
	}

	if !t.CallbackToken.Valid || !constantTimeTokenMatch(t.CallbackToken.String, token) {
		return CallbackUnauthorized, nil
	}

	s.pollAndAdvance(ctx, t)
	return CallbackAccepted, nil
}

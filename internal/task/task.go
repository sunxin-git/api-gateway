// Package task 实现异步视频任务的状态机、提交流程与 Asynq workers（Phase 2 / Unit 6）。
//
// 计划：docs/plans/2026-05-28-001-feat-async-video-relay-mvp-plan.md Unit 6
// 决策：docs/adr/0006-async-execution-asynq-redis.md（异步基座 / DB 原子 claim / 上游无幂等 fail-closed）
// 设计文档：docs/multimedia-gateway-design.md §9 / §9ter / §9.5
//
// 职责分层（CLAUDE.md SRP）：
//   - task.go：状态机白名单（FSM）+ 上游状态→task 终态映射（纯逻辑，无 IO）
//   - snapshot.go：财务快照（reserve↔settle 同 correlation + 冻结价 + 请求参数重建）
//   - service.go：提交流程 + 状态机 CAS 封装 + settle 编排（持 pool/ledger/adapter/channel/catalog）
//   - workers.go：Asynq handler（submit/settle）+ 入队抽象
//
// 状态机硬约束（CONTEXT.md 状态机模式 / ADR-0006）：所有状态变更**只**走带 from 条件的
// CAS（sql/queries/task.sql 的 :execrows，受影响 0 行 = CAS 失败）；本文件的转移白名单是
// 显式真相源，service 推进前用 canTransition 自检，杜绝非法转移。
package task

import (
	"github.com/sunxin-git/api-gateway/internal/db"
	"github.com/sunxin-git/api-gateway/internal/relay/video"
)

// allowedTransitions 任务状态机的合法 from→to 转移白名单（plan §High-Level Technical Design 状态机图）。
//
// 10 态（db.TaskStatus）：
//
//	SUBMITTED ─▶ UPSTREAM_SUBMITTING ─▶ UPSTREAM_SUBMITTED ─▶ {COMPLETED/FAILED/CANCELLED/EXPIRED}
//	                                                              └─▶ SETTLING ─▶ SETTLED / SETTLE_FAILED
//
// 不变量：
//   - 进上游终态（COMPLETED/FAILED/CANCELLED/EXPIRED）的 CAS 同事务释放并发 claim（service 落实）。
//   - 上游终态 → SETTLING 由 settle worker 推进；SETTLING → SETTLED（成功）/ SETTLE_FAILED（缺 usage）。
//   - SETTLE_FAILED 不持 claim（claim 已在上游终态释放）；其 reserve 留对账 worker 处理（Unit 7/6b）。
var allowedTransitions = map[db.TaskStatus]map[db.TaskStatus]struct{}{
	db.TaskStatusSUBMITTED: {
		db.TaskStatusUPSTREAMSUBMITTING: {}, // submit worker 抢占 lease
		db.TaskStatusEXPIRED:            {}, // expire 兜底（超最长执行期仍未提交）
	},
	db.TaskStatusUPSTREAMSUBMITTING: {
		db.TaskStatusUPSTREAMSUBMITTED: {}, // 上游 Submit 成功 → 存 upstream_task_id
		db.TaskStatusFAILED:            {}, // submit 失败 / recover fail-closed（不双扣）
		db.TaskStatusEXPIRED:           {}, // expire 兜底
	},
	db.TaskStatusUPSTREAMSUBMITTED: {
		db.TaskStatusCOMPLETED: {}, // 回调/Poll 命中成功
		db.TaskStatusFAILED:    {},
		db.TaskStatusCANCELLED: {},
		db.TaskStatusEXPIRED:   {},
	},
	// 上游终态 → SETTLING（settle worker CAS 抢占结算）。
	db.TaskStatusCOMPLETED: {db.TaskStatusSETTLING: {}},
	db.TaskStatusFAILED:    {db.TaskStatusSETTLING: {}},
	db.TaskStatusCANCELLED: {db.TaskStatusSETTLING: {}},
	db.TaskStatusEXPIRED:   {db.TaskStatusSETTLING: {}},
	db.TaskStatusSETTLING: {
		db.TaskStatusSETTLED:      {}, // 成功不可变终态（commit / release 完成）
		db.TaskStatusSETTLEFAILED: {}, // 缺 usage / Poll 持续失败 → 对账队列
	},
	// SETTLED / SETTLE_FAILED 为不可变终态，无出边。
}

// upstreamTerminalStatuses 占用上游并发 claim 的「上游终态」集合（进入即释放 claim）。
var upstreamTerminalStatuses = map[db.TaskStatus]struct{}{
	db.TaskStatusCOMPLETED: {},
	db.TaskStatusFAILED:    {},
	db.TaskStatusCANCELLED: {},
	db.TaskStatusEXPIRED:   {},
}

// canTransition 报告 from→to 是否合法转移（service 推进前自检，挡编程错误于 DB 之前）。
func canTransition(from, to db.TaskStatus) bool {
	tos, ok := allowedTransitions[from]
	if !ok {
		return false
	}
	_, ok = tos[to]
	return ok
}

// isUpstreamTerminal 报告该状态是否「上游终态」（进入即释放 claim）。
func isUpstreamTerminal(s db.TaskStatus) bool {
	_, ok := upstreamTerminalStatuses[s]
	return ok
}

// upstreamStatusToTaskTerminal 把上游终态映射到对应的 task 上游终态（6b fetch reconciler 用）。
//
// 映射（与 allowedTransitions 的 UPSTREAM_SUBMITTED→{...} 出边一一对应）：
//
//	Succeeded → COMPLETED / Failed → FAILED / Cancelled → CANCELLED / Expired → EXPIRED
//
// 非终态（Running，含未知/空状态被 adapter 归一为 Running）返回 (_, false)：调用方据此**跳过**
// （不提前终态——继续轮询，超最长执行期由 expire worker 兜底，fail-safe）。
//
// 注：isInflight（计划提到的另一 helper）6b 未引入——各 sweep 的「在途」判定已由 SQL scan 的
// status 谓词承载（ScanStuck* / ScanExpirableTasks），Go 侧再加一个等价判定属冗余，故省（避免死代码）。
func upstreamStatusToTaskTerminal(s video.UpstreamStatus) (db.TaskStatus, bool) {
	switch s {
	case video.UpstreamSucceeded:
		return db.TaskStatusCOMPLETED, true
	case video.UpstreamFailed:
		return db.TaskStatusFAILED, true
	case video.UpstreamCancelled:
		return db.TaskStatusCANCELLED, true
	case video.UpstreamExpired:
		return db.TaskStatusEXPIRED, true
	default:
		return "", false
	}
}

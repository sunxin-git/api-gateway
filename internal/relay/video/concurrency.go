package video

import "fmt"

// 账户×模型并发上限**值**解析（plan §Unit 8 concurrency.go）。
//
// **职责边界**：本类型只解析 cap 的**数值**（默认 + per-(account,model) 覆写）；R15 的实际并发硬上限
// **实施**在 DB 原子 claim（sql/queries/task.sql 的 ClaimConcurrencySlot，以本值为 @cap_limit），
// **不**映射 Asynq 队列 concurrency（ADR-0006 决策 2：队列 concurrency 是进程级吞吐、非跨副本硬上限）。
//
// cap 语义（与 ClaimConcurrencySlot 对齐）：
//   - cap >= 1：该 (account, model) 最多 cap 个任务同时在上游在途（SUBMITTED/UPSTREAM_SUBMITTING/
//     UPSTREAM_SUBMITTED 三态）；占满 → 提交返 429。
//   - cap == 0：**禁用**该 (account, model)（ClaimConcurrencySlot 的 cap=0 守卫使首次 INSERT 也占不到 → 429）。

// ConcurrencyKey 是并发上限覆写的查找键（账户×模型粒度）。
type ConcurrencyKey struct {
	AccountID string
	Model     string
}

// ConcurrencyLimits 解析每 (account, model) 的并发上限值（默认 + 覆写）。
//
// 不可变：构造后只读，可安全并发 Cap()（覆写 map 构造时深拷贝，调用方后续改原 map 不影响）。
type ConcurrencyLimits struct {
	defaultCap int32
	overrides  map[ConcurrencyKey]int32
}

// NewConcurrencyLimits 构造并发上限解析器 + fail-fast 校验（default/覆写均须 >= 0）。
//
// overrides 可为 nil（仅用默认）；MVP 由 config 注入默认值，覆写表预留给 Unit 11 admin 写入。
func NewConcurrencyLimits(defaultCap int32, overrides map[ConcurrencyKey]int32) (*ConcurrencyLimits, error) {
	if defaultCap < 0 {
		return nil, fmt.Errorf("video.ConcurrencyLimits: defaultCap 不能为负（当前 %d）", defaultCap)
	}
	cp := make(map[ConcurrencyKey]int32, len(overrides))
	for k, v := range overrides {
		if v < 0 {
			return nil, fmt.Errorf(
				"video.ConcurrencyLimits: 覆写 (account=%q, model=%q) 上限不能为负（当前 %d）",
				k.AccountID, k.Model, v)
		}
		cp[k] = v
	}
	return &ConcurrencyLimits{defaultCap: defaultCap, overrides: cp}, nil
}

// Cap 返回 (account, model) 的并发上限：命中覆写返覆写值，否则返默认值。
func (c *ConcurrencyLimits) Cap(accountID, model string) int32 {
	if v, ok := c.overrides[ConcurrencyKey{AccountID: accountID, Model: model}]; ok {
		return v
	}
	return c.defaultCap
}

// DefaultCap 返回默认上限（运维 / 诊断用）。
func (c *ConcurrencyLimits) DefaultCap() int32 { return c.defaultCap }

package admintoken

import (
	"log/slog"
	"sort"
	"sync"
	"time"
)

// InProcessRPM 进程内 RPM 限速实现（计划 Unit 3 + 决策 D6）。
//
// 数据结构：sync.Map[tokenID int64] *tokenRPMState；每 token 持 sync.Mutex + timestamp slice。
//
// 决策权衡：
//   - timestamp slice 而非固定 60 bucket：避免 500ms × 200 burst 误判（决策 D6 Approach）
//   - hard cap 容量 = token.RequestsPerMinute：防止恶意 token 内存爆炸
//   - GC goroutine：每 5 min 扫并删除长时间未触达的 token state；防短期攻击 token 内存常驻
//   - 多实例失效：P0 单实例部署接受；P1 接 Redis 时换成 Lua 脚本（接口不变）
//
// 进程重启即 RPM 计数清零，被视为攻击征兆 → 启动时调一次 onColdStart hook（让 Unit 7 bump cold-start metric）。
type InProcessRPM struct {
	states sync.Map // tokenID int64 → *tokenRPMState

	now func() time.Time // 注入式时钟，便于测试控制时间

	gcInterval    time.Duration
	idleThreshold time.Duration // last_seen < now - idleThreshold → GC
	gcStopCh      chan struct{}
	gcDoneCh      chan struct{}
	onColdStart   func() // 启动 hook（如 bump cold-start metric）；可 nil
	log           *slog.Logger
}

// tokenRPMState 单 token 的 RPM 状态。
//
// 字段：
//   - timestamps：按时间升序排列的纳秒时间戳；每次 Check 通过时追加
//   - lastSeen：最近一次 Check 时间，GC 用
//   - mu：保护并发 Check 同 token 时的 slice 修改
type tokenRPMState struct {
	mu         sync.Mutex
	timestamps []int64 // ns
	lastSeen   int64   // ns
}

// NewInProcessRPM 构造 InProcessRPM；启动 GC goroutine。
//
// 参数：
//   - log：slog logger，不能为 nil
//   - onColdStart：进程启动 hook（如 bump prometheus metric）；可 nil（测试用）
//
// 返回的 RPM 实例必须最终调 Close() 停 GC goroutine（main.go defer）。
func NewInProcessRPM(log *slog.Logger, onColdStart func()) *InProcessRPM {
	r := newInProcessRPMBase(log, onColdStart)
	r.gcDoneCh = make(chan struct{})
	go r.gcLoop()
	return r
}

// newInProcessRPMBase 构造 InProcessRPM 但**不**启动 GC goroutine。
//
// 仅供包内测试用：避免 GC goroutine 与测试主线对 now / gcInterval / idleThreshold
// 等字段产生数据竞争（race detector 触发）。测试需要 GC 时显式调 r.gcOnce()。
// gcDoneCh 留 nil；Close 检测 nil 跳过等待。
func newInProcessRPMBase(log *slog.Logger, onColdStart func()) *InProcessRPM {
	if log == nil {
		panic("admintoken.NewInProcessRPM: log 不能为 nil")
	}
	r := &InProcessRPM{
		now:           time.Now,
		gcInterval:    5 * time.Minute,
		idleThreshold: 10 * time.Minute,
		gcStopCh:      make(chan struct{}),
		// gcDoneCh 不分配；NewInProcessRPM 启动 goroutine 时才分配
		onColdStart: onColdStart,
		log:         log,
	}

	// 进程冷启 hook：让 Unit 7 注入 metric bump
	if onColdStart != nil {
		onColdStart()
	}
	log.Info("InProcessRPM 启动；RPM 计数已清零（进程重启即归零，多实例计数独立）")
	return r
}

// Close 停 GC goroutine；调用方应在进程退出时 defer 调用。
//
// 已 Close 的 RPM 实例不应再 Check（行为未定义；P0 当前 panic）。
// 测试构造（newInProcessRPMBase）无 GC goroutine，gcDoneCh 为 nil，Close 立即返回。
// 多次 Close 幂等。
func (r *InProcessRPM) Close() error {
	select {
	case <-r.gcStopCh:
		// 已 close，幂等返回
	default:
		close(r.gcStopCh)
	}
	if r.gcDoneCh != nil {
		<-r.gcDoneCh
	}
	return nil
}

// Check 检查 token 是否超 RPM 限额；通过则追加当前时间戳到 ring。
//
// 调用方应在 token.RequestsPerMinute = nil 时**不**调本方法（直接 nil-check 跳过），
// 但本方法对 nil RequestsPerMinute 也做 fail-open 防御。
//
// 内部步骤：
//
//  1. LoadOrStore 取得 state（首次访问的 token 创建空 state）
//  2. Lock + 裁 cutoff (now - 60s) 之前的 timestamps
//  3. 当前 count + 1 ≤ limit → 追加 now，返 nil；否则返 ErrRPMExceeded
//  4. 失败时**不**追加（被拒请求不计入下次基数）
func (r *InProcessRPM) Check(token *Token) error {
	if token == nil || token.RequestsPerMinute == nil {
		return nil
	}
	limit := int(*token.RequestsPerMinute)
	if limit <= 0 {
		// 0 视为永久拒绝；管理员应通过 nil 表达"无限制"
		return ErrRPMExceeded
	}

	v, _ := r.states.LoadOrStore(token.ID, &tokenRPMState{})
	st, ok := v.(*tokenRPMState)
	if !ok {
		// 防御性：sync.Map 中类型异常不应发生
		return nil
	}

	now := r.now().UnixNano()
	cutoff := now - int64(time.Minute)

	st.mu.Lock()
	defer st.mu.Unlock()

	// 二分查找第一个 >= cutoff 的位置，截掉前面（窗口已滑出）
	idx := sort.Search(len(st.timestamps), func(i int) bool { return st.timestamps[i] >= cutoff })
	if idx > 0 {
		// 复用底层数组容量，避免每次 grow
		n := copy(st.timestamps, st.timestamps[idx:])
		st.timestamps = st.timestamps[:n]
	}

	st.lastSeen = now

	if len(st.timestamps) >= limit {
		return ErrRPMExceeded
	}

	// 防御性 hard cap：避免恶意客户端导致 slice 无限增长（理论上 limit 限制了，但兜底）
	if cap(st.timestamps) > 4*limit && len(st.timestamps) < limit/2 {
		// slice 容量远大于当前内容时主动收缩
		newTS := make([]int64, len(st.timestamps), limit)
		copy(newTS, st.timestamps)
		st.timestamps = newTS
	}

	st.timestamps = append(st.timestamps, now)
	return nil
}

// gcLoop 周期性清理长时间未访问的 token state。
//
// 触发条件：lastSeen < now - idleThreshold；删除其 sync.Map entry。
// 防短期攻击 token 内存常驻；token 数本来就少（≤ 几十个），GC 主要意义在 lastSeen 过老的 token 释放 slice。
func (r *InProcessRPM) gcLoop() {
	defer close(r.gcDoneCh)
	ticker := time.NewTicker(r.gcInterval)
	defer ticker.Stop()

	for {
		select {
		case <-r.gcStopCh:
			return
		case <-ticker.C:
			r.gcOnce()
		}
	}
}

// gcOnce 单轮 GC 扫描；测试可直接调用验证行为。
func (r *InProcessRPM) gcOnce() {
	now := r.now().UnixNano()
	cutoff := now - int64(r.idleThreshold)

	var removed int
	r.states.Range(func(key, value any) bool {
		st, ok := value.(*tokenRPMState)
		if !ok {
			return true
		}
		st.mu.Lock()
		idle := st.lastSeen < cutoff
		st.mu.Unlock()
		if idle {
			r.states.Delete(key)
			removed++
		}
		return true
	})
	if removed > 0 {
		r.log.Debug("InProcessRPM GC removed idle tokens", slog.Int("removed", removed))
	}
}

// peekCount 仅测试用：返回当前 token 窗口内的 count（不裁剪、不追加）。
func (r *InProcessRPM) peekCount(tokenID int64) int {
	v, ok := r.states.Load(tokenID)
	if !ok {
		return 0
	}
	st, ok := v.(*tokenRPMState)
	if !ok {
		return 0
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return len(st.timestamps)
}

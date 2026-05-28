package businesskey

import (
	"log/slog"
	"sort"
	"sync"
	"time"
)

// InProcessRPM 业务侧进程内 RPM 限速实现（F-min 决策 D7）。
//
// **设计决策**：1:1 复制 admintoken.InProcessRPM 代码模式而非抽象 generic（CLAUDE.md §六
// 稳定 > 优雅；plan §D7）。两套实现并列让 reviewer 一眼看出"两路独立"，且未来
// admin / business 可各自演化（admin 加 IP 维度 / business 加 account 维度）不冲突。
//
// 与 admintoken 同名实现的差异：
//   - 字段名 keyID 替换 tokenID
//   - cold-start metric 名（main.go Unit 7 装配时注入；构造函数仅接 hook）
//   - 其他逻辑（sync.Map / timestamp slice / 二分裁窗 / GC / cold-start hook / Check 方法）
//     字符级一致
//
// 数据结构：sync.Map[keyID int64] *keyRPMState；每 key 持 sync.Mutex + timestamp slice。
//
// 决策权衡（与 admintoken D6 相同）：
//   - timestamp slice 而非固定 60 bucket：避免 500ms × 200 burst 误判
//   - hard cap 容量 = key.RequestsPerMinute：防止恶意 key 内存爆炸
//   - GC goroutine：每 5 min 扫并删除长时间未触达的 key state
//   - 多实例失效：P0 单实例部署接受；P1 接 Redis 时换成 Lua 脚本（接口不变）
//
// 进程重启即 RPM 计数清零，被视为攻击征兆 → 启动时调一次 onColdStart hook
// （Unit 7 装配时 bump cold-start metric）。
type InProcessRPM struct {
	states sync.Map // keyID int64 → *keyRPMState

	now func() time.Time // 注入式时钟，便于测试控制时间

	gcInterval    time.Duration
	idleThreshold time.Duration // last_seen < now - idleThreshold → GC
	gcStopCh      chan struct{}
	gcDoneCh      chan struct{} // nil 表示无 GC goroutine（测试构造路径）
	onColdStart   func()        // 启动 hook（如 bump cold-start metric）；可 nil
	log           *slog.Logger
}

// keyRPMState 单 key 的 RPM 状态。
type keyRPMState struct {
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
		panic("businesskey.NewInProcessRPM: log 不能为 nil")
	}
	r := &InProcessRPM{
		now:           time.Now,
		gcInterval:    5 * time.Minute,
		idleThreshold: 10 * time.Minute,
		gcStopCh:      make(chan struct{}),
		// gcDoneCh 不分配
		onColdStart: onColdStart,
		log:         log,
	}
	if onColdStart != nil {
		onColdStart()
	}
	log.Info("businesskey InProcessRPM 启动；RPM 计数已清零（进程重启即归零，多实例计数独立）")
	return r
}

// Close 停 GC goroutine；调用方应在进程退出时 defer 调用。
// 测试构造无 GC goroutine，Close 立即返回。多次 Close 幂等。
func (r *InProcessRPM) Close() error {
	select {
	case <-r.gcStopCh:
	default:
		close(r.gcStopCh)
	}
	if r.gcDoneCh != nil {
		<-r.gcDoneCh
	}
	return nil
}

// Check 检查 key 是否超 RPM 限额；通过则追加当前时间戳到 ring。
//
// 调用方应在 key.RequestsPerMinute = nil 时**不**调本方法，但本方法对 nil 也做 fail-open 防御。
//
// 内部步骤：
//  1. LoadOrStore 取得 state（首次访问的 key 创建空 state）
//  2. Lock + 裁 cutoff (now - 60s) 之前的 timestamps
//  3. 当前 count + 1 ≤ limit → 追加 now，返 nil；否则返 ErrRPMExceeded
//  4. 失败时**不**追加（被拒请求不计入下次基数）
func (r *InProcessRPM) Check(key *Key) error {
	if key == nil || key.RequestsPerMinute == nil {
		return nil
	}
	limit := int(*key.RequestsPerMinute)
	if limit <= 0 {
		return ErrRPMExceeded
	}

	v, _ := r.states.LoadOrStore(key.ID, &keyRPMState{})
	st, ok := v.(*keyRPMState)
	if !ok {
		// 防御性：sync.Map 中类型异常不应发生
		return nil
	}

	now := r.now().UnixNano()
	cutoff := now - int64(time.Minute)

	st.mu.Lock()
	defer st.mu.Unlock()

	idx := sort.Search(len(st.timestamps), func(i int) bool { return st.timestamps[i] >= cutoff })
	if idx > 0 {
		n := copy(st.timestamps, st.timestamps[idx:])
		st.timestamps = st.timestamps[:n]
	}

	st.lastSeen = now

	if len(st.timestamps) >= limit {
		return ErrRPMExceeded
	}

	// 防御性 hard cap：避免恶意客户端导致 slice 无限增长
	if cap(st.timestamps) > 4*limit && len(st.timestamps) < limit/2 {
		newTS := make([]int64, len(st.timestamps), limit)
		copy(newTS, st.timestamps)
		st.timestamps = newTS
	}

	st.timestamps = append(st.timestamps, now)
	return nil
}

// gcLoop 周期性清理长时间未访问的 key state。
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
		st, ok := value.(*keyRPMState)
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
		r.log.Debug("businesskey InProcessRPM GC removed idle keys", slog.Int("removed", removed))
	}
}

// peekCount 仅测试用：返回当前 key 窗口内的 count（不裁剪、不追加）。
func (r *InProcessRPM) peekCount(keyID int64) int {
	v, ok := r.states.Load(keyID)
	if !ok {
		return 0
	}
	st, ok := v.(*keyRPMState)
	if !ok {
		return 0
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return len(st.timestamps)
}

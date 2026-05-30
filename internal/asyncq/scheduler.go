package asyncq

import (
	"time"

	"github.com/hibiken/asynq"
)

// Scheduler 是 Asynq Scheduler 的薄封装：按 cronspec 定时把**周期任务**入队（Unit 6b sweep 兜底）。
//
// 设计依据 ADR-0006：sweep（fetch reconciler / recover / expire / orphan-reserve）属 QueueLow 兜底
// tier，由本 Scheduler 定时入队，Server 的 worker 取走执行（享 asynq 重试/调度，与 submit/settle
// 同一执行模型）。Scheduler 内部持有独立 asynq client（NewScheduler 据 RedisConnOpt 建），
// Shutdown 时自行 Close，无需外部管理其连接。
//
// 与 Server/Client 一样：构造惰性（不连 Redis）；Redis 可达性由 Server 的启动 PingContext 统一
// fail-fast（同一 Redis，无需 Scheduler 再 ping）。
type Scheduler struct {
	inner *asynq.Scheduler
}

// NewScheduler 构造 Asynq Scheduler（时区固定 UTC，与账期 / 结构化日志时间口径一致）。
func NewScheduler(cfg Config) *Scheduler {
	opts := &asynq.SchedulerOpts{Location: time.UTC}
	if cfg.Logger != nil {
		opts.Logger = slogAdapter{l: cfg.Logger}
	}
	return &Scheduler{inner: asynq.NewScheduler(cfg.redisOpt(), opts)}
}

// Register 注册一个周期任务：按 cronspec（支持 robfig/cron 的 "@every 30s" 描述符）定时把类型为
// typ、空 payload 的任务入队到 queue。
//
// 用 asynq.Unique(uniqueTTL) 在 uniqueTTL 窗口内对 (typ, 空payload, queue) 去重（Redis 唯一锁，
// 跨副本一致）：多副本各自跑 Scheduler 时，一个窗口内只入队一个 sweep job（sweep 是全表幂等扫描，
// 一个窗口一个 job 足矣，避免 N 副本 N 次重复全表扫）。uniqueTTL 建议 ≈ cronspec 周期。
func (s *Scheduler) Register(cronspec, typ, queue string, uniqueTTL time.Duration) (entryID string, err error) {
	return s.inner.Register(cronspec, asynq.NewTask(typ, nil),
		asynq.Queue(queue),
		asynq.Unique(uniqueTTL),
	)
}

// Start 非阻塞启动 scheduler（内部起 cron 调度 + 入队 goroutine）。
func (s *Scheduler) Start() error { return s.inner.Start() }

// Shutdown 停止 scheduler（停 cron + Close 内部 client）；停机序列应在 Server.Shutdown 前调用，
// 避免停机过程中仍有新 sweep job 入队。
func (s *Scheduler) Shutdown() { s.inner.Shutdown() }

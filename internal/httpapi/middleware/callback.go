package middleware

import (
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// 回调端点专用中间件（plan §Unit 8：独立限速 + body 大小上限）。
//
// 回调入口是**公网未鉴权**（鉴权 = URL 路径 per-task token）；限速与 body 上限是 defense-in-depth，
// 防伪造回调洪泛 / 超大 body OOM（主防线仍是 token 校验 + 去抖，见 task.HandleCallback）。

// CallbackBodyLimitBytes 回调 body 上限：64 KiB（回调体仅小 JSON，且网关不解析其内容——回调体不可信）。
const CallbackBodyLimitBytes = 64 * 1024

// CallbackBodyLimit 给回调路由装 body size 上限（MaxBytesReader 封顶，防超大 body）。
func CallbackBodyLimit() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, CallbackBodyLimitBytes)
		c.Next()
	}
}

// CallbackThrottle 是回调端点的**全局令牌桶**限速器（非 per-IP：上游回调来自少数固定 IP）。
//
// 选择全局而非 per-key/IP：回调无业务 key（不能复用 BusinessRPM）；上游 IP 少且可信，全局桶足以
// 挡伪造洪泛。stdlib 自实现（避免新增直接依赖 golang.org/x/time/rate；ADR-0006 仅列其为 asynq
// 传递依赖）。并发安全（mutex）；allow(now) 收 now 入参便于确定性测试。
type CallbackThrottle struct {
	mu         sync.Mutex
	tokens     float64
	burst      float64
	ratePerSec float64
	last       time.Time
}

// NewCallbackThrottle 构造令牌桶：ratePerSec 稳态速率、burst 桶容量（初始满桶）。
func NewCallbackThrottle(ratePerSec, burst float64) *CallbackThrottle {
	if ratePerSec <= 0 {
		ratePerSec = 1
	}
	if burst < 1 {
		burst = 1
	}
	return &CallbackThrottle{
		tokens:     burst,
		burst:      burst,
		ratePerSec: ratePerSec,
		last:       time.Now(),
	}
}

// allow 按 now 补充令牌并尝试取 1 个；取到返 true。
//
// 时钟回拨（now < last，NTP 校正）：elapsed < 0 → **不补令牌**（fail-safe：只会更严格不会放宽），
// 但仍把 last 钳到 now——否则 last 卡在「未来」，回拨期间后续 elapsed 持续为负、refill 长期停摆。
// 钳位后时间一旦前进即从 now 正常 refill（ce-review adversarial/testing）。
func (t *CallbackThrottle) allow(now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if elapsed := now.Sub(t.last).Seconds(); elapsed > 0 {
		t.tokens = math.Min(t.burst, t.tokens+elapsed*t.ratePerSec)
	}
	t.last = now
	if t.tokens >= 1 {
		t.tokens--
		return true
	}
	return false
}

// Middleware 返回 gin 中间件：超速 → 429 abort（无 body，回调端点不需详细错误）。
func (t *CallbackThrottle) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !t.allow(time.Now()) {
			c.Status(http.StatusTooManyRequests)
			c.Abort()
			return
		}
		c.Next()
	}
}

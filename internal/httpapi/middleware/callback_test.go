package middleware

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCallbackThrottle_Allow(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	th := NewCallbackThrottle(10, 3) // 10 rps，burst 3
	th.last = base                   // 固定起点（构造时 last=now()，覆盖为确定值）
	th.tokens = 3

	// burst：同一时刻前 3 个通过，第 4 个拒。
	assert.True(t, th.allow(base))
	assert.True(t, th.allow(base))
	assert.True(t, th.allow(base))
	assert.False(t, th.allow(base), "桶空 → 拒")

	// 0.1s 后补 1 个令牌（10 rps × 0.1s = 1）。
	assert.True(t, th.allow(base.Add(100*time.Millisecond)))
	assert.False(t, th.allow(base.Add(100*time.Millisecond)))

	// 长时间后补满但封顶 burst=3。
	far := base.Add(10 * time.Second)
	assert.True(t, th.allow(far))
	assert.True(t, th.allow(far))
	assert.True(t, th.allow(far))
	assert.False(t, th.allow(far), "补满封顶 burst=3")
}

// TestCallbackThrottle_ClockBackwardsFailSafe 时钟回拨不补令牌（fail-safe：更严格不放宽），
// 且 last 被钳到回拨时刻，时间再前进即从该点正常 refill（不长期停摆）。
func TestCallbackThrottle_ClockBackwardsFailSafe(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	th := NewCallbackThrottle(10, 2) // 10 rps，burst 2
	th.last = base
	th.tokens = 2

	assert.True(t, th.allow(base))
	assert.True(t, th.allow(base))
	assert.False(t, th.allow(base), "桶空")

	// 时钟回拨 10s：elapsed<0 → 不补令牌 → 仍拒（不放宽）。
	assert.False(t, th.allow(base.Add(-10*time.Second)), "回拨不补令牌（fail-safe）")

	// last 已钳到回拨点；从该点前进 1s → 补 min(2, 10×1)=2，可再取。
	assert.True(t, th.allow(base.Add(-10*time.Second).Add(time.Second)), "钳位后正常 refill，不长期停摆")
}

// TestCallbackThrottle_Middleware 超速请求返 429。
func TestCallbackThrottle_Middleware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// burst=1 + 极慢补充 → 第 2 个请求必拒。
	th := NewCallbackThrottle(0.001, 1)
	r := gin.New()
	r.Use(th.Middleware())
	r.POST("/cb", func(c *gin.Context) { c.Status(http.StatusOK) })

	do := func() int {
		req := httptest.NewRequest(http.MethodPost, "/cb", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}
	assert.Equal(t, http.StatusOK, do(), "首个请求通过")
	assert.Equal(t, http.StatusTooManyRequests, do(), "第二个请求超速 → 429")
}

func TestCallbackBodyLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(CallbackBodyLimit())
	r.POST("/cb", func(c *gin.Context) {
		if _, err := io.ReadAll(c.Request.Body); err != nil {
			c.Status(http.StatusRequestEntityTooLarge)
			return
		}
		c.Status(http.StatusOK)
	})

	do := func(body string) int {
		req := httptest.NewRequest(http.MethodPost, "/cb", strings.NewReader(body))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}
	assert.Equal(t, http.StatusOK, do("small body"), "小 body 通过")
	require.Equal(t, http.StatusRequestEntityTooLarge,
		do(strings.Repeat("a", CallbackBodyLimitBytes+1)), "超 64KiB → 读 body 报错 → 413")
}

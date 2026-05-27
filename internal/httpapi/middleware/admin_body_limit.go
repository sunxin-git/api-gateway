package middleware

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
)

// AdminBodyLimitBytes admin 链统一 body 上限：64 KiB（与 audit request_hash 的 64KB cap 对齐）。
//
// 选择理由：
//   - 业务系统传 ID + amount + external_ref + metadata，理论 ≤ 几 KB
//   - 64KB 给 metadata JSON 留充裕余量
//   - 防 OOM 攻击（恶意客户端发 GB 级 body 引发内存爆）
//
// 实施位置：admin 链最前（在 AdminTokenAuth 之前），让恶意 body 在解析前就被拒。
const AdminBodyLimitBytes = 64 * 1024

// AdminBodyLimit 给 admin 路由组装配 body size 上限。
//
// 实现：包装 `c.Request.Body` 为 `http.MaxBytesReader`；后续读 body（gin bind / 手工 io.ReadAll）
// 超出会得到 *http.MaxBytesError；本中间件在 c.Next() 后检查并返 413。
//
// 注意：MaxBytesReader 在第一次 Read 到第 N+1 个字节时才触发；不是预检 Content-Length。
// 选择不预检 Content-Length 是因为：
//   - 攻击者可伪造 / 不发 Content-Length；预检不安全
//   - MaxBytesReader 在真实读取时触发；handler 拿到的 err 链含 *http.MaxBytesError，
//     middleware defer 中检查即可可靠捕获
//
// metric `tooLargeCounter` 是 pre-auth 阶段；不带 token_id 标签（此时未鉴权未知 token）。
func AdminBodyLimit(tooLargeCounter prometheus.Counter) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 包装 body；后续 bind / read 时若超出会返 *http.MaxBytesError
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, AdminBodyLimitBytes)

		c.Next()

		// 检查 handler 是否因 body 超限失败：c.Errors 中含 *http.MaxBytesError
		// 或 c.Writer.Status() 已被 handler 写为 413
		for _, ge := range c.Errors {
			var mbe *http.MaxBytesError
			if errors.As(ge.Err, &mbe) {
				if tooLargeCounter != nil {
					tooLargeCounter.Inc()
				}
				// 避免重复 WriteHeader
				if !c.Writer.Written() {
					c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{
						"error": gin.H{
							"code":       "payload_too_large",
							"message":    "请求体超过上限（64 KiB）",
							"request_id": GetRequestID(c),
						},
					})
				}
				return
			}
		}
	}
}

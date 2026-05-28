package middleware

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/sunxin-git/api-gateway/internal/relay"
)

// BusinessBodyLimitBytes 业务路由组 body 上限：1 MiB（plan §中间件 4 件套）。
//
// 选择理由（与 admin 64 KiB 上限差异）：
//   - chat completions 请求含完整 messages 历史；长上下文 32k token ≈ 100 KiB 文本
//   - 单图多模态请求 base64 编码后可达几百 KiB
//   - 1 MiB 留足余量同时防 OOM 攻击
//
// 实施位置：business 链最前（在 BusinessKeyAuth 之前），让恶意 body 在解析前就被拒。
const BusinessBodyLimitBytes = 1 * 1024 * 1024

// BusinessBodyLimit 给业务路由组装配 body size 上限。
//
// 实现：包装 c.Request.Body 为 http.MaxBytesReader；后续读 body（handler bind / 手工 read）
// 超出会得到 *http.MaxBytesError；本中间件在 c.Next() 后检查并返 413 OpenAI 兼容错误。
//
// metric tooLargeCounter（pre-auth 阶段，不带 token_id 标签；与 admin pattern 一致）。
func BusinessBodyLimit(tooLargeCounter prometheus.Counter) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, BusinessBodyLimitBytes)

		c.Next()

		for _, ge := range c.Errors {
			var mbe *http.MaxBytesError
			if errors.As(ge.Err, &mbe) {
				if tooLargeCounter != nil {
					tooLargeCounter.Inc()
				}
				if !c.Writer.Written() {
					relay.WriteErrorJSON(c,
						http.StatusRequestEntityTooLarge,
						relay.ErrTypeInvalidRequest,
						"payload_too_large",
						"请求体超过上限（1 MiB）",
					)
				}
				return
			}
		}
	}
}

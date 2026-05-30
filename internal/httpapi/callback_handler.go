package httpapi

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/sunxin-git/api-gateway/internal/task"
)

// 上游异步视频回调入口（plan §Unit 8 / ADR-0006 决策 5）。
//
// 设计要点：
//   - 由**上游**（火山）调用，**不**走业务 key 鉴权链；鉴权 = URL 路径里的 per-task token。
//   - 回调体**不可信**：仅作「去查」触发；真实终态与 usage 一律由网关 Poll 反查上游（HandleCallback 内）。
//   - token 校验 / 去抖 / 推进逻辑在 task 包（HandleCallback）；本层只做 HTTP 转换 + 状态码映射，
//     **绝不**记录 token 或含 token 的完整 URL（task_id 非秘密，可记）。

// CallbackProcessor 是回调处理的最小依赖（*task.Service 满足；测试可 fake）。
type CallbackProcessor interface {
	HandleCallback(ctx context.Context, taskID, token string) (task.CallbackOutcome, error)
}

// VideoCallbackHandler 处理 POST {task.CallbackPathPrefix}/:task_id/:token。
type VideoCallbackHandler struct {
	svc    CallbackProcessor
	logger *slog.Logger
}

// NewVideoCallbackHandler 构造回调 handler。
func NewVideoCallbackHandler(svc CallbackProcessor, logger *slog.Logger) *VideoCallbackHandler {
	return &VideoCallbackHandler{svc: svc, logger: logger}
}

// Handle 是 gin handler：解析路径 task_id/token → HandleCallback → 映射状态码。
//
// 状态码映射（不泄露任务存在性）：
//   - accepted / ignored(unknown|state) → 200（已触发反查 / 忽略；上游视为成功不重投）
//   - unauthorized（token 缺/错且任务在途）→ 401（不改状态）
//   - 瞬时内部错误（DB 抖动）→ 500（上游可重试；周期 sweep 亦兜底）
func (h *VideoCallbackHandler) Handle(c *gin.Context) {
	taskID := c.Param("task_id")
	token := c.Param("token")
	if taskID == "" || token == "" {
		c.Status(http.StatusNotFound) // 路由本要求两段；防御性
		return
	}

	outcome, err := h.svc.HandleCallback(c.Request.Context(), taskID, token)
	if err != nil {
		// task_id 可记（非秘密）；token / 完整 URL 绝不记。
		h.logger.Error("video callback: 处理失败（瞬时，上游可重试 / sweep 兜底）",
			slog.String("task_id", taskID), slog.String("err", err.Error()))
		c.Status(http.StatusInternalServerError)
		return
	}

	switch outcome {
	case task.CallbackUnauthorized:
		h.logger.Warn("video callback: token 校验失败，拒绝（不改状态）", slog.String("task_id", taskID))
		c.Status(http.StatusUnauthorized)
	case task.CallbackAccepted:
		h.logger.Info("video callback: 已触发 Poll 反查推进", slog.String("task_id", taskID))
		c.Status(http.StatusOK)
	default:
		// CallbackIgnoredUnknownTask / CallbackIgnoredState → 200 忽略（不泄露存在性 / 去抖）。
		c.Status(http.StatusOK)
	}
}

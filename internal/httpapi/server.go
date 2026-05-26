// Package httpapi 提供 HTTP 服务装配：Gin 引擎、中间件链、骨架端点。
//
// Phase 1 只提供 /healthz、/readyz、/metrics 三个端点；不嵌入前端、不连 DB。
// 业务路由由后续 Phase 在此文件外扩展。
package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/sunxin-git/api-gateway/internal/config"
	"github.com/sunxin-git/api-gateway/internal/httpapi/middleware"
	"github.com/sunxin-git/api-gateway/internal/obs"
)

// BuildInfo 进程构建信息，由 main.go 在初始化时填入（通常来自 ldflags）。
// Phase 1 默认值 "dev"。
type BuildInfo struct {
	Version string
	Commit  string
}

// ReadinessCheck 是一个就绪探针 callable，返回 nil 表示就绪。
// Phase 1 暂无任何依赖；Phase 2 可注册 DB.Ping 等。
type ReadinessCheck func(ctx context.Context) error

// Deps 是 Server 的外部依赖集合（显式注入，禁止全局状态）。
type Deps struct {
	Logger  *slog.Logger
	Metrics *obs.Metrics
	Build   BuildInfo
}

// Server 封装 Gin engine 与 http.Server，提供 graceful start/stop。
type Server struct {
	cfg      *config.Config
	deps     Deps
	engine   *gin.Engine
	httpSrv  *http.Server
	readyMu  sync.RWMutex
	readyChk map[string]ReadinessCheck
}

// NewServer 装配 Gin engine + 中间件链 + 骨架端点。
//
// 中间件链顺序（架构约束）：
//
//	recover → requestid → slog → otel → prom → cors
func NewServer(cfg *config.Config, deps Deps) *Server {
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()

	engine.Use(
		middleware.Recover(deps.Logger, deps.Metrics.PanicTotal),
		middleware.RequestID(),
		middleware.Slog(deps.Logger),
		middleware.OTel("api-gateway"),
		middleware.Prom(deps.Metrics.HTTPRequestDuration),
		middleware.CORS(cfg.CORSAllowedOrigins),
	)

	s := &Server{
		cfg:      cfg,
		deps:     deps,
		engine:   engine,
		readyChk: make(map[string]ReadinessCheck),
	}

	s.registerSkeletonRoutes()

	s.httpSrv = &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           engine,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s
}

// Engine 暴露底层 gin.Engine，方便后续 Phase 注册业务路由。
func (s *Server) Engine() *gin.Engine {
	return s.engine
}

// AddReadinessCheck 注册一个就绪探针。name 唯一；重名将覆盖。
// 设计意图：Phase 2 可注入 DB.Ping、Redis Ping 等。
func (s *Server) AddReadinessCheck(name string, check ReadinessCheck) {
	s.readyMu.Lock()
	defer s.readyMu.Unlock()
	s.readyChk[name] = check
}

// Start 启动 HTTP 服务（阻塞直到 listener 失败或 Shutdown 被调用）。
// 调用方通常在 goroutine 内调用并监听 ErrServerClosed。
func (s *Server) Start() error {
	return s.httpSrv.ListenAndServe()
}

// Shutdown 优雅停机：等待 ctx 内的存活请求完成；超时则强制 Close。
func (s *Server) Shutdown(ctx context.Context) error {
	if err := s.httpSrv.Shutdown(ctx); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			// 超时：强制关闭剩余连接。
			_ = s.httpSrv.Close()
		}
		return err
	}
	return nil
}

// registerSkeletonRoutes 注册 Phase 1 骨架端点。
func (s *Server) registerSkeletonRoutes() {
	s.engine.GET("/healthz", s.handleHealthz)
	s.engine.GET("/readyz", s.handleReadyz)
	s.engine.GET("/metrics", gin.WrapH(
		promhttp.HandlerFor(s.deps.Metrics.Registry, promhttp.HandlerOpts{}),
	))
}

func (s *Server) handleHealthz(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status":  "ok",
		"version": s.deps.Build.Version,
		"commit":  s.deps.Build.Commit,
	})
}

func (s *Server) handleReadyz(c *gin.Context) {
	s.readyMu.RLock()
	checks := make(map[string]ReadinessCheck, len(s.readyChk))
	for k, v := range s.readyChk {
		checks[k] = v
	}
	s.readyMu.RUnlock()

	results := make(map[string]string, len(checks))
	failed := false
	// 每个 check 设单独超时，避免单个慢探针拖垮整体。
	for name, check := range checks {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
		err := check(ctx)
		cancel()
		if err != nil {
			results[name] = err.Error()
			failed = true
		} else {
			results[name] = "ok"
		}
	}

	if failed {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"status": "not_ready",
			"checks": results,
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"status": "ready",
		"checks": results,
	})
}

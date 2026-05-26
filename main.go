// api-gateway 进程入口（Phase 1 骨架）。
//
// 启动流程：
//  1. 加载配置（默认值 → .env.local → 进程 env，env 胜出，缺失必填项 fail-fast）。
//  2. 初始化可观测性（slog logger / Prometheus metrics / OTel tracer provider）。
//  3. 装配 httpapi.Server（Gin engine + 中间件链 + 骨架端点）。
//  4. 监听 SIGINT/SIGTERM，收到信号后 30s graceful shutdown，超时强制 Close。
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/sunxin-git/api-gateway/internal/config"
	"github.com/sunxin-git/api-gateway/internal/httpapi"
	"github.com/sunxin-git/api-gateway/internal/obs"
)

// 通过 ldflags 注入；Phase 1 默认值。
//
//	go build -ldflags "-X main.version=v1.0.0 -X main.commit=abcdef"
var (
	version = "dev"
	commit  = "dev"
)

func main() {
	if err := run(); err != nil {
		// 用 stderr 直接打印（此时 logger 可能还未就绪）。
		fmt.Fprintf(os.Stderr, "[FATAL] api-gateway 启动失败: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// 第 1 步：加载配置。
	cfg, err := config.Load(".env.local")
	if err != nil {
		return fmt.Errorf("加载配置失败: %w", err)
	}

	// 第 2 步：初始化可观测性。
	logger, err := obs.NewLogger(cfg.LogLevel)
	if err != nil {
		return fmt.Errorf("初始化 logger 失败: %w", err)
	}
	slog.SetDefault(logger)

	metrics := obs.NewMetrics(version, commit)

	tp, err := obs.NewTracerProvider(cfg.OTelExporter)
	if err != nil {
		return fmt.Errorf("初始化 OTel tracer provider 失败: %w", err)
	}
	otel.SetTracerProvider(tp)
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tp.Shutdown(shutdownCtx); err != nil {
			logger.Error("关闭 OTel tracer provider 失败", slog.String("err", err.Error()))
		}
	}()

	logger.Info("api-gateway 启动",
		slog.String("version", version),
		slog.String("commit", commit),
		slog.String("http_addr", cfg.HTTPAddr),
		slog.String("log_level", cfg.LogLevel),
		slog.String("otel_exporter", cfg.OTelExporter),
		slog.Int("cors_allowed_origins", len(cfg.CORSAllowedOrigins)),
	)

	// 第 3 步：装配 HTTP server。
	srv := httpapi.NewServer(cfg, httpapi.Deps{
		Logger:  logger,
		Metrics: metrics,
		Build:   httpapi.BuildInfo{Version: version, Commit: commit},
	})

	// 第 4 步：信号驱动的优雅停机。
	serverErr := make(chan error, 1)
	go func() {
		if err := srv.Start(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
		close(serverErr)
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		logger.Info("收到信号，开始优雅停机", slog.String("signal", sig.String()))
	case err := <-serverErr:
		if err != nil {
			return fmt.Errorf("HTTP server 异常退出: %w", err)
		}
		return nil
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("HTTP server 停机异常", slog.String("err", err.Error()))
		// Shutdown 内部已调用 Close 兜底，这里只记录不再返回错误。
	}

	logger.Info("api-gateway 已退出")
	return nil
}

package obs

import (
	"context"
	"fmt"
	"os"

	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// 服务名常量，Phase 1 暂硬编码；后续可考虑由 config 注入。
const serviceName = "api-gateway"

// NewTracerProvider 构造并返回一个 *sdktrace.TracerProvider。
//
// exporter 取值：
//   - "stdout"：trace 写入进程 stdout（开发用）。
//   - "otlp" ：返回错误，Phase 1 暂未接 OTLP（待 Phase 2 引入 endpoint 配置）。
//
// 调用方负责在进程退出前调用 provider.Shutdown(ctx) 刷新 span。
func NewTracerProvider(exporter string) (*sdktrace.TracerProvider, error) {
	switch exporter {
	case "stdout":
		return newStdoutTracerProvider()
	case "otlp":
		return nil, fmt.Errorf("OTel exporter %q 在 Phase 1 暂未实现，请改用 stdout", exporter)
	default:
		return nil, fmt.Errorf("非法 OTel exporter %q（仅支持 stdout|otlp）", exporter)
	}
}

func newStdoutTracerProvider() (*sdktrace.TracerProvider, error) {
	exp, err := stdouttrace.New(
		stdouttrace.WithWriter(os.Stdout),
		stdouttrace.WithPrettyPrint(),
	)
	if err != nil {
		return nil, fmt.Errorf("创建 stdout trace exporter 失败: %w", err)
	}

	res, err := resource.New(context.Background(),
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("构造 OTel resource 失败: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	return tp, nil
}

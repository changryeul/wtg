// Package otelinit — WTG service 별 OpenTelemetry TracerProvider 초기화 helper.
//
// 운영 환경에서 mci-* service 가 W3C tracecontext + OTel span tree 를 발행해
// 외부 backend (Jaeger / Tempo / Datadog / Honeycomb 등) 와 호환.
//
// 사용:
//
//	shutdown, err := otelinit.Setup(ctx, otelinit.Options{
//	    ServiceName: "mci-api",
//	    Endpoint:    cfg.OtelEndpoint, // "otel-collector:4317" — 비면 비활성
//	})
//	if err != nil { return err }
//	defer shutdown(ctx)
//
// 초기화 후 otel.Tracer("...") 호출 시 자동으로 본 provider 사용.
package otelinit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"
	"google.golang.org/grpc/credentials/insecure"
)

// Options — Setup 옵션.
type Options struct {
	// ServiceName — Jaeger / Tempo 등에서 service 식별. 필수.
	ServiceName string

	// Endpoint — OTLP gRPC endpoint (예: "otel-collector:4317" 또는
	// "jaeger:4317"). 빈 값이면 stdout exporter (debug). "stdout" 명시도 동일.
	Endpoint string

	// Insecure — TLS 없이 OTLP gRPC 연결 (dev 환경). 운영은 별도 TLS 설정 후
	// false 권장.
	Insecure bool

	// SampleRatio — 0.0..1.0. 1.0 = 모두 샘플링 (dev). 운영 권장 0.01~0.1.
	// 0 이면 default 1.0 (전체).
	SampleRatio float64

	// Headers — OTLP 요청 헤더 (예: API key). 빈 가능.
	Headers map[string]string
}

// Setup — TracerProvider 등록 + W3C tracecontext propagator. 반환된 shutdown 을
// service 종료 시 호출 (남은 span flush).
//
// Endpoint 가 비어있거나 "stdout" 이면 stdout exporter — production 에는 사용 X.
func Setup(ctx context.Context, opt Options) (func(context.Context) error, error) {
	if opt.ServiceName == "" {
		return nil, errors.New("otelinit: ServiceName 필수")
	}

	exporter, err := buildExporter(ctx, opt)
	if err != nil {
		return nil, fmt.Errorf("otelinit: exporter: %w", err)
	}

	// resource — service.name 만 명시. Default resource (host / process / etc)
	// 와 schema 가 충돌할 수 있어 별도 attribute 만 직접 빌드.
	res := resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(opt.ServiceName),
	)

	sampler := sdktrace.AlwaysSample()
	if opt.SampleRatio > 0 && opt.SampleRatio < 1.0 {
		sampler = sdktrace.ParentBased(sdktrace.TraceIDRatioBased(opt.SampleRatio))
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter, sdktrace.WithBatchTimeout(5*time.Second)),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
	)
	otel.SetTracerProvider(tp)
	// W3C tracecontext + Baggage propagator — middleware 의 traceparent 와 호환.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return func(ctx context.Context) error {
		shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return tp.Shutdown(shutdownCtx)
	}, nil
}

// buildExporter — endpoint 에 따라 OTLP gRPC 또는 stdout 선택.
func buildExporter(ctx context.Context, opt Options) (sdktrace.SpanExporter, error) {
	ep := strings.TrimSpace(opt.Endpoint)
	if ep == "" || strings.EqualFold(ep, "stdout") {
		return stdouttrace.New(stdouttrace.WithPrettyPrint())
	}
	o := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(ep)}
	if opt.Insecure {
		o = append(o, otlptracegrpc.WithTLSCredentials(insecure.NewCredentials()))
	}
	if len(opt.Headers) > 0 {
		o = append(o, otlptracegrpc.WithHeaders(opt.Headers))
	}
	return otlptracegrpc.New(ctx, o...)
}

// SetupIfEnabled — main boilerplate 축소 helper. endpoint 비고 stdout=false
// 면 nil 반환 (no-op). 그 외엔 Setup 호출 후 shutdown 반환. Setup 실패는
// warn log + nil — 운영 fail-open.
//
// 사용:
//
//	if shutdown := otelinit.SetupIfEnabled(ctx, "mci-price",
//	    cfg.OtelEndpoint, cfg.OtelStdout, cfg.OtelInsecure, cfg.OtelSampleRatio,
//	    logger); shutdown != nil {
//	    defer shutdown(ctx)
//	}
func SetupIfEnabled(ctx context.Context, serviceName, endpoint string,
	stdout, insecure bool, sample float64, logger *slog.Logger,
) func(context.Context) error {
	if endpoint == "" && !stdout {
		return nil
	}
	ep := endpoint
	if stdout {
		ep = "stdout"
	}
	shutdown, err := Setup(ctx, Options{
		ServiceName: serviceName,
		Endpoint:    ep,
		Insecure:    insecure,
		SampleRatio: sample,
	})
	if err != nil {
		if logger != nil {
			logger.Warn("OTel Setup 실패 — span 비활성",
				slog.String("service", serviceName), slog.Any("error", err))
		}
		return nil
	}
	if logger != nil {
		logger.Info("OTel TracerProvider 활성",
			slog.String("service", serviceName), slog.String("endpoint", ep))
	}
	return shutdown
}

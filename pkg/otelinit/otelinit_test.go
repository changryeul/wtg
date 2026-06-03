package otelinit

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
)

func TestSetup_RejectsEmptyServiceName(t *testing.T) {
	_, err := Setup(context.Background(), Options{})
	if err == nil {
		t.Error("ServiceName 빈값에 에러 기대")
	}
}

// stdout exporter — endpoint 비면 default. provider 등록 + shutdown 정상.
func TestSetup_StdoutExporterDefault(t *testing.T) {
	ctx := context.Background()
	shutdown, err := Setup(ctx, Options{ServiceName: "mci-test"})
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown(ctx)

	// global tracer 가 sdk 의 것으로 교체됐는지 — Tracer 호출 panic 없으면 OK.
	tr := otel.Tracer("test")
	_, span := tr.Start(ctx, "test-span")
	span.End()
}

// stdout 명시.
func TestSetup_StdoutExplicit(t *testing.T) {
	ctx := context.Background()
	shutdown, err := Setup(ctx, Options{ServiceName: "mci-test", Endpoint: "stdout"})
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown(ctx)
}

// 잘못된 endpoint 도 stdouttrace + grpc dial 둘 다 실패하면 에러.
// grpc 는 lazy 라 비활성 endpoint 도 NewClient 성공할 수 있음 — 구현 확인용.
func TestSetup_GrpcEndpointLazy(t *testing.T) {
	ctx := context.Background()
	// 절대 안 떠 있는 host:port — grpc dial 은 lazy 라 Setup 자체는 성공 가능.
	shutdown, err := Setup(ctx, Options{
		ServiceName: "mci-test",
		Endpoint:    "127.0.0.1:1", // 존재 불가 port
		Insecure:    true,
	})
	if err == nil {
		// 정상: lazy dial — 실제 export 시점에 실패하지만 Setup 은 OK.
		_ = shutdown(ctx)
	}
	// 실패해도 OK — 호출 패턴 검증 (panic 없음).
}

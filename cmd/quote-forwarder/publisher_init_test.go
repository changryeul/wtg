package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

// 초기 gRPC 연결이 안 되는(mci-price 미기동) 상황에서도 newGRPCPublisher 는
// 치명적 실패(nil,err)로 프로세스를 죽이지 않고, publisher 를 반환해 supervisor
// 가 backoff 재연결하도록 한다 — 배포 롤링 재기동 시 health check false-negative
// (systemd Type=simple 이 즉시 active) 회피. 회귀 방지.
func TestNewGRPCPublisher_UnreachableIsNonFatal(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// 127.0.0.1:1 — 연결 거부되는 주소.
	p, err := newGRPCPublisher(context.Background(), logger, "127.0.0.1:1")
	if err != nil {
		t.Fatalf("초기 연결 실패가 치명적이면 안 됨: err=%v", err)
	}
	if p == nil {
		t.Fatal("publisher 가 nil — supervisor 로 재시도 못 함")
	}
	t.Cleanup(func() { _ = p.Close() })
}

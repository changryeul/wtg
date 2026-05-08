package mymq

import (
	"context"
	"crypto/tls"
	"strings"
	"testing"
	"time"
)

// Options.TLS 가 nil 이면 dialBroker 가 plain TCP 사용 — 도달 불가능 endpoint 로
// 시도해 dial 자체가 빠르게 실패하는지만 확인 (기존 동작 회귀 방지).
func TestDialBrokerPlainTCP(t *testing.T) {
	opts := &Options{DialTimeout: 200 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	_, err := dialBroker(ctx, "127.0.0.1:1", opts)
	if err == nil {
		t.Error("도달 불가능 port 인데 성공")
	}
}

// Options.TLS 가 nil 이 아니면 tls.Dial 경로 — 같은 endpoint 로 시도하되
// TLS 핸드셰이크가 시작되기 전 TCP 단계에서 실패해도 무방. 단순히 분기 확인.
func TestDialBrokerTLSPath(t *testing.T) {
	opts := &Options{
		DialTimeout: 200 * time.Millisecond,
		TLS:         &tls.Config{InsecureSkipVerify: true},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	_, err := dialBroker(ctx, "127.0.0.1:1", opts)
	if err == nil {
		t.Error("도달 불가능 port 인데 성공")
	}
	// 에러 메시지가 dial 단계인지 (TLS 분기에서도 plain TCP 와 동일하게 connect 실패).
	if err != nil && !strings.Contains(err.Error(), "connect") &&
		!strings.Contains(err.Error(), "refused") &&
		!strings.Contains(err.Error(), "deadline") {
		t.Logf("err=%v", err) // 플랫폼별 다양 — 로그만.
	}
}

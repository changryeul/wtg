package routing

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// 외부 etcd 가 없는 환경에서 동작 검증할 수 있는 영역만 단위 테스트.
// 실제 watch / put 동작은 etcd 통합 테스트 (별도 환경 변수로 활성화) 에서.

// dial 자체가 실패해야 하는 케이스 — 잘못된 endpoint.
func TestNewEtcdRegistryNoEndpoints(t *testing.T) {
	_, err := NewEtcdRegistry(context.Background(), EtcdRegistryOptions{})
	if err == nil {
		t.Error("빈 endpoints 인데 통과")
	}
}

// 도달 불가능한 endpoint 로 dial — DialTimeout 초과 시 에러.
func TestNewEtcdRegistryUnreachable(t *testing.T) {
	if testing.Short() {
		t.Skip("short 모드 — etcd dial 실패 대기 시간 절약")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := NewEtcdRegistry(ctx, EtcdRegistryOptions{
		Endpoints:   []string{"127.0.0.1:1"}, // unreachable
		DialTimeout: 500 * time.Millisecond,
	})
	if err == nil {
		t.Error("도달 불가능한 endpoint 인데 성공")
		return
	}
	// dial 실패 메시지 (etcd / context / 네트워크 중 하나) — 엄격 검증 X.
	if !strings.Contains(err.Error(), "etcd") && !errors.Is(err, context.DeadlineExceeded) {
		t.Logf("err=%v", err) // 환경에 따라 메시지 다양 — 로그만 남김.
	}
}

// keyOf / aliasFromKey 의 prefix 보정 동작.
func TestEtcdRegistryKeyHelpers(t *testing.T) {
	r := &EtcdRegistry{prefix: "wtg/routes/"}
	if got := r.keyOf("ORDER_NEW"); got != "wtg/routes/ORDER_NEW" {
		t.Errorf("keyOf=%q", got)
	}
	if got := r.aliasFromKey("wtg/routes/A"); got != "A" {
		t.Errorf("aliasFromKey=%q", got)
	}
	if got := r.aliasFromKey("other/A"); got != "" {
		t.Errorf("외부 prefix 인데 alias 추출됨: %q", got)
	}
}

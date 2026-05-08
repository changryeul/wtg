package policy

import (
	"context"
	"testing"
	"time"
)

// 외부 etcd 가 없는 환경 — 옵션 검증 및 헬퍼만.
func TestStartEtcdSyncMissingDeps(t *testing.T) {
	if _, err := StartEtcdSync(context.Background(), nil, EtcdSyncOptions{Endpoints: []string{"x"}}); err == nil {
		t.Error("nil engine 인데 통과")
	}
	if _, err := StartEtcdSync(context.Background(), NewEngine(nil), EtcdSyncOptions{}); err == nil {
		t.Error("빈 endpoints 인데 통과")
	}
}

func TestStartEtcdSyncUnreachable(t *testing.T) {
	if testing.Short() {
		t.Skip("etcd dial 실패 대기 시간 — short 모드 스킵")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := StartEtcdSync(ctx, NewEngine(nil), EtcdSyncOptions{
		Endpoints:   []string{"127.0.0.1:1"},
		DialTimeout: 500 * time.Millisecond,
	})
	if err == nil {
		t.Error("도달 불가능 endpoint 인데 성공")
	}
}

func TestSplitEndpoints(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"a", 1},
		{"a,b,c", 3},
		{" a ,, b ", 2},
	}
	for _, c := range cases {
		got := SplitEndpoints(c.in)
		if len(got) != c.want {
			t.Errorf("SplitEndpoints(%q)=%v, want %d", c.in, got, c.want)
		}
	}
}

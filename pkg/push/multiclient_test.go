package push

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestMultiClient_UserStickyRouting — user 같으면 항상 같은 인스턴스.
// 단순 hash mod N — 분배 결정성 검증.
func TestMultiClient_UserStickyRouting(t *testing.T) {
	// 4 endpoint stub.
	var hits [4]atomic.Uint64
	servers := make([]*httptest.Server, 4)
	for i := 0; i < 4; i++ {
		idx := i
		servers[i] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits[idx].Add(1)
			_ = json.NewEncoder(w).Encode(Result{Injected: true})
		}))
		defer servers[i].Close()
	}
	mc, err := NewMultiClient(MultiClientOptions{
		Endpoints: []string{servers[0].URL, servers[1].URL, servers[2].URL, servers[3].URL},
	})
	if err != nil {
		t.Fatalf("NewMultiClient: %v", err)
	}
	defer mc.Close()

	// 같은 user 10 회 push — 정확히 한 인스턴스만 hit 받아야.
	for i := 0; i < 10; i++ {
		if _, err := mc.Push(context.Background(), Message{User: "dealer01", Data: json.RawMessage(`"x"`)}); err != nil {
			t.Fatalf("Push: %v", err)
		}
	}
	expectedIdx := mc.IndexForUser("dealer01")
	if hits[expectedIdx].Load() != 10 {
		t.Errorf("dealer01 → idx=%d 만 10회 받아야, 실제 %d", expectedIdx, hits[expectedIdx].Load())
	}
	totalOther := uint64(0)
	for i, h := range hits {
		if i != expectedIdx {
			totalOther += h.Load()
		}
	}
	if totalOther != 0 {
		t.Errorf("dealer01 외 인스턴스 hit=%d (sticky 깨짐)", totalOther)
	}
}

// TestMultiClient_BroadcastFanout — user 빈값 → 모든 인스턴스 fan-out.
func TestMultiClient_BroadcastFanout(t *testing.T) {
	var hits [3]atomic.Uint64
	servers := make([]*httptest.Server, 3)
	for i := 0; i < 3; i++ {
		idx := i
		servers[i] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits[idx].Add(1)
			_ = json.NewEncoder(w).Encode(Result{Injected: true})
		}))
		defer servers[i].Close()
	}
	mc, err := NewMultiClient(MultiClientOptions{
		Endpoints: []string{servers[0].URL, servers[1].URL, servers[2].URL},
	})
	if err != nil {
		t.Fatalf("NewMultiClient: %v", err)
	}
	defer mc.Close()

	// broadcast 1 회 → 3 인스턴스 모두 hit.
	if _, err := mc.Push(context.Background(), Message{User: "", Data: json.RawMessage(`"halt"`)}); err != nil {
		t.Fatalf("Push: %v", err)
	}
	for i, h := range hits {
		if h.Load() != 1 {
			t.Errorf("인스턴스 %d hit=%d, 기대 1", i, h.Load())
		}
	}
}

// TestMultiClient_BroadcastPartialFailure — 1 인스턴스 fail 해도 broadcast 성공
// (한 곳이라도 OK 면 nil error 반환, errCount 만 누적).
func TestMultiClient_BroadcastPartialFailure(t *testing.T) {
	// 첫 인스턴스 fail (500), 나머지 OK.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()
	ok1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(Result{Injected: true})
	}))
	defer ok1.Close()

	mc, err := NewMultiClient(MultiClientOptions{
		Endpoints: []string{bad.URL, ok1.URL},
	})
	if err != nil {
		t.Fatalf("NewMultiClient: %v", err)
	}
	defer mc.Close()

	res, err := mc.Push(context.Background(), Message{Data: json.RawMessage(`"x"`)})
	if err != nil {
		t.Errorf("부분 실패는 nil err 기대, got %v", err)
	}
	if res == nil || !res.Injected {
		t.Errorf("최소 1 인스턴스 성공 시 Injected=true 기대")
	}
	if mc.ErrCounts()[0] != 1 {
		t.Errorf("bad 인스턴스 ErrCount=1 기대, got %d", mc.ErrCounts()[0])
	}
	if mc.ErrCounts()[1] != 0 {
		t.Errorf("ok 인스턴스 ErrCount=0 기대, got %d", mc.ErrCounts()[1])
	}
}

// TestMultiClient_AllInstancesFail — 모두 fail 시 error 반환.
func TestMultiClient_AllInstancesFail(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()

	mc, _ := NewMultiClient(MultiClientOptions{Endpoints: []string{bad.URL, bad.URL}})
	defer mc.Close()

	_, err := mc.Push(context.Background(), Message{Data: json.RawMessage(`"x"`)})
	if err == nil {
		t.Errorf("모든 인스턴스 fail 시 error 기대")
	}
}

// TestMultiClient_EmptyEndpoints — endpoint 미설정 시 error.
func TestMultiClient_EmptyEndpoints(t *testing.T) {
	if _, err := NewMultiClient(MultiClientOptions{}); err == nil {
		t.Errorf("Endpoints 비면 error 기대")
	}
}

// TestUserIndex_Distribution — hash mod N 의 분배가 어느 정도 균등한지.
// 1000 user 를 4 인스턴스에 분배 — 각 인스턴스 100~400 범위 (10x 편차 이내) 정상.
func TestUserIndex_Distribution(t *testing.T) {
	var counts [4]int
	for i := 0; i < 1000; i++ {
		idx := userIndex("user-"+itoa(i), 4)
		counts[idx]++
	}
	for i, c := range counts {
		if c < 100 || c > 400 {
			t.Errorf("인스턴스 %d 분배 count=%d (100~400 기대)", i, c)
		}
	}
}

// itoa — strconv.Itoa 의존 회피 (test 의존 최소).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

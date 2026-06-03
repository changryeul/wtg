package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/winwaysystems/wtg/internal/api/middleware"
	"github.com/winwaysystems/wtg/pkg/idempotency"
	"github.com/winwaysystems/wtg/pkg/mymq"
)

// doAuthBulk — 인증된 bulk 요청.
func doAuthBulk(t *testing.T, deps *Deps, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/tx/bulk", strings.NewReader(body))
	req = req.WithContext(middleware.ContextWithPrincipal(req.Context(), &middleware.Principal{
		Usid: "trader01", Channel: "WEB",
	}))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	BulkTransaction(deps)(rr, req)
	return rr
}

// 정상 흐름 — 3 item 모두 성공.
func TestBulk_AllSuccess(t *testing.T) {
	var calls atomic.Int32
	caller := &fakeCaller{
		reply: func(_ context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			calls.Add(1)
			return &mymq.Reply{Body: []byte(`{"id":"` + string(in.Rkey[:]) + `"}`)}, nil
		},
	}
	deps := quietDeps(caller)
	rr := doAuthBulk(t, deps, `{"items":[
		{"exchange":"ORDER","routing_key":"A"},
		{"exchange":"ORDER","routing_key":"B"},
		{"exchange":"ORDER","routing_key":"C"}
	]}`, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rr.Code)
	}
	if calls.Load() != 3 {
		t.Errorf("broker calls=%d, want 3", calls.Load())
	}
	var resp BulkResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Items) != 3 {
		t.Fatalf("items=%d, want 3", len(resp.Items))
	}
	for i, it := range resp.Items {
		if it.Status != http.StatusOK {
			t.Errorf("item[%d] status=%d, want 200", i, it.Status)
		}
		if it.Envelope == nil {
			t.Errorf("item[%d] envelope nil", i)
		}
	}
}

// stop_on_error=true — 두 번째 item 비즈니스 에러 시 세 번째는 not_attempted.
func TestBulk_StopOnError(t *testing.T) {
	var calls atomic.Int32
	caller := &fakeCaller{
		reply: func(_ context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			n := calls.Add(1)
			if n == 2 {
				return &mymq.Reply{Errn: mymq.ErrNoSvc, ErrMsg: "no service"}, nil
			}
			return &mymq.Reply{Body: []byte(`{"ok":true}`)}, nil
		},
	}
	deps := quietDeps(caller)
	rr := doAuthBulk(t, deps, `{
		"stop_on_error": true,
		"items":[
			{"exchange":"X","routing_key":"A"},
			{"exchange":"X","routing_key":"B"},
			{"exchange":"X","routing_key":"C"}
		]
	}`, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rr.Code)
	}
	if calls.Load() != 2 {
		t.Errorf("broker calls=%d, want 2 (stop after 2nd)", calls.Load())
	}
	var resp BulkResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.Items[0].Status != 200 || resp.Items[1].Envelope == nil || resp.Items[1].Envelope.Errn == 0 {
		t.Errorf("items[0/1] 비정상: %+v", resp.Items[:2])
	}
	if resp.Items[2].Error != "not_attempted" {
		t.Errorf("items[2] error=%q, want not_attempted", resp.Items[2].Error)
	}
}

// stop_on_error=false (default) — 첫 실패 후에도 끝까지 진행.
func TestBulk_ContinueOnError(t *testing.T) {
	var calls atomic.Int32
	caller := &fakeCaller{
		reply: func(_ context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			n := calls.Add(1)
			if n == 1 {
				return &mymq.Reply{Errn: mymq.ErrNoSvc, ErrMsg: "no service"}, nil
			}
			return &mymq.Reply{Body: []byte(`{"ok":true}`)}, nil
		},
	}
	deps := quietDeps(caller)
	rr := doAuthBulk(t, deps, `{"items":[
		{"exchange":"X","routing_key":"A"},
		{"exchange":"X","routing_key":"B"}
	]}`, nil)
	if calls.Load() != 2 {
		t.Errorf("broker calls=%d, want 2 (default continue)", calls.Load())
	}
	_ = rr
}

// items 비어있음 → 400.
func TestBulk_EmptyItems(t *testing.T) {
	rr := doAuthBulk(t, quietDeps(nil), `{"items":[]}`, nil)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", rr.Code)
	}
}

// items 너무 많음 → 400.
func TestBulk_TooManyItems(t *testing.T) {
	items := make([]string, bulkMaxItems+1)
	for i := range items {
		items[i] = `{"exchange":"X","routing_key":"R"}`
	}
	body := `{"items":[` + strings.Join(items, ",") + `]}`
	rr := doAuthBulk(t, quietDeps(nil), body, nil)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", rr.Code)
	}
}

// broker network 실패 — item.Status=5xx, error 메시지.
func TestBulk_BrokerNetworkError(t *testing.T) {
	caller := &fakeCaller{
		reply: func(_ context.Context, _ *mymq.FrameInput) (*mymq.Reply, error) {
			return nil, errors.New("connection lost")
		},
	}
	rr := doAuthBulk(t, quietDeps(caller), `{"items":[{"exchange":"X","routing_key":"A"}]}`, nil)
	if rr.Code != http.StatusOK {
		t.Errorf("status=%d, want 200 (bulk 자체는 처리)", rr.Code)
	}
	var resp BulkResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.Items[0].Status < 500 {
		t.Errorf("item status=%d, want >=500 (network error)", resp.Items[0].Status)
	}
}

// Idempotency-Key + bulk — 같은 키 + 같은 body 의 두 번째 호출이 캐시 hit.
func TestBulk_IdempotencyCachedReplay(t *testing.T) {
	var calls atomic.Int32
	caller := &fakeCaller{
		reply: func(_ context.Context, _ *mymq.FrameInput) (*mymq.Reply, error) {
			calls.Add(1)
			return &mymq.Reply{Body: []byte(`{"ok":true}`)}, nil
		},
	}
	deps := quietDeps(caller)
	deps.Idempotency = idempotency.NewMemoryStore(idempotency.Options{})
	body := `{"items":[
		{"exchange":"X","routing_key":"A"},
		{"exchange":"X","routing_key":"B"}
	]}`
	rr1 := doAuthBulk(t, deps, body, map[string]string{"Idempotency-Key": "BULK-1"})
	if rr1.Code != http.StatusOK {
		t.Fatalf("1차 status=%d", rr1.Code)
	}
	if calls.Load() != 2 {
		t.Fatalf("1차 calls=%d, want 2", calls.Load())
	}
	rr2 := doAuthBulk(t, deps, body, map[string]string{"Idempotency-Key": "BULK-1"})
	if calls.Load() != 2 {
		t.Errorf("2차 broker 추가 호출 발생: calls=%d", calls.Load())
	}
	if rr2.Header().Get("Idempotency-Cached") != "true" {
		t.Errorf("2차에 Idempotency-Cached=true 누락")
	}
	if rr1.Body.String() != rr2.Body.String() {
		t.Errorf("bulk 응답 본문 불일치")
	}
}

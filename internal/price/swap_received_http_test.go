package price

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func postJSON(t *testing.T, h http.HandlerFunc, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// received 주입 + delta 설정 → view 병합(effective) 확인.
func TestSwapHTTP_ReceivedDeltaView(t *testing.T) {
	store := NewSwapStore()

	// 1) 로이터 수신 주입.
	rec := postJSON(t, SwapReceivedInjectHandler(store), `{"updates":[{"pair":"USD/KRW","tenor":"M01","bid":2.5,"ask":2.7}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("received status=%d", rec.Code)
	}
	// 2) 운영자 조정(delta).
	rec = postJSON(t, SwapDeltaHandler(store), `{"updates":[{"pair":"USD/KRW","tenor":"M01","bid":0.05,"ask":-0.03}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("delta status=%d", rec.Code)
	}
	// 3) view — effective = received + delta.
	greq := httptest.NewRequest(http.MethodGet, "/", nil)
	grec := httptest.NewRecorder()
	SwapViewHandler(store)(grec, greq)
	var views []SwapView
	if err := json.Unmarshal(grec.Body.Bytes(), &views); err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 {
		t.Fatalf("views %d, want 1", len(views))
	}
	v := views[0]
	if !near(v.RecvBid, 2.5) || !near(v.DeltaBid, 0.05) || !near(v.EffBid, 2.55) || !near(v.EffAsk, 2.67) {
		t.Errorf("view=%+v", v)
	}
}

func TestSwapHTTP_NilStore(t *testing.T) {
	rec := postJSON(t, SwapReceivedInjectHandler(nil), `{}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("nil store status=%d, want 503", rec.Code)
	}
}

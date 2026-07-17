package price

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 주입(POST) 후 조회(GET) 왕복.
func TestSwapReceivedHTTP_InjectThenList(t *testing.T) {
	store := NewReceivedSwapStore()

	// 주입.
	body := `{"updates":[{"pair":"USD/KRW","tenor":"M01","bid":2.5,"ask":2.7}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/pricing/swap-received", strings.NewReader(body))
	rec := httptest.NewRecorder()
	SwapReceivedInjectHandler(store)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("inject status=%d body=%s", rec.Code, rec.Body.String())
	}
	var ir map[string]int
	_ = json.Unmarshal(rec.Body.Bytes(), &ir)
	if ir["applied"] != 1 {
		t.Errorf("applied=%d, want 1", ir["applied"])
	}

	// store 에 실제 반영됐는지.
	m, ok := store.Get("USD/KRW", "M01")
	if !ok || m.BidAmount != 2.5 || m.AskAmount != 2.7 {
		t.Errorf("store Get=%+v ok=%v", m, ok)
	}

	// 조회.
	greq := httptest.NewRequest(http.MethodGet, "/v1/pricing/swap-received", nil)
	grec := httptest.NewRecorder()
	SwapReceivedListHandler(store)(grec, greq)
	var list []SwapReceivedEntry
	if err := json.Unmarshal(grec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Pair != "USD/KRW" || list[0].Bid != 2.5 {
		t.Errorf("list=%+v", list)
	}
}

func TestSwapReceivedHTTP_InjectNilStore(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/pricing/swap-received", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	SwapReceivedInjectHandler(nil)(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("nil store status=%d, want 503", rec.Code)
	}
}

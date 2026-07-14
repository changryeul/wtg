package price

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestMarketStatusHandler(t *testing.T) {
	stats := BestStats{Symbols: map[string]BestSymbolStat{
		"USD/KRW": {ActiveSources: 2},
		"EUR/USD": {ActiveSources: 0}, // 전 source stale
	}}
	h := MarketStatusHandler(func() BestStats { return stats }, false)

	check := func(url string, wantOpen bool, wantActive int) {
		t.Helper()
		w := httptest.NewRecorder()
		h(w, httptest.NewRequest("GET", url, nil))
		var out struct {
			Open          bool `json:"open"`
			ActiveSymbols int  `json:"active_symbols"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("resp: %v", err)
		}
		if out.Open != wantOpen || out.ActiveSymbols != wantActive {
			t.Fatalf("%s → open=%v active=%d, want %v/%d", url, out.Open, out.ActiveSymbols, wantOpen, wantActive)
		}
	}
	check("/v1/pricing/market-status", true, 1)
	check("/v1/pricing/market-status?pair=USD/KRW", true, 1)
	check("/v1/pricing/market-status?pair=EUR/USD", false, 0) // stale 심볼 → 시세중단
	check("/v1/pricing/market-status?pair=GBP/USD", false, 0) // 미지 심볼
}

func TestMarketStatusHandler_NoBest(t *testing.T) {
	h := MarketStatusHandler(nil, false)
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest("GET", "/v1/pricing/market-status", nil))
	if w.Code != 503 {
		t.Fatalf("code=%d, want 503", w.Code)
	}
}

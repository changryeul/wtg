package price

// 시장 시세 생존 게이트 — GET /v1/pricing/market-status.
//
// WTR005 (주문가능 체크 lib) 의 mds SHM 게이트 (market_get NULL +
// sendclient_flag) 를 대체한다: open = staleness 필터를 통과한 active
// source 를 가진 심볼이 1개 이상 (pair 지정 시 해당 심볼만 판정).
// 주문 hot path 소비자 (wtg_price_market_open) 가 호출 — 응답 최소화.

import (
	"net/http"
	"time"
)

// MarketStatusHandler — GET /v1/pricing/market-status?market=BEST[&pair=USD/KRW]
func MarketStatusHandler(statsFn func() BestStats, devMode bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if devMode {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		}
		if statsFn == nil {
			swapPointError(w, http.StatusServiceUnavailable, "no_best", "BestConsumer 미구성")
			return
		}
		market := r.URL.Query().Get("market")
		if market == "" {
			market = "BEST"
		}
		pair := r.URL.Query().Get("pair")

		st := statsFn()
		active := 0
		for sym, s := range st.Symbols {
			if pair != "" && sym != pair {
				continue
			}
			if s.ActiveSources > 0 {
				active++
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"market":         market,
			"open":           active > 0,
			"active_symbols": active,
			"checked_at":     time.Now().UTC().Format(time.RFC3339),
		})
	}
}

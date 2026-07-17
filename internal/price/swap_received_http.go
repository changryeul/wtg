package price

import (
	"encoding/json"
	"net/http"

	"github.com/winwaysystems/wtg/pkg/pricing"
	"github.com/winwaysystems/wtg/pkg/session"
)

// swap_received_http.go — 로이터 수신 swap point 주입/조회 endpoint.
//   - POST /v1/pricing/swap-received : 피드(또는 mock)가 수신값 주입 → ReceivedSwapStore
//   - GET  /v1/pricing/swap-received : admin 화면 표시용 수신값 목록
//
// 수신값은 자동 적용되지 않는다. admin 이 표시·조정(delta) 후 기존
// POST /v1/pricing/swap 로 반영 → effective = received + delta.

// SwapReceivedEntry — 한 (pair, tenor) 의 수신 swap point.
type SwapReceivedEntry struct {
	Pair  string  `json:"pair"`
	Tenor string  `json:"tenor"`
	Bid   float64 `json:"bid"`
	Ask   float64 `json:"ask"`
}

// SwapReceivedRequest — POST /v1/pricing/swap-received 본문.
type SwapReceivedRequest struct {
	Updates []SwapReceivedEntry `json:"updates"`
}

// SwapReceivedInjectHandler — POST. 로이터 피드가 수신 swap point 주입.
func SwapReceivedInjectHandler(store *ReceivedSwapStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "swap_received_disabled"})
			return
		}
		var req SwapReceivedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_json", "message": err.Error()})
			return
		}
		n := 0
		for _, u := range req.Updates {
			if u.Pair == "" || u.Tenor == "" {
				continue
			}
			store.Set(session.Pair(u.Pair), pricing.Tenor(u.Tenor), u.Bid, u.Ask)
			n++
		}
		writeJSON(w, http.StatusOK, map[string]int{"applied": n})
	}
}

// SwapReceivedListHandler — GET. admin 표시용 수신값 목록.
func SwapReceivedListHandler(store *ReceivedSwapStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		out := []SwapReceivedEntry{}
		if store != nil {
			for k, v := range store.Snapshot() {
				out = append(out, SwapReceivedEntry{
					Pair: string(k.Pair), Tenor: string(k.Tenor),
					Bid: v.BidAmount, Ask: v.AskAmount,
				})
			}
		}
		writeJSON(w, http.StatusOK, out)
	}
}

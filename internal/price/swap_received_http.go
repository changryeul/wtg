package price

import (
	"encoding/json"
	"net/http"

	"github.com/winwaysystems/wtg/pkg/pricing"
	"github.com/winwaysystems/wtg/pkg/session"
)

// swap_received_http.go — swap point 주입/조정/조회 endpoint.
//   - POST /v1/pricing/swap-received : 로이터 피드가 수신값 주입 (add 규약 points)
//   - POST /v1/pricing/swap-delta    : 운영자(admin) 조정값 설정 (add 규약 points)
//   - GET  /v1/pricing/swap          : 병합 조회 (received+delta+effective) — admin 화면
//
// effective = received + delta (add 규약). 수신은 표시만, 조정 후 반영은 delta 설정.

// SwapEntry — 주입/조정 한 건.
type SwapEntry struct {
	Pair  string  `json:"pair"`
	Tenor string  `json:"tenor"`
	Bid   float64 `json:"bid"`
	Ask   float64 `json:"ask"`
}

// SwapEntriesRequest — POST 본문.
type SwapEntriesRequest struct {
	Updates []SwapEntry `json:"updates"`
}

func decodeSwapEntries(w http.ResponseWriter, r *http.Request, store *SwapStore, set func(session.Pair, pricing.Tenor, float64, float64)) {
	if store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "swap_store_disabled"})
		return
	}
	var req SwapEntriesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_json", "message": err.Error()})
		return
	}
	n := 0
	for _, u := range req.Updates {
		if u.Pair == "" || u.Tenor == "" {
			continue
		}
		set(session.Pair(u.Pair), pricing.Tenor(u.Tenor), u.Bid, u.Ask)
		n++
	}
	writeJSON(w, http.StatusOK, map[string]int{"applied": n})
}

// SwapReceivedInjectHandler — POST. 로이터 피드가 수신값 주입.
func SwapReceivedInjectHandler(store *SwapStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			decodeSwapEntries(w, r, nil, nil)
			return
		}
		decodeSwapEntries(w, r, store, store.SetReceived)
	}
}

// SwapDeltaHandler — POST. 운영자 조정값(delta) 설정.
func SwapDeltaHandler(store *SwapStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			decodeSwapEntries(w, r, nil, nil)
			return
		}
		decodeSwapEntries(w, r, store, store.SetDelta)
	}
}

// SwapViewHandler — GET. admin 화면용 병합 (received+delta+effective).
func SwapViewHandler(store *SwapStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		out := []SwapView{}
		if store != nil {
			out = store.ViewSnapshot()
		}
		writeJSON(w, http.StatusOK, out)
	}
}

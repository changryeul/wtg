package price

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/winwaysystems/wtg/pkg/pricing"
	"github.com/winwaysystems/wtg/pkg/quote"
	"github.com/winwaysystems/wtg/pkg/session"
)

// Phase 5 (forward) — 선물환 시세 snapshot 응답 DTO.
//
// 운영 흐름:
//   1. 클라이언트가 forward 거래 화면 진입 시 본 endpoint 1회 호출.
//   2. SPOT 호가 + 각 tenor 별 forward 호가 + swap_point 표시.
//   3. SPOT stream (SubscribeCustomerQuote) 의 tick 갱신을 받으면 클라이언트가
//      보유한 swap_bid/swap_ask 를 더해 forward 갱신 (재호출 X).
//   4. table_version 이 바뀌면 (운영자 swap 수정) 클라이언트가 본 endpoint 재호출.
//   5. 실 거래는 별도 POST /v1/quote/forward/lock 으로 quote_id 발급 (Phase 5b 예정).

type ForwardSnapshot struct {
	Pair         string                 `json:"pair"`
	Profile      string                 `json:"profile"`
	CustomerID   string                 `json:"customer_id,omitempty"`
	Spot         ForwardSnapshotSpot    `json:"spot"`
	Tenors       []ForwardSnapshotTenor `json:"tenors"`
	TableVersion int64                  `json:"table_version"`
	SnapshotTS   string                 `json:"snapshot_ts"`
}

// ForwardSnapshotSpot — 현재 SPOT 호가 (5-Layer 마진 적용 + raw).
type ForwardSnapshotSpot struct {
	Bid    float64 `json:"bid"`     // customer-applied SPOT bid
	Ask    float64 `json:"ask"`     // customer-applied SPOT ask
	RawBid float64 `json:"raw_bid"` // 원본 BEST bid (참고용)
	RawAsk float64 `json:"raw_ask"`
}

// ForwardSnapshotTenor — 한 tenor 의 forward 호가 + swap 분해.
//
// Bid/Ask 는 ApplyForCustomer 결과 — SPOT 마진 + swap 합산된 final.
// SwapBid/SwapAsk 는 순수 swap_point — 클라이언트가 SPOT stream tick 갱신 시
// 단순 합성 (forward_bid = customer_spot_bid - swap_bid) 으로 호가 갱신 가능.
type ForwardSnapshotTenor struct {
	Tenor   string  `json:"tenor"`
	Bid     float64 `json:"bid"`
	Ask     float64 `json:"ask"`
	SwapBid float64 `json:"swap_bid"`
	SwapAsk float64 `json:"swap_ask"`
}

// ForwardSnapshotDeps — handler 의존성. cmd/mci-price 가 wire.
type ForwardSnapshotDeps struct {
	Store *pricing.Store
	Best  *BestConsumer
	// Cross — 옵션. 있으면 BEST cache miss 시 cross 합성 cache 도 시도
	// → forward-snapshot 이 cross pair (예: JPY/KRW) 도 응답 가능.
	Cross *CrossRateConsumer
	// Swap — 옵션. received+delta effective swap 원천 (add 규약). 있으면 forward 는
	// spot(margined) + effective 로 산정 (mds/algo 와 동일 규약). nil 이면 tenor 미노출.
	Swap *SwapStore
}

// ForwardSnapshotHandler — GET /v1/quote/forward-snapshot
//
// Query:
//
//	pair        : "USD/KRW" (필수)
//	profile     : "WEB.BRANCH.VIP" (필수)
//	customer_id : 선택. 채워지면 5-Layer (HQ+Site+Customer+Window) 적용.
//
// 응답: ForwardSnapshot JSON. DevMode 한정 CORS:* 노출.
func ForwardSnapshotHandler(deps ForwardSnapshotDeps, devMode bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if devMode {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		}
		pair := r.URL.Query().Get("pair")
		profileKey := r.URL.Query().Get("profile")
		customerID := r.URL.Query().Get("customer_id")
		if pair == "" || profileKey == "" {
			writeForwardErr(w, http.StatusBadRequest, "pair, profile 쿼리 필수")
			return
		}
		profile, err := session.ParseProfileKey(profileKey)
		if err != nil {
			writeForwardErr(w, http.StatusBadRequest, "invalid profile: "+err.Error())
			return
		}

		// 1. 현재 BEST SPOT 호가 조회.
		if deps.Best == nil {
			writeForwardErr(w, http.StatusServiceUnavailable, "BEST consumer 미활성")
			return
		}
		best := deps.Best.Stats()
		sym := strings.ReplaceAll(pair, "/", "")
		var rawBid, rawAsk float64
		if spotStat, ok := best.Symbols[sym]; ok {
			rawBid, rawAsk = spotStat.BestBid, spotStat.BestAsk
		} else if deps.Cross != nil {
			// BEST miss → cross consumer 의 last emit cache 시도.
			// stale 한 cross 호가는 forward-snapshot 으로 새지 않게 거부 —
			// 분쟁 재계산 같은 운영 시점은 신선 데이터만 사용 (auth.md 정책).
			bid, ask, _, isStale, ok := deps.Cross.LatestCross(session.Pair(pair))
			if !ok {
				writeForwardErr(w, http.StatusNotFound, "no BEST/cross snapshot for "+sym)
				return
			}
			if isStale {
				writeForwardErr(w, http.StatusNotFound, "cross snapshot stale for "+sym)
				return
			}
			rawBid, rawAsk = bid, ask
		} else {
			writeForwardErr(w, http.StatusNotFound, "no BEST snapshot for symbol "+sym)
			return
		}

		now := time.Now()
		spotRaw := quote.Quote{
			Pair: session.Pair(pair),
			Bid:  rawBid,
			Ask:  rawAsk,
			TS:   now,
		}

		tbl := deps.Store.Load()
		if tbl == nil {
			writeForwardErr(w, http.StatusServiceUnavailable, "PricingTable 미로드")
			return
		}

		// 2. SPOT customer-applied (마진만, swap 무관).
		spotCQ := tbl.ApplyForCustomer(spotRaw, profile, pricing.TenorSpot, now, customerID)

		// 3~4. forward = spot(margined) + effective swap[tenor] (add 규약, mds/algo 동일).
		//   swap 은 시장 레벨(received+delta) — customer 무관. tenor 는 swap store 에서.
		ts := []ForwardSnapshotTenor{}
		if deps.Swap != nil {
			pr := session.Pair(pair)
			tenors := deps.Swap.Tenors(pr)
			sort.Slice(tenors, func(i, j int) bool { return tenors[i] < tenors[j] })
			for _, tn := range tenors {
				eff, ok := deps.Swap.Effective(pr, tn)
				if !ok {
					continue
				}
				ts = append(ts, ForwardSnapshotTenor{
					Tenor:   string(tn),
					Bid:     spotCQ.Bid + eff.BidAmount, // forward = spot + swap
					Ask:     spotCQ.Ask + eff.AskAmount,
					SwapBid: eff.BidAmount, // effective (received+delta)
					SwapAsk: eff.AskAmount,
				})
			}
		}

		out := ForwardSnapshot{
			Pair:       pair,
			Profile:    profileKey,
			CustomerID: customerID,
			Spot: ForwardSnapshotSpot{
				Bid:    spotCQ.Bid,
				Ask:    spotCQ.Ask,
				RawBid: rawBid,
				RawAsk: rawAsk,
			},
			Tenors:       ts,
			TableVersion: tbl.Version,
			SnapshotTS:   now.UTC().Format(time.RFC3339),
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}
}

func writeForwardErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

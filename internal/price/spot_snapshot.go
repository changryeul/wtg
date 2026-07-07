package price

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/winwaysystems/wtg/pkg/pricing"
	"github.com/winwaysystems/wtg/pkg/quote"
	"github.com/winwaysystems/wtg/pkg/session"
)

// Phase 5 (S2) — Spot-only 고속 snapshot.
//
// 매칭 엔진이 spot 거래 원가 계산을 위해 호출하는 lite endpoint. ForwardSnapshot
// 의 spot 부분만 잘라낸 형태 — forward tenor 루프 (ApplyForCustomer × N tenor)
// 를 skip 해 latency 절감. spot 거래만 다루는 hot path 용.
//
// 운영 흐름:
//   1. 매칭 엔진이 spot 거래 진입 시 본 endpoint 1회 호출 (단일 또는 다중 pair).
//   2. 응답 = 각 pair 의 customer-applied bid/ask + raw 시장가 + table_version.
//   3. SPOT stream tick 갱신은 SubscribeCustomerQuote 로 받고 본 endpoint
//      재호출 없음 — table_version 이 바뀌면 재호출.
//
// forward 거래는 GET /v1/quote/forward-snapshot 사용.
//
// Bulk:
//   ?pair=USD/KRW,EUR/KRW,JPY/KRW → 한 호출로 3 pair 응답.
//   응답 형태는 단일/다중 동일 array — 매칭 엔진의 parse 일관성 확보.

// SpotSnapshotEntry — 단일 pair 의 spot 호가 + 마진 정보.
type SpotSnapshotEntry struct {
	Pair      string  `json:"pair"`
	Bid       float64 `json:"bid"`        // customer-applied
	Ask       float64 `json:"ask"`        // customer-applied
	RawBid    float64 `json:"raw_bid"`    // 시장 BEST bid
	RawAsk    float64 `json:"raw_ask"`    // 시장 BEST ask
	Spread    float64 `json:"spread"`     // ask - bid (customer-applied)
	RawSpread float64 `json:"raw_spread"` // raw 시장 spread
	Source    string  `json:"source"`     // "BEST" 또는 "CROSS"
}

// SpotSnapshot — 다중 pair 응답.
type SpotSnapshot struct {
	Profile      string              `json:"profile"`
	CustomerID   string              `json:"customer_id,omitempty"`
	TableVersion int64               `json:"table_version"`
	SnapshotTS   string              `json:"snapshot_ts"`
	Spots        []SpotSnapshotEntry `json:"spots"`
	// Missing — BEST/cross 모두 miss 한 pair (운영자가 시드 누락 / cooker 단절
	// 즉시 인지). hot path 의 매칭 엔진은 이 list 가 비어있어야 정상.
	Missing []string `json:"missing,omitempty"`
}

// SpotSnapshotHandler — GET /v1/quote/spot
//
// Query:
//
//	pair        : "USD/KRW" 단일 또는 콤마 구분 다중 "USD/KRW,EUR/KRW" (필수)
//	profile     : "WEB.BRANCH.VIP" (필수)
//	customer_id : 선택. 채워지면 5-Layer (HQ+Site+Customer+Window) 적용.
//
// 응답: SpotSnapshot JSON. ForwardSnapshotDeps 재사용 (Best/Cross/Store 동일).
func SpotSnapshotHandler(deps ForwardSnapshotDeps, devMode bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if devMode {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		}
		pairStr := r.URL.Query().Get("pair")
		profileKey := r.URL.Query().Get("profile")
		customerID := r.URL.Query().Get("customer_id")
		if pairStr == "" || profileKey == "" {
			writeForwardErr(w, http.StatusBadRequest, "pair, profile 쿼리 필수 (pair 는 콤마 구분 다중 가능)")
			return
		}
		profile, err := session.ParseProfileKey(profileKey)
		if err != nil {
			writeForwardErr(w, http.StatusBadRequest, "invalid profile: "+err.Error())
			return
		}
		if deps.Best == nil {
			writeForwardErr(w, http.StatusServiceUnavailable, "BEST consumer 미활성")
			return
		}
		tbl := deps.Store.Load()
		if tbl == nil {
			writeForwardErr(w, http.StatusServiceUnavailable, "PricingTable 미로드")
			return
		}

		// 다중 pair 파싱 — 빈 토큰 / 중복은 무시.
		pairs := splitPairsCSV(pairStr)
		now := time.Now()
		out := SpotSnapshot{
			Profile:      profileKey,
			CustomerID:   customerID,
			TableVersion: tbl.Version,
			SnapshotTS:   now.UTC().Format(time.RFC3339),
			Spots:        make([]SpotSnapshotEntry, 0, len(pairs)),
		}

		// BEST stats 한 번만 조회 — 다중 pair 호출에서 lookup 비용 절감.
		best := deps.Best.Stats()
		for _, pair := range pairs {
			rawBid, rawAsk, source, ok := lookupSpotRaw(deps, best, pair)
			if !ok {
				out.Missing = append(out.Missing, pair)
				continue
			}
			spotRaw := quote.Quote{
				Pair: session.Pair(pair),
				Bid:  rawBid,
				Ask:  rawAsk,
				TS:   now,
			}
			cq := tbl.ApplyForCustomer(spotRaw, profile, pricing.TenorSpot, now, customerID)
			out.Spots = append(out.Spots, SpotSnapshotEntry{
				Pair:      pair,
				Bid:       cq.Bid,
				Ask:       cq.Ask,
				RawBid:    rawBid,
				RawAsk:    rawAsk,
				Spread:    cq.Ask - cq.Bid,
				RawSpread: rawAsk - rawBid,
				Source:    source,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}
}

// lookupSpotRaw — BEST → cross 순으로 spot 호가 조회. forward-snapshot 의
// inline 로직을 함수로 추출 (bulk 호출 시 코드 중복 회피 + 일관 동작).
func lookupSpotRaw(deps ForwardSnapshotDeps, best BestStats, pair string) (bid, ask float64, source string, ok bool) {
	sym := strings.ReplaceAll(pair, "/", "")
	if s, found := best.Symbols[sym]; found {
		return s.BestBid, s.BestAsk, "BEST", true
	}
	if deps.Cross != nil {
		b, a, _, isStale, ok := deps.Cross.LatestCross(session.Pair(pair))
		if ok && !isStale {
			return b, a, "CROSS", true
		}
	}
	return 0, 0, "", false
}

// splitPairsCSV — 콤마 구분 입력에서 pair 추출. 빈/공백 trim + 중복 제거.
func splitPairsCSV(s string) []string {
	parts := strings.Split(s, ",")
	seen := make(map[string]struct{}, len(parts))
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		t := strings.TrimSpace(p)
		if t == "" {
			continue
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}

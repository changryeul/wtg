package price

import (
	"net/http"

	"github.com/winwaysystems/wtg/pkg/pricing"
	"github.com/winwaysystems/wtg/pkg/session"
)

// CustomerMarginResponse — 매칭엔진(mat) 의 f_get_cust_prc(WLM003) 가 조회하는
// 고객 "영업점마진". mat-side C shim(wtgmrgn_wtg)/WLM003 이 이 JSON 을 PAIRMRGN_ST
// 로 매핑한다. 방향 semantic:
//   - bid_margin: 매도(SELL) 측 — 고객이 파는 bid 쪽 마진 (quote 의 bid_delta 대응)
//   - ask_margin: 매입(BUY)  측 — 고객이 사는 ask 쪽 마진 (quote 의 ask_delta 대응)
//
// 표시가 = 체결가 정합: 고객 스프레드(customer_margin, override)는 고객이 보는 quote
// 의 **총** 스프레드다 (quote = BEST ± delta). 엔진 체결가는 본점마진(SHM, CMG019M
// 미러 = etcd hq_margin) 위에 영업점마진을 **가산**하므로, 여기서는 총스프레드에서
// 본점마진을 뺀 영업점 몫만 반환한다:
//   체결가 = 시세 + 본점(hq) + 영업점(delta - hq) = 시세 + delta = 고객이 본 quote ✓
// hq 조회는 tier 와일드카드("") — fx-sync 가 CMG019M 그룹마진을 tier "" 로 미러한다.
type CustomerMarginResponse struct {
	CustomerID string  `json:"customer_id"`
	Pair       string  `json:"pair"`
	Pdcd       string  `json:"pdcd"`       // SPT | FWD
	Dcd        string  `json:"dcd"`        // 마진유형구분 — 1:금액 (WTG delta 는 금액 기준)
	BidMargin  float64 `json:"bid_margin"` // 매도(SELL/bid)측 영업점마진 = max(0, bid_delta - hq.bid)
	AskMargin  float64 `json:"ask_margin"` // 매입(BUY/ask)측 영업점마진 = max(0, ask_delta - hq.ask)
	HQBid      float64 `json:"hq_bid"`     // 참조 — 차감된 본점마진 (bid 측)
	HQAsk      float64 `json:"hq_ask"`     // 참조 — 차감된 본점마진 (ask 측)
	Found      bool    `json:"found"`      // 고객 규칙 매칭 여부. false 면 마진 0 (본점만 반영된 체결).
}

// CustomerMarginHandler — GET /v1/customer-margin?cust=<no>&pair=USD/KRW&pdcd=SPT
//
// PricingTable.CustomerMargin 에서 (cust, pair) 매칭 규칙의 delta 에서 hq_margin
// (tier "" 와일드카드) 을 차감해 영업점 몫을 반환한다. CustomerMargin 은 priority
// desc 정렬 — 첫 매칭 사용. 미매칭이면 마진 0 (found=false) — 체결가 = 시세 + 본점.
func CustomerMarginHandler(store *pricing.Store, dev bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if dev {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		}
		cust := r.URL.Query().Get("cust")
		pair := r.URL.Query().Get("pair")
		pdcd := r.URL.Query().Get("pdcd")
		if pdcd == "" {
			pdcd = "SPT"
		}
		resp := CustomerMarginResponse{CustomerID: cust, Pair: pair, Pdcd: pdcd, Dcd: "1"}
		if store != nil && cust != "" {
			if tbl := store.Load(); tbl != nil {
				p := session.Pair(pair)
				// 본점마진 (tier "" 와일드카드) — CMG019M 미러값.
				if hq, ok := tbl.HQMargin[pricing.HQKey{Pair: p, Tier: ""}]; ok {
					resp.HQBid, resp.HQAsk = hq.BidAmount, hq.AskAmount
				}
				for _, rule := range tbl.CustomerMargin {
					if rule.CustomerID != cust {
						continue
					}
					if rule.Pair != "" && rule.Pair != p {
						continue
					}
					resp.BidMargin = max(0, rule.BidDelta-resp.HQBid)
					resp.AskMargin = max(0, rule.AskDelta-resp.HQAsk)
					resp.Found = true
					break
				}
			}
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

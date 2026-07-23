package price

import (
	"net/http"

	"github.com/winwaysystems/wtg/pkg/pricing"
	"github.com/winwaysystems/wtg/pkg/session"
)

// CustomerMarginResponse — 매칭엔진(mat) 의 f_get_cust_prc(WLM003) 가 로컬 SHM 대신
// mci-price 를 SoT 로 조회하는 고객 영업점마진. mat-side C shim(wtgmrgn_wtg) 가 이
// JSON 을 PAIRMRGN_ST 로 매핑한다 (bid_margin→d_spt_bymg, ask_margin→d_spt_slmg,
// dcd→s_spt_bomg_dcd). 마진 SoT 를 WTG 로 일원화하는 아키텍처의 mat 진입점.
type CustomerMarginResponse struct {
	CustomerID string  `json:"customer_id"`
	Pair       string  `json:"pair"`
	Pdcd       string  `json:"pdcd"`       // SPT | FWD
	Dcd        string  `json:"dcd"`        // 마진유형구분 — 1:금액 (WTG delta 는 금액 기준)
	BidMargin  float64 `json:"bid_margin"` // 매입마진 (d_spt_bymg)
	AskMargin  float64 `json:"ask_margin"` // 매도마진 (d_spt_slmg)
	Found      bool    `json:"found"`      // 고객 규칙 매칭 여부. false 면 마진 0 (raw 체결).
}

// CustomerMarginHandler — GET /v1/customer-margin?cust=<no>&pair=USD/KRW&pdcd=SPT
//
// PricingTable.CustomerMargin 에서 (cust, pair) 매칭 규칙의 BidDelta/AskDelta 를 반환한다.
// CustomerMargin 은 priority desc 로 정렬돼 있어 첫 매칭을 사용. 미매칭이면 마진 0
// (found=false) — mat 은 raw 시세로 체결한다. 본점그룹마진은 WTG 미보유이므로 mat shim
// 이 0 으로 채운다.
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
				for _, rule := range tbl.CustomerMargin {
					if rule.CustomerID != cust {
						continue
					}
					if rule.Pair != "" && rule.Pair != p {
						continue
					}
					resp.BidMargin = rule.BidDelta
					resp.AskMargin = rule.AskDelta
					resp.Found = true
					break
				}
			}
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

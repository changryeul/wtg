package fxsync

import "github.com/winwaysystems/wtg/pkg/pricing"

// CustomerSpread — 고객별 스프레드 1건. WTG pricing 의 CustomerMargin(override)로 미러.
//
// 적용(엔진 engine.go override): bid = BEST − BidDelta, ask = BEST + AskDelta.
// Mode=override 이므로 HQ/Site(tier) 마진은 무시된다 — 즉 고객 스프레드가 tier 를
// 대체한다. 스프레드 미등록 고객만 tier/HQ margin 으로 fallback (자동, 엔진 무변경).
//
// DB column 매핑 (향후 Oracle backend — 고객 스프레드 마스터):
//
//	Usid     ← 고객 로그인 ID. customerID = usid (edge 가 ws 접속 시 usid 로 등록).
//	           스프레드 DB 가 별도 고객번호를 쓰면 usid↔고객번호 매핑 seam 필요.
//	Pair     ← 통화쌍 (CRNC_PAIR_ID → "USD/KRW" 형식)
//	BidDelta ← 매수 스프레드 (BEST 로부터)
//	AskDelta ← 매도 스프레드
//	Active   ← 사용여부 (Y/N) — false 는 CustomerMargin 에서 제외(→ tier fallback)
type CustomerSpread struct {
	Usid     string  `json:"usid"`
	Pair     string  `json:"pair"`
	BidDelta float64 `json:"bid_delta"`
	AskDelta float64 `json:"ask_delta"`
	Active   bool    `json:"active"`
}

// CustomerSpreads — 목록.
type CustomerSpreads []CustomerSpread

// customerSpreadsToEntries — CustomerSpread → pricing.CustomerEntryDoc(Mode=override).
// inactive 는 제외 (그 고객은 CustomerMargin 미등록 → tier/HQ margin fallback).
func customerSpreadsToEntries(cs CustomerSpreads) []pricing.CustomerEntryDoc {
	out := make([]pricing.CustomerEntryDoc, 0, len(cs))
	for _, c := range cs {
		if !c.Active || c.Usid == "" {
			continue
		}
		out = append(out, pricing.CustomerEntryDoc{
			CustomerID: c.Usid,
			Pair:       pairFromString(c.Pair),
			BidDelta:   c.BidDelta,
			AskDelta:   c.AskDelta,
			Mode:       "override", // 고객 스프레드 = 절대 마진 (tier 무시)
		})
	}
	return out
}

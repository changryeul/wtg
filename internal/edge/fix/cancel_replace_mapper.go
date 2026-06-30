package fix

import (
	"errors"
	"fmt"

	"github.com/quickfixgo/field"
	"github.com/quickfixgo/fix44/ordercancelreplacerequest"
	"github.com/quickfixgo/fix44/ordercancelrequest"
)

// cancel_replace_mapper.go — Phase C. OrderCancelRequest (35=F) +
// OrderCancelReplaceRequest (35=G) 의 FIX → OrderEnvelope 변환.
//
// 매매 엔진의 alias handler 가 envelope.Op 봐서 dispatch:
//   op="new"     → 신규 주문 (Phase A/B)
//   op="cancel"  → 주문 취소 (Phase C)
//   op="replace" → 주문 정정 (Phase C)
//
// 한 alias (`Counterparty.OrderAlias`) 가 모든 lifecycle 처리 — 운영 룰 단순.

// mapOrderCancelRequest — FIX 4.4 OrderCancelRequest(35=F) → OrderEnvelope.
//
// 필수 tag: OrigClOrdID(41) / ClOrdID(11) / Side(54) / TransactTime(60).
// 옵션: OrderID(37), OrderQty(38), Symbol(55).
func mapOrderCancelRequest(cr ordercancelrequest.OrderCancelRequest) (OrderEnvelope, error) {
	var env OrderEnvelope
	env.Op = "cancel"

	// OrigClOrdID (필수) — 취소 대상 주문의 원래 ClOrdID.
	var orig field.OrigClOrdIDField
	if err := cr.Get(&orig); err != nil {
		return env, errors.New("OrigClOrdID(41) 필수")
	}
	env.OrigClientOrderID = orig.String()

	// ClOrdID (필수) — 본 취소 요청의 ID.
	var cl field.ClOrdIDField
	if err := cr.Get(&cl); err != nil {
		return env, errors.New("ClOrdID(11) 필수")
	}
	env.ClientOrderID = cl.String()

	// Side (필수).
	var side field.SideField
	if err := cr.Get(&side); err != nil {
		return env, errors.New("Side(54) 필수")
	}
	switch s := side.String(); s {
	case "1":
		env.Side = "buy"
	case "2":
		env.Side = "sell"
	default:
		return env, fmt.Errorf("Side(54) 미지원: %q", s)
	}

	// OrderID (옵션) — 엔진 채번 ID. 클라가 가진다면 첨부.
	var oid field.OrderIDField
	if err := cr.Get(&oid); err == nil {
		env.OrderID = oid.String()
	}

	// Symbol (옵션 in FIX 4.4 — 비즈니스 일관성 위해 보존).
	var sym field.SymbolField
	if err := cr.Get(&sym); err == nil {
		env.Symbol = sym.String()
	}

	// OrderQty (옵션 — 원본 주문 수량 확인용).
	var qty field.OrderQtyField
	if err := cr.Get(&qty); err == nil {
		q, _ := qty.Value().Float64()
		env.Qty = q
	}

	// Layer 3 — raw_fix 보존.
	env.RawFix = extractRawFix(cr.Body.FieldMap)
	return env, nil
}

// mapOrderCancelReplaceRequest — FIX 4.4 OrderCancelReplaceRequest(35=G) →
// OrderEnvelope.
//
// 필수: OrigClOrdID(41) / ClOrdID(11) / Side(54) / TransactTime(60) /
// OrdType(40). 옵션: OrderID(37), OrderQty(38), Price(44), Symbol(55), TIF(59),
// QuoteID(117).
func mapOrderCancelReplaceRequest(rr ordercancelreplacerequest.OrderCancelReplaceRequest) (OrderEnvelope, error) {
	var env OrderEnvelope
	env.Op = "replace"

	// OrigClOrdID (필수).
	var orig field.OrigClOrdIDField
	if err := rr.Get(&orig); err != nil {
		return env, errors.New("OrigClOrdID(41) 필수")
	}
	env.OrigClientOrderID = orig.String()

	// ClOrdID (필수).
	var cl field.ClOrdIDField
	if err := rr.Get(&cl); err != nil {
		return env, errors.New("ClOrdID(11) 필수")
	}
	env.ClientOrderID = cl.String()

	// Side (필수).
	var side field.SideField
	if err := rr.Get(&side); err != nil {
		return env, errors.New("Side(54) 필수")
	}
	switch s := side.String(); s {
	case "1":
		env.Side = "buy"
	case "2":
		env.Side = "sell"
	default:
		return env, fmt.Errorf("Side(54) 미지원: %q", s)
	}

	// OrdType (필수).
	var ot field.OrdTypeField
	if err := rr.Get(&ot); err != nil {
		return env, errors.New("OrdType(40) 필수")
	}
	switch t := ot.String(); t {
	case "1":
		env.OrdType = "market"
	case "2":
		env.OrdType = "limit"
		var px field.PriceField
		if err := rr.Get(&px); err == nil {
			p, _ := px.Value().Float64()
			env.Price = p
		}
	default:
		return env, fmt.Errorf("OrdType(40) 미지원: %q", t)
	}

	// 옵션 — OrderID / Symbol / OrderQty / TIF / QuoteID.
	var oid field.OrderIDField
	if err := rr.Get(&oid); err == nil {
		env.OrderID = oid.String()
	}
	var sym field.SymbolField
	if err := rr.Get(&sym); err == nil {
		env.Symbol = sym.String()
	}
	var qty field.OrderQtyField
	if err := rr.Get(&qty); err == nil {
		q, _ := qty.Value().Float64()
		env.Qty = q
	}
	var tif field.TimeInForceField
	if err := rr.Get(&tif); err == nil {
		switch t := tif.String(); t {
		case "0":
			env.TIF = "day"
		case "1":
			env.TIF = "gtc"
		case "3":
			env.TIF = "ioc"
		case "4":
			env.TIF = "fok"
		}
	}
	var qid field.QuoteIDField
	if err := rr.Get(&qid); err == nil {
		env.QuoteID = qid.String()
	}

	env.RawFix = extractRawFix(rr.Body.FieldMap)
	return env, nil
}

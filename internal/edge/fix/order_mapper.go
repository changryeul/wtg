package fix

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/quickfixgo/field"
	"github.com/quickfixgo/fix44/newordersingle"
	"github.com/quickfixgo/quickfix"
)

// OrderEnvelope — NewOrderSingle (35=D) → JSON envelope 변환 결과.
//
// `/v1/tx` 호출 시 body 의 `data` 영역에 그대로 들어감. envelope wire 호환
// 형식 — 매매 엔진은 변경 없이 받음.
//
// 매핑:
//
//	FIX tag   FIX 의미        envelope 필드
//	------    --------        ---------------
//	11       ClOrdID          client_order_id
//	55       Symbol           symbol (예: "USD/KRW" — Symbol field 그대로)
//	54       Side (1=Buy)     side ("buy" / "sell")
//	38       OrderQty         qty
//	40       OrdType (2=Lmt)  ord_type ("limit" / "market")
//	44       Price            price (Limit 일 때만)
//	59       TimeInForce      tif ("day" / "gtc" / "ioc" / "fok")
//	117      QuoteID          quote_id
type OrderEnvelope struct {
	ClientOrderID string  `json:"client_order_id"`
	Symbol        string  `json:"symbol"`
	Side          string  `json:"side"`
	Qty           float64 `json:"qty"`
	OrdType       string  `json:"ord_type"`
	Price         float64 `json:"price,omitempty"`
	TIF           string  `json:"tif,omitempty"`
	QuoteID       string  `json:"quote_id,omitempty"`
}

// mapNewOrderSingle — FIX 4.4 NewOrderSingle 메시지 → OrderEnvelope.
//
// 필수: ClOrdID(11) / Symbol(55) / Side(54) / OrderQty(38) / OrdType(40).
// Limit 일 때만 Price(44) 필수.
func mapNewOrderSingle(nos newordersingle.NewOrderSingle) (OrderEnvelope, error) {
	var env OrderEnvelope

	// ClOrdID (필수).
	var clOrdID field.ClOrdIDField
	if err := nos.Get(&clOrdID); err != nil {
		return env, errors.New("ClOrdID(11) 필수")
	}
	env.ClientOrderID = clOrdID.String()

	// Symbol (필수).
	var sym field.SymbolField
	if err := nos.Get(&sym); err != nil {
		return env, errors.New("Symbol(55) 필수")
	}
	env.Symbol = sym.String()

	// Side (필수). 1=Buy / 2=Sell.
	var side field.SideField
	if err := nos.Get(&side); err != nil {
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

	// OrderQty (필수).
	var qty field.OrderQtyField
	if err := nos.Get(&qty); err != nil {
		return env, errors.New("OrderQty(38) 필수")
	}
	// decimal.Float64() 의 두 번째 반환은 "정확한 변환 여부" flag — 값은
	// 항상 유효. 운영에서 수량은 정수라 정확. exact 손실은 무시.
	q, _ := qty.Value().Float64()
	env.Qty = q

	// OrdType (필수). 1=Market / 2=Limit.
	var ot field.OrdTypeField
	if err := nos.Get(&ot); err != nil {
		return env, errors.New("OrdType(40) 필수")
	}
	switch t := ot.String(); t {
	case "1":
		env.OrdType = "market"
	case "2":
		env.OrdType = "limit"
		// Limit 일 때만 Price 필수.
		var px field.PriceField
		if err := nos.Get(&px); err != nil {
			return env, errors.New("OrdType=Limit 인데 Price(44) 없음")
		}
		// Float64 의 exact flag 무시 — 가격의 미세한 손실은 quote 단계에서
		// 흡수. 운영 정확도가 필요하면 별도 decimal 필드 도입 (Phase B).
		p, _ := px.Value().Float64()
		env.Price = p
	default:
		return env, fmt.Errorf("OrdType(40) 미지원: %q", t)
	}

	// TimeInForce (옵션).
	var tif field.TimeInForceField
	if err := nos.Get(&tif); err == nil {
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

	// QuoteID (옵션 — 시세 잠금이 있을 때만).
	var qid field.QuoteIDField
	if err := nos.Get(&qid); err == nil {
		env.QuoteID = qid.String()
	}

	return env, nil
}

// httpForwarder — OrderEnvelope 을 mci-api 의 POST /v1/tx 로 forward.
//
// Phase A 의 PoC mode 에서 옵션 — TxForwardURL 가 채워졌을 때만 활성.
type httpForwarder struct {
	url    string
	logger *slog.Logger
	client *http.Client
}

func newHTTPForwarder(url string, logger *slog.Logger) *httpForwarder {
	return &httpForwarder{
		url:    strings.TrimRight(url, "/") + "/v1/tx",
		logger: logger,
		client: &http.Client{},
	}
}

// Forward — JSON envelope POST. principal 의 Channel/Usid 를 X-WTG-Edge-*
// 헤더로 전달 — 기존 internal 인증 패턴 그대로.
func (f *httpForwarder) Forward(ctx context.Context, p Principal, env OrderEnvelope) error {
	body, err := json.Marshal(map[string]any{
		"alias": "FIX_NEW_ORDER",
		"data":  env,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.url,
		strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-WTG-Edge-User", p.Usid)
	req.Header.Set("X-WTG-Edge-Channel", p.Channel)
	req.Header.Set("X-WTG-Edge-Site", p.Site)
	req.Header.Set("X-WTG-Edge-Tier", p.Tier)
	resp, err := f.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("tx forward HTTP %d", resp.StatusCode)
	}
	return nil
}

// Compile-time interface 확인.
var _ OrderForwarder = (*httpForwarder)(nil)

// quickfixMessageType helper — Phase A 진단용. message 가 NewOrderSingle 인지
// 빠른 sanity check.
func isNewOrderSingle(msg *quickfix.Message) bool {
	t, err := msg.MsgType()
	return err == nil && t == "D"
}

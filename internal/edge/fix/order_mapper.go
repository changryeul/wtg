package fix

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
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
// Layer 1 (typed) 매핑 — 표준 FIX 4.4 의 공통 tag (모든 카운터파티 공통):
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
//
// Layer 3 (raw_fix) — Body 의 모든 tag/value 를 stringly-typed dict 로 보존
// (Phase B). 카운터파티별 dialect (user-defined tag 5000+ / 카운터파티별
// required tag) 를 매매 엔진이 알아서 해석. WTG 는 변환 0 — generic envelope
// 원칙 일관.
type OrderEnvelope struct {
	// Op — Phase C. 운영 alias 의 분기 (한 alias 가 모든 lifecycle 처리).
	// "new" / "cancel" / "replace". 매매 엔진의 alias handler 가 op 봐서 dispatch.
	Op string `json:"op"`

	ClientOrderID string  `json:"client_order_id"`
	Symbol        string  `json:"symbol,omitempty"`
	Side          string  `json:"side"`
	Qty           float64 `json:"qty,omitempty"`
	OrdType       string  `json:"ord_type,omitempty"`
	Price         float64 `json:"price,omitempty"`
	TIF           string  `json:"tif,omitempty"`
	QuoteID       string  `json:"quote_id,omitempty"`

	// OrigClientOrderID — Phase C 의 Cancel/Replace 의 원본 ClOrdID (FIX tag 41).
	// op="new" 이면 빈값.
	OrigClientOrderID string `json:"orig_client_order_id,omitempty"`

	// OrderID — 매매 엔진의 채번 ID (FIX tag 37). Cancel/Replace 시 클라가
	// 직전 ExecutionReport 의 OrderID 를 그대로 송신.
	OrderID string `json:"order_id,omitempty"`

	// RawFix — Phase B Layer 3. 메시지 Body 의 모든 tag/value
	// (string key = tag 번호). 카운터파티별 dialect / user-defined tag 보존.
	// nil 이면 typed 필드만 사용 — Phase A 호환.
	RawFix map[string]string `json:"raw_fix,omitempty"`
}

// mapNewOrderSingle — FIX 4.4 NewOrderSingle 메시지 → OrderEnvelope.
//
// 필수: ClOrdID(11) / Symbol(55) / Side(54) / OrderQty(38) / OrdType(40).
// Limit 일 때만 Price(44) 필수.
func mapNewOrderSingle(nos newordersingle.NewOrderSingle) (OrderEnvelope, error) {
	var env OrderEnvelope
	env.Op = "new"

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

	// Layer 3 — Body 의 모든 tag/value 를 raw_fix 에 보존 (카운터파티별 dialect
	// 처리). 35=D 본문의 tags() 를 stringly-typed map 으로 복사.
	env.RawFix = extractRawFix(nos.Body.FieldMap)

	return env, nil
}

// extractRawFix — FieldMap 의 모든 tag/value 를 string map 으로. 매매 엔진이
// dialect (user-defined tag / 카운터파티별 required tag) 해석 시 참조.
//
// 실패한 GetString 은 skip — typed 필드에선 이미 검증됨.
func extractRawFix(body quickfix.FieldMap) map[string]string {
	tags := body.Tags()
	if len(tags) == 0 {
		return nil
	}
	out := make(map[string]string, len(tags))
	for _, t := range tags {
		v, err := body.GetString(t)
		if err != nil {
			continue
		}
		out[strconv.Itoa(int(t))] = v
	}
	return out
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
// 헤더로 전달 — 기존 internal 인증 패턴 그대로. Phase B Layer 2 — alias 는
// principal.OrderAlias 사용 (카운터파티별 매매 routing 분기).
func (f *httpForwarder) Forward(ctx context.Context, p Principal, env OrderEnvelope) error {
	alias := p.OrderAlias
	if alias == "" {
		alias = "FIX_NEW_ORDER"
	}
	body, err := json.Marshal(map[string]any{
		"alias": alias,
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

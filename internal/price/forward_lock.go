package price

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/winwaysystems/wtg/pkg/pricing"
	"github.com/winwaysystems/wtg/pkg/quote"
	"github.com/winwaysystems/wtg/pkg/quoteid"
	"github.com/winwaysystems/wtg/pkg/session"
)

// Phase 5 3단계 — forward 거래 시점 quote 잠금 (last-look).
//
// 클라이언트가 forward 화면에서 "USD/KRW 1M 1만 달러" 거래 진입 시 호출:
//   POST /v1/quote/forward/lock
//   { "pair":"USD/KRW", "tenor":"1M", "profile":"WEB.BRANCH.VIP",
//     "customer_id":"alice", "side":"buy" }
//
// 서버는 그 시점의 BEST SPOT + PricingTable.ApplyForCustomer (해당 tenor) 로
// 가격 재합성 → quoteid.Generator 발급 + Registry.Put → quote_id + valid_until
// 반환. 클라이언트가 quote_id 첨부해 실 거래 트랜잭션 호출.
//
// 표시 가격 (forward-snapshot) 과 거래 가격 (forward/lock) 은 다를 수 있음 —
// last-look 의 본질. 분쟁 시 Record 가 권위 소스.

// ForwardLockRequest — 요청 본문.
//
// Tenor / ValueDate 둘 다 비면 SPOT. 둘 다 채워지면 ValueDate 우선 (broken-date
// 처리). 운영자/UI 는 보통 하나만 보냄.
type ForwardLockRequest struct {
	Pair       string `json:"pair"`
	Tenor      string `json:"tenor,omitempty"`      // "SPOT" | "1W" | "1M" ... (선택)
	ValueDate  string `json:"value_date,omitempty"` // "2026-06-15" (RFC3339 date) — broken-date 시 우선
	Profile    string `json:"profile"`
	CustomerID string `json:"customer_id,omitempty"`
	// side, amount 는 metadata — Record 에는 안 들어가지만 audit 로그에 남길 수 있음.
	Side   string  `json:"side,omitempty"`   // "buy" | "sell"
	Amount float64 `json:"amount,omitempty"` // base 통화 수량
}

// ForwardLockResponse — 응답.
type ForwardLockResponse struct {
	QuoteID    string `json:"quote_id"`
	Pair       string `json:"pair"`
	Tenor      string `json:"tenor,omitempty"`      // exact tenor (보간 안 됐을 때).
	ValueDate  string `json:"value_date,omitempty"` // 요청 echo (broken-date 일 때).
	Profile    string `json:"profile"`
	CustomerID string `json:"customer_id,omitempty"`
	Side       string `json:"side,omitempty"`

	Bid    float64 `json:"bid"`     // customer-applied (lock 시점)
	Ask    float64 `json:"ask"`
	RawBid float64 `json:"raw_bid"` // 그 시점 BEST raw
	RawAsk float64 `json:"raw_ask"`

	IssuedUnixNano     int64 `json:"issued_unix_nano"`
	ValidUntilUnixNano int64 `json:"valid_until_unix_nano"`

	TableVersion int64 `json:"table_version"`

	// 보간 정보 (broken-date 일 때만 채워짐). Exact 매칭이면 InterpolatedFrom 만,
	// 보간된 경우 From + To + Weight + 보간 swap.
	Interpolation *ForwardLockInterpolation `json:"interpolation,omitempty"`
}

// ForwardLockInterpolation — broken-date 보간 audit.
type ForwardLockInterpolation struct {
	OffsetDays int     `json:"offset_days"`
	From       string  `json:"from"` // tenor (예: "1W")
	To         string  `json:"to"`   // tenor (예: "1M"); Exact 일 때는 From 과 동일.
	Weight     float64 `json:"weight"`
	SwapBid    float64 `json:"swap_bid"`
	SwapAsk    float64 `json:"swap_ask"`
	Exact      bool    `json:"exact,omitempty"`
}

// ForwardLockDeps — handler 의존성.
type ForwardLockDeps struct {
	Store    *pricing.Store
	Best     *BestConsumer
	Gen      *quoteid.Generator
	Reg      quoteid.Registry
	Validity time.Duration

	// PutTimeout — Registry.Put 의 timeout. default 200ms.
	PutTimeout time.Duration
}

// ForwardLockHandler — POST /v1/quote/forward/lock
//
// 책임:
//   1. 입력 검증 (pair / profile / tenor 필수).
//   2. BEST SPOT raw 조회.
//   3. PricingTable.ApplyForCustomer (해당 tenor) — customer 매칭 + 마진 + swap 합산.
//   4. quoteid.Generator.Next() + Registry.Put.
//   5. 응답 반환. Registry.Put 실패는 audit 흐릿하지만 거래 자체는 진행 (best-effort).
//
// 보안: 본 endpoint 호출자는 인증된 클라이언트만. mci-edge-api 가 인증 미들웨어
// 통과 후 backend 로 forward 하는 패턴 권장 (현재 dev 직접 노출은 검증용).
func ForwardLockHandler(deps ForwardLockDeps, devMode bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if devMode {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		var req ForwardLockRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeForwardErr(w, http.StatusBadRequest, "JSON 파싱: "+err.Error())
			return
		}
		req.Pair = strings.TrimSpace(req.Pair)
		req.Tenor = strings.TrimSpace(req.Tenor)
		req.ValueDate = strings.TrimSpace(req.ValueDate)
		req.Profile = strings.TrimSpace(req.Profile)
		if req.Pair == "" || req.Profile == "" {
			writeForwardErr(w, http.StatusBadRequest, "pair, profile 필수")
			return
		}
		// tenor / value_date 둘 다 비면 SPOT.
		if req.Tenor == "" && req.ValueDate == "" {
			req.Tenor = "SPOT"
		}
		profile, perr := session.ParseProfileKey(req.Profile)
		if perr != nil {
			writeForwardErr(w, http.StatusBadRequest, "invalid profile: "+perr.Error())
			return
		}

		// BEST SPOT 호가.
		if deps.Best == nil {
			writeForwardErr(w, http.StatusServiceUnavailable, "BEST consumer 미활성")
			return
		}
		best := deps.Best.Stats()
		sym := strings.ReplaceAll(req.Pair, "/", "")
		spotStat, ok := best.Symbols[sym]
		if !ok {
			writeForwardErr(w, http.StatusNotFound, "no BEST snapshot for "+sym)
			return
		}

		// PricingTable + ApplyForCustomer.
		tbl := deps.Store.Load()
		if tbl == nil {
			writeForwardErr(w, http.StatusServiceUnavailable, "PricingTable 미로드")
			return
		}
		now := time.Now()
		raw := quote.Quote{
			Pair: session.Pair(req.Pair),
			Bid:  spotStat.BestBid,
			Ask:  spotStat.BestAsk,
			TS:   now,
		}

		// value_date 우선 → ApplyForValueDate (보간), 없으면 tenor → ApplyForCustomer.
		var cq pricing.CustomerQuote
		var interp pricing.SwapInterpolation
		var brokenDate bool
		if req.ValueDate != "" {
			vd, err := parseValueDate(req.ValueDate)
			if err != nil {
				writeForwardErr(w, http.StatusBadRequest, "invalid value_date: "+err.Error())
				return
			}
			// SPOT 일수 — Phase 5 1차는 T+2 고정. pair 별 SPOT 컨벤션 (USD/CAD T+1)
			// 은 후속에서 PricingTable 에 spot_days 필드로 확장.
			spotDays := 2
			var perr error
			cq, interp, perr = tbl.ApplyForValueDate(raw, profile, vd, now, spotDays, req.CustomerID)
			if perr != nil {
				writeForwardErr(w, http.StatusBadRequest, "value_date 처리: "+perr.Error())
				return
			}
			brokenDate = true
		} else {
			cq = tbl.ApplyForCustomer(raw, profile, pricing.Tenor(req.Tenor), now, req.CustomerID)
		}

		// QuoteID 발급 + Registry.Put.
		var quoteIDStr string
		var validUntil time.Time
		if deps.Gen != nil && deps.Reg != nil {
			id := deps.Gen.Next()
			validity := deps.Validity
			if validity <= 0 {
				validity = 500 * time.Millisecond
			}
			validUntil = now.Add(validity)
			rec := quoteid.Record{
				QuoteID:    id,
				Pair:       cq.Pair,
				Profile:    profile,
				Tenor:      string(cq.Tenor),
				Bid:        cq.Bid,
				Ask:        cq.Ask,
				IssuedAt:   now.UnixNano(),
				ValidUntil: validUntil.UnixNano(),
				Sequence:   deps.Gen.NextSequence(),
				Issuer:     deps.Gen.Instance(),
			}
			// broken-date 면 보간 정보 보존.
			if brokenDate {
				vd, _ := parseValueDate(req.ValueDate)
				rec.ValueDateUnixNano = vd.UnixNano()
				rec.OffsetDays = interp.OffsetDays
				rec.InterpolatedFrom = string(interp.From)
				rec.InterpolatedTo = string(interp.To)
				rec.InterpolationWeight = interp.Weight
				rec.InterpolatedSwapBid = interp.Margin.BidAmount
				rec.InterpolatedSwapAsk = interp.Margin.AskAmount
			}
			putTimeout := deps.PutTimeout
			if putTimeout <= 0 {
				putTimeout = 200 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), putTimeout)
			_ = deps.Reg.Put(ctx, rec) // 실패는 audit 손실 — 거래 자체는 진행 (best-effort)
			cancel()
			quoteIDStr = string(id)
		}

		out := ForwardLockResponse{
			QuoteID:            quoteIDStr,
			Pair:               req.Pair,
			Tenor:              string(cq.Tenor),
			ValueDate:          req.ValueDate,
			Profile:            req.Profile,
			CustomerID:         req.CustomerID,
			Side:               req.Side,
			Bid:                cq.Bid,
			Ask:                cq.Ask,
			RawBid:             spotStat.BestBid,
			RawAsk:             spotStat.BestAsk,
			IssuedUnixNano:     now.UnixNano(),
			ValidUntilUnixNano: validUntil.UnixNano(),
			TableVersion:       tbl.Version,
		}
		if brokenDate {
			out.Interpolation = &ForwardLockInterpolation{
				OffsetDays: interp.OffsetDays,
				From:       string(interp.From),
				To:         string(interp.To),
				Weight:     interp.Weight,
				SwapBid:    interp.Margin.BidAmount,
				SwapAsk:    interp.Margin.AskAmount,
				Exact:      interp.Exact,
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}
}

// parseValueDate — "YYYY-MM-DD" (RFC3339 date-only) 또는 RFC3339 datetime 둘 다.
// 결과는 UTC 자정으로 정규화.
func parseValueDate(s string) (time.Time, error) {
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		// 자정 정렬.
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC), nil
	}
	return time.Time{}, errors.New("date 형식 (YYYY-MM-DD 또는 RFC3339)")
}

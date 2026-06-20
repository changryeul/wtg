package price

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/winwaysystems/wtg/pkg/pricing"
	"github.com/winwaysystems/wtg/pkg/quote"
	"github.com/winwaysystems/wtg/pkg/quoteid"
	"github.com/winwaysystems/wtg/pkg/session"
)

// Phase S3 (FX swap) — swap (near leg + far leg) 거래 시점 quote 잠금.
//
// wire/응답 schema 는 docs/swap-trade-spec.md 가 단일 출처.
//
// S3-a (완료): input validation + 단일 시점 snapshot + leg ApplyForCustomer
//              / ApplyForValueDate.
// S3-b (본 파일): SwapIndex 원자 발급 + 부분실패 revoke + 메트릭.
// S3-c (예정): 매매 AP 측 swap_id 동시 검증 정책.
// S3-d (예정): C 클라이언트 wtg_price_swap_lock 추가.

// SwapLeg — request 의 leg 명세. tenor 또는 value_date 중 하나만.
type SwapLeg struct {
	Tenor     string `json:"tenor,omitempty"`      // "SPOT" | "1W" | "1M" | ...
	ValueDate string `json:"value_date,omitempty"` // "YYYY-MM-DD" (broken-date)
}

// SwapLockRequest — POST /v1/quote/swap/lock 본문.
type SwapLockRequest struct {
	Pair       string  `json:"pair"`
	Near       SwapLeg `json:"near"`
	Far        SwapLeg `json:"far"`
	Profile    string  `json:"profile"`
	CustomerID string  `json:"customer_id,omitempty"`
	Side       string  `json:"side,omitempty"`   // "buy_sell" | "sell_buy"
	Amount     float64 `json:"amount,omitempty"` // base 통화 수량 — audit
}

// SwapLegResult — 한 leg 의 잠금 결과. forward_lock 의 leg 와 schema 일치.
type SwapLegResult struct {
	QuoteID       string                    `json:"quote_id"`
	Tenor         string                    `json:"tenor"`
	ValueDate     string                    `json:"value_date,omitempty"`
	Bid           float64                   `json:"bid"`
	Ask           float64                   `json:"ask"`
	RawBid        float64                   `json:"raw_bid"`
	RawAsk        float64                   `json:"raw_ask"`
	SwapBid       float64                   `json:"swap_bid"` // far leg 에만 의미
	SwapAsk       float64                   `json:"swap_ask"`
	Interpolation *ForwardLockInterpolation `json:"interpolation,omitempty"`
}

// SwapDiff — far - near 호가 차이 (customer-applied 이후).
type SwapDiff struct {
	BidDiff float64 `json:"bid_diff"`
	AskDiff float64 `json:"ask_diff"`
}

// SwapLockResponse — POST /v1/quote/swap/lock 응답.
type SwapLockResponse struct {
	SwapID             string        `json:"swap_id"`
	Pair               string        `json:"pair"`
	Profile            string        `json:"profile"`
	CustomerID         string        `json:"customer_id,omitempty"`
	Side               string        `json:"side,omitempty"`
	IssuedUnixNano     int64         `json:"issued_unix_nano"`
	ValidUntilUnixNano int64         `json:"valid_until_unix_nano"`
	TableVersion       int64         `json:"table_version"`
	Near               SwapLegResult `json:"near"`
	Far                SwapLegResult `json:"far"`
	SwapDiff           SwapDiff      `json:"swap_diff"`
}

// SwapLockDeps — handler 의존성.
type SwapLockDeps struct {
	Store    *pricing.Store
	Best     *BestConsumer
	Cross    *CrossRateConsumer
	Gen      *quoteid.Generator
	Reg      quoteid.Registry
	Idx      quoteid.SwapIndex
	Validity time.Duration

	// PutTimeout — Registry/SwapIndex 한 호출의 timeout. default 200ms.
	PutTimeout time.Duration

	// SpotDays — SPOT 결제일 환산 (T+N). 1차는 caller 가 2 전달. pair 별
	// 컨벤션은 후속 phase.
	SpotDays int

	// Metrics — partial-failure 단계 + revoke 결과 hook. nil 이면 미수집.
	Metrics SwapLockMetrics
}

// SwapLockMetrics — 운영 관측 hook. mci-admin / Prometheus exporter 가 주입.
// 모든 메서드는 hot path 에서 호출되므로 non-blocking 보장.
type SwapLockMetrics interface {
	OnRequest()
	OnSuccess()
	// stage: "near" | "far" | "swap_index". reason 은 짧은 식별자.
	OnPartialFailure(stage, reason string)
	// outcome: "ok" | "fail". stage 는 무엇을 revoke 했는지 ("near"|"far"|"swap").
	OnRevoke(stage, outcome string)
}

// NoopSwapLockMetrics — 테스트/단독 부팅 시 기본값.
type NoopSwapLockMetrics struct{}

func (NoopSwapLockMetrics) OnRequest()                            {}
func (NoopSwapLockMetrics) OnSuccess()                            {}
func (NoopSwapLockMetrics) OnPartialFailure(_, _ string)          {}
func (NoopSwapLockMetrics) OnRevoke(_, _ string)                  {}

// AtomicSwapLockMetrics — 단순 counter. mci-admin 페이지가 Snapshot 으로 노출.
type AtomicSwapLockMetrics struct {
	requests        atomic.Uint64
	successes       atomic.Uint64
	failNear        atomic.Uint64
	failFar         atomic.Uint64
	failSwapIndex   atomic.Uint64
	revokeOK        atomic.Uint64
	revokeFail      atomic.Uint64
}

func (m *AtomicSwapLockMetrics) OnRequest() { m.requests.Add(1) }
func (m *AtomicSwapLockMetrics) OnSuccess() { m.successes.Add(1) }
func (m *AtomicSwapLockMetrics) OnPartialFailure(stage, _ string) {
	switch stage {
	case "near":
		m.failNear.Add(1)
	case "far":
		m.failFar.Add(1)
	case "swap_index":
		m.failSwapIndex.Add(1)
	}
}
func (m *AtomicSwapLockMetrics) OnRevoke(_, outcome string) {
	if outcome == "ok" {
		m.revokeOK.Add(1)
	} else {
		m.revokeFail.Add(1)
	}
}

// SwapLockMetricsSnapshot — Atomic counter 의 노출용 스냅샷.
type SwapLockMetricsSnapshot struct {
	Requests      uint64 `json:"requests"`
	Successes     uint64 `json:"successes"`
	FailNear      uint64 `json:"fail_near"`
	FailFar       uint64 `json:"fail_far"`
	FailSwapIndex uint64 `json:"fail_swap_index"`
	RevokeOK      uint64 `json:"revoke_ok"`
	RevokeFail    uint64 `json:"revoke_fail"`
}

// Snapshot — 현재 counter 값을 한 번에 노출. 메트릭 endpoint / admin UI 용.
func (m *AtomicSwapLockMetrics) Snapshot() SwapLockMetricsSnapshot {
	return SwapLockMetricsSnapshot{
		Requests:      m.requests.Load(),
		Successes:     m.successes.Load(),
		FailNear:      m.failNear.Load(),
		FailFar:       m.failFar.Load(),
		FailSwapIndex: m.failSwapIndex.Load(),
		RevokeOK:      m.revokeOK.Load(),
		RevokeFail:    m.revokeFail.Load(),
	}
}

// 에러 — input validation 단계.
var (
	errSwapSameLeg    = errors.New("near 와 far 가 동일 결제 — 단일 leg 거래는 /forward/lock 사용")
	errSwapLegInvert  = errors.New("near 결제일이 far 보다 늦음 — leg 순서 invert")
	errSwapLegMissing = errors.New("near/far 의 tenor 또는 value_date 중 하나는 필수")
)

// SwapLockHandler — POST /v1/quote/swap/lock
//
// 흐름 (S3-b):
//  1. input validation + profile parse.
//  2. BEST snapshot 단일 조회 (cross fallback).
//  3. PricingTable.Load + 두 leg ApplyForCustomer / ApplyForValueDate (동일 now / raw).
//  4. quote_id 발급 + Registry.Put near → far → SwapIndex.PutSwap 순차.
//     - near 실패 → 즉시 503 (far 시도 안 함).
//     - far 실패 → near revoke 시도 → 503.
//     - swap_index 실패 → near + far revoke 시도 → 503.
//  5. 성공 시 응답 + Metrics.OnSuccess.
func SwapLockHandler(deps SwapLockDeps, devMode bool) http.HandlerFunc {
	metrics := deps.Metrics
	if metrics == nil {
		metrics = NoopSwapLockMetrics{}
	}
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
		metrics.OnRequest()

		var req SwapLockRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeForwardErr(w, http.StatusBadRequest, "JSON 파싱: "+err.Error())
			return
		}
		req.Pair = strings.TrimSpace(req.Pair)
		req.Profile = strings.TrimSpace(req.Profile)
		req.Near.Tenor = strings.TrimSpace(req.Near.Tenor)
		req.Near.ValueDate = strings.TrimSpace(req.Near.ValueDate)
		req.Far.Tenor = strings.TrimSpace(req.Far.Tenor)
		req.Far.ValueDate = strings.TrimSpace(req.Far.ValueDate)

		if req.Pair == "" || req.Profile == "" {
			writeForwardErr(w, http.StatusBadRequest, "pair, profile 필수")
			return
		}
		if err := validateSwapLegs(req.Near, req.Far, deps.SpotDays); err != nil {
			writeForwardErr(w, http.StatusBadRequest, err.Error())
			return
		}
		profile, perr := session.ParseProfileKey(req.Profile)
		if perr != nil {
			writeForwardErr(w, http.StatusBadRequest, "invalid profile: "+perr.Error())
			return
		}

		// BEST snapshot — 단일 조회.
		if deps.Best == nil {
			writeForwardErr(w, http.StatusServiceUnavailable, "BEST consumer 미활성")
			return
		}
		best := deps.Best.Stats()
		rawBid, rawAsk, ok := lookupSpotRawForSwap(deps, best, req.Pair)
		if !ok {
			writeForwardErr(w, http.StatusNotFound, "no BEST/cross snapshot for "+req.Pair)
			return
		}
		tbl := deps.Store.Load()
		if tbl == nil {
			writeForwardErr(w, http.StatusServiceUnavailable, "PricingTable 미로드")
			return
		}

		// 핵심 — 두 leg 가 같은 now + 같은 raw 를 본다.
		now := time.Now()
		raw := quote.Quote{
			Pair: session.Pair(req.Pair),
			Bid:  rawBid,
			Ask:  rawAsk,
			TS:   now,
		}

		nearCQ, nearInterp, nearBroken, err := applyLeg(tbl, raw, profile, req.Near, deps.SpotDays, now, req.CustomerID)
		if err != nil {
			writeForwardErr(w, http.StatusBadRequest, "near leg: "+err.Error())
			return
		}
		farCQ, farInterp, farBroken, err := applyLeg(tbl, raw, profile, req.Far, deps.SpotDays, now, req.CustomerID)
		if err != nil {
			writeForwardErr(w, http.StatusBadRequest, "far leg: "+err.Error())
			return
		}

		validity := deps.Validity
		if validity <= 0 {
			validity = 500 * time.Millisecond
		}
		validUntil := now.Add(validity)

		// quote_id 발급 정책 (S3-b):
		// Gen/Reg/Idx 모두 있어야 본 endpoint 가 의미 — 미주입 시 503.
		// (서버 라우트 등록 단계에서 같은 조건으로 게이트하지만 방어 차원.)
		if deps.Gen == nil || deps.Reg == nil || deps.Idx == nil {
			writeForwardErr(w, http.StatusServiceUnavailable, "quoteid Generator/Registry/SwapIndex 미주입")
			return
		}

		putTimeout := deps.PutTimeout
		if putTimeout <= 0 {
			putTimeout = 200 * time.Millisecond
		}
		issuer := deps.Gen.Instance()
		nearID := deps.Gen.Next()
		nearSeq := deps.Gen.NextSequence()
		farID := deps.Gen.Next()
		farSeq := deps.Gen.NextSequence()
		swapID := "SW-" + string(deps.Gen.Next())

		nearRec := buildLegRecord(nearID, nearCQ, profile,
			nearSeq, issuer, now, validUntil, nearBroken, nearInterp, req.Near.ValueDate)
		farRec := buildLegRecord(farID, farCQ, profile,
			farSeq, issuer, now, validUntil, farBroken, farInterp, req.Far.ValueDate)
		swapRec := quoteid.SwapRecord{
			SwapID:     swapID,
			NearID:     nearID,
			FarID:      farID,
			IssuedAt:   now.UnixNano(),
			ValidUntil: validUntil.UnixNano(),
			Issuer:     issuer,
		}

		// 원자 발급 — 순차 시도 + 부분실패 revoke.
		if err := putSwapAtomic(r.Context(), deps, nearRec, farRec, swapRec, putTimeout, metrics); err != nil {
			writeForwardErr(w, http.StatusServiceUnavailable, err.Error())
			return
		}

		diff := SwapDiff{
			BidDiff: farCQ.Bid - nearCQ.Bid,
			AskDiff: farCQ.Ask - nearCQ.Ask,
		}
		out := SwapLockResponse{
			SwapID:             swapID,
			Pair:               req.Pair,
			Profile:            req.Profile,
			CustomerID:         req.CustomerID,
			Side:               req.Side,
			IssuedUnixNano:     now.UnixNano(),
			ValidUntilUnixNano: validUntil.UnixNano(),
			TableVersion:       tbl.Version,
			Near:               buildLegResult(string(nearID), nearCQ, rawBid, rawAsk, req.Near.ValueDate, nearBroken, nearInterp, false),
			Far:                buildLegResult(string(farID), farCQ, rawBid, rawAsk, req.Far.ValueDate, farBroken, farInterp, true),
			SwapDiff:           diff,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
		metrics.OnSuccess()
	}
}

// putSwapAtomic — Registry.Put near → far → SwapIndex.PutSwap 순차. 한 단계
// 실패하면 이전 단계 revoke (best-effort) + 에러 반환. parent ctx 가 만료된
// 경우에도 revoke 는 별도 background ctx 로 — partial state 가 leak 되면 안 됨.
func putSwapAtomic(
	parentCtx context.Context, deps SwapLockDeps,
	near, far quoteid.Record, sw quoteid.SwapRecord,
	putTimeout time.Duration, metrics SwapLockMetrics,
) error {
	// 1. near leg.
	ctx, cancel := context.WithTimeout(parentCtx, putTimeout)
	err := deps.Reg.Put(ctx, near)
	cancel()
	if err != nil {
		metrics.OnPartialFailure("near", classifyErr(err))
		return errors.New("near leg 등록 실패: " + err.Error())
	}

	// 2. far leg. 실패 시 near revoke.
	ctx, cancel = context.WithTimeout(parentCtx, putTimeout)
	err = deps.Reg.Put(ctx, far)
	cancel()
	if err != nil {
		metrics.OnPartialFailure("far", classifyErr(err))
		revokeLeg(deps.Idx, near.QuoteID, "near", putTimeout, metrics)
		return errors.New("far leg 등록 실패: " + err.Error())
	}

	// 3. swap_index. 실패 시 near + far 둘 다 revoke.
	ctx, cancel = context.WithTimeout(parentCtx, putTimeout)
	err = deps.Idx.PutSwap(ctx, sw)
	cancel()
	if err != nil {
		metrics.OnPartialFailure("swap_index", classifyErr(err))
		revokeLeg(deps.Idx, near.QuoteID, "near", putTimeout, metrics)
		revokeLeg(deps.Idx, far.QuoteID, "far", putTimeout, metrics)
		return errors.New("swap_id 인덱스 등록 실패: " + err.Error())
	}
	return nil
}

// revokeLeg — best-effort. parent ctx 와 무관한 background timeout — partial
// state 가 남으면 매매 AP 가 stale quote_id 를 신뢰할 수 있어 위험.
func revokeLeg(idx quoteid.SwapIndex, id quoteid.QuoteID, stage string, timeout time.Duration, metrics SwapLockMetrics) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := idx.Delete(ctx, id); err != nil {
		metrics.OnRevoke(stage, "fail")
		return
	}
	metrics.OnRevoke(stage, "ok")
}

// classifyErr — 메트릭 레이블용 짧은 식별자. 자세히는 로그/trace 가 보존.
func classifyErr(err error) string {
	if err == nil {
		return "none"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if errors.Is(err, quoteid.ErrInvalidRecord) {
		return "invalid_record"
	}
	return "other"
}

// validateSwapLegs — leg 동일/역전 등 input 단계 검증.
func validateSwapLegs(near, far SwapLeg, spotDays int) error {
	nearKind, farKind := legKind(near), legKind(far)
	if nearKind == legKindMissing || farKind == legKindMissing {
		return errSwapLegMissing
	}
	if nearKind == legKindTenor && farKind == legKindTenor && near.Tenor == far.Tenor {
		return errSwapSameLeg
	}
	if nearKind == legKindValueDate && farKind == legKindValueDate && near.ValueDate == far.ValueDate {
		return errSwapSameLeg
	}
	if nearKind == legKindValueDate && farKind == legKindValueDate {
		nv, err := parseValueDate(near.ValueDate)
		if err != nil {
			return errors.New("near.value_date: " + err.Error())
		}
		fv, err := parseValueDate(far.ValueDate)
		if err != nil {
			return errors.New("far.value_date: " + err.Error())
		}
		if !nv.Before(fv) {
			return errSwapLegInvert
		}
	}
	_ = spotDays
	return nil
}

type legKindEnum int

const (
	legKindMissing legKindEnum = iota
	legKindTenor
	legKindValueDate
)

func legKind(l SwapLeg) legKindEnum {
	if l.ValueDate != "" {
		return legKindValueDate
	}
	if l.Tenor != "" {
		return legKindTenor
	}
	return legKindMissing
}

// applyLeg — 한 leg 의 ApplyForCustomer / ApplyForValueDate 분기. now/raw 는
// caller 가 동일 값을 전달 — 본 함수는 stateless.
func applyLeg(
	tbl *pricing.PricingTable, raw quote.Quote, profile session.Profile,
	leg SwapLeg, spotDays int, now time.Time, customerID string,
) (pricing.CustomerQuote, pricing.SwapInterpolation, bool, error) {
	if leg.ValueDate != "" {
		vd, err := parseValueDate(leg.ValueDate)
		if err != nil {
			return pricing.CustomerQuote{}, pricing.SwapInterpolation{}, false,
				errors.New("invalid value_date: " + err.Error())
		}
		days := spotDays
		if days <= 0 {
			days = 2
		}
		cq, interp, err := tbl.ApplyForValueDate(raw, profile, vd, now, days, customerID)
		if err != nil {
			return pricing.CustomerQuote{}, pricing.SwapInterpolation{}, false, err
		}
		return cq, interp, true, nil
	}
	cq := tbl.ApplyForCustomer(raw, profile, pricing.Tenor(leg.Tenor), now, customerID)
	return cq, pricing.SwapInterpolation{}, false, nil
}

// buildLegRecord — quoteid.Record 빌드.
func buildLegRecord(
	id quoteid.QuoteID, cq pricing.CustomerQuote, profile session.Profile,
	seq uint64, issuer string, issued, validUntil time.Time,
	broken bool, interp pricing.SwapInterpolation, valueDateStr string,
) quoteid.Record {
	rec := quoteid.Record{
		QuoteID:    id,
		Pair:       cq.Pair,
		Profile:    profile,
		Tenor:      string(cq.Tenor),
		Bid:        cq.Bid,
		Ask:        cq.Ask,
		IssuedAt:   issued.UnixNano(),
		ValidUntil: validUntil.UnixNano(),
		Sequence:   seq,
		Issuer:     issuer,
	}
	if broken {
		if vd, err := parseValueDate(valueDateStr); err == nil {
			rec.ValueDateUnixNano = vd.UnixNano()
		}
		rec.OffsetDays = interp.OffsetDays
		rec.InterpolatedFrom = string(interp.From)
		rec.InterpolatedTo = string(interp.To)
		rec.InterpolationWeight = interp.Weight
		rec.InterpolatedSwapBid = interp.Margin.BidAmount
		rec.InterpolatedSwapAsk = interp.Margin.AskAmount
	}
	return rec
}

// buildLegResult — 응답 SwapLegResult 빌드. isFar 면 swap_bid/swap_ask 채움.
func buildLegResult(
	quoteIDStr string, cq pricing.CustomerQuote, rawBid, rawAsk float64,
	valueDate string, broken bool, interp pricing.SwapInterpolation, isFar bool,
) SwapLegResult {
	out := SwapLegResult{
		QuoteID:   quoteIDStr,
		Tenor:     string(cq.Tenor),
		ValueDate: valueDate,
		Bid:       cq.Bid,
		Ask:       cq.Ask,
		RawBid:    rawBid,
		RawAsk:    rawAsk,
	}
	if isFar {
		out.SwapBid = interp.Margin.BidAmount
		out.SwapAsk = interp.Margin.AskAmount
	}
	if broken {
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
	return out
}

// lookupSpotRawForSwap — BEST → cross fallback. forward_snapshot 패턴 재사용.
func lookupSpotRawForSwap(deps SwapLockDeps, best BestStats, pair string) (bid, ask float64, ok bool) {
	sym := strings.ReplaceAll(pair, "/", "")
	if s, found := best.Symbols[sym]; found {
		return s.BestBid, s.BestAsk, true
	}
	if deps.Cross != nil {
		b, a, _, isStale, found := deps.Cross.LatestCross(session.Pair(pair))
		if found && !isStale {
			return b, a, true
		}
	}
	return 0, 0, false
}

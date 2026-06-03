package admin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/winwaysystems/wtg/pkg/pricing"
	"github.com/winwaysystems/wtg/pkg/quote"
	"github.com/winwaysystems/wtg/pkg/session"
)

// PricingPreviewRequest — wizard 가 보낸 변경 미리보기 입력.
//
// Changes: 변경된 PricingTableDoc 전체 (raw JSON). 기존 etcd 의 PricingTable
// 과 본 doc 둘 다 ApplyAt 으로 계산해 before/after 비교.
//
// 단순화: 단일 (Profile, Pair, Tenor) 시뮬레이션. 매트릭스 (N×M) 는 후속 옵션.
type PricingPreviewRequest struct {
	Profile   session.Profile `json:"profile"`
	Pair      string          `json:"pair"`
	Tenor     string          `json:"tenor,omitempty"` // 빈값 = SPOT
	SampleBid float64         `json:"sample_bid"`
	SampleAsk float64         `json:"sample_ask"`
	Changes   json.RawMessage `json:"changes"` // 변경된 PricingTableDoc JSON
}

// PricingPreviewResponse — before/after spread 비교.
type PricingPreviewResponse struct {
	Profile        string    `json:"profile"`
	Pair           string    `json:"pair"`
	Tenor          string    `json:"tenor"`
	CurrentVersion int64     `json:"current_version"`
	PreviewVersion int64     `json:"preview_version"`
	Before         legBidAsk `json:"before"`
	After          legBidAsk `json:"after"`
	DeltaBid       float64   `json:"delta_bid"`    // after.Bid - before.Bid
	DeltaAsk       float64   `json:"delta_ask"`    // after.Ask - before.Ask
	DeltaSpread    float64   `json:"delta_spread"` // after.Spread - before.Spread
}

type legBidAsk struct {
	Bid    float64 `json:"bid"`
	Ask    float64 `json:"ask"`
	Spread float64 `json:"spread"`
}

// PreviewPricing — POST /v1/admin/pricing/preview.
//
// 흐름:
//  1. 현재 etcd 의 PricingTable GET → currentTbl
//  2. body.Changes 를 새 PricingTableDoc 으로 parse → previewTbl
//  3. 두 table 에 같은 raw Quote 로 ApplyAt → CustomerQuote 비교
//  4. before/after + delta 반환
//
// 운영자가 wizard 에서 "변경 저장" 전 spread 영향을 확인할 수 있게 한다. 실
// 변경 (PUT pricing/table) 은 별도 endpoint 의 명시적 확인 후.
func PreviewPricing(deps *PricingDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Cli == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "no_etcd", "etcd 미구성")
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 2<<20)) // 2MB cap — Changes 포함
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "read", err.Error())
			return
		}
		var req PricingPreviewRequest
		if err := json.Unmarshal(body, &req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad_json", err.Error())
			return
		}
		if req.Pair == "" {
			writeJSONError(w, http.StatusBadRequest, "validation", "pair 필요")
			return
		}
		if req.SampleBid <= 0 || req.SampleAsk <= 0 {
			writeJSONError(w, http.StatusBadRequest, "validation", "sample_bid/sample_ask 양수")
			return
		}
		if len(req.Changes) == 0 {
			writeJSONError(w, http.StatusBadRequest, "validation", "changes (변경된 PricingTableDoc JSON) 필요")
			return
		}

		// 현재 etcd table.
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		resp, err := deps.Cli.Get(ctx, deps.key())
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "etcd", err.Error())
			return
		}
		var currentTbl *pricing.PricingTable
		if len(resp.Kvs) > 0 {
			currentTbl, err = pricing.ParsePricingTable(resp.Kvs[0].Value)
			if err != nil {
				writeJSONError(w, http.StatusInternalServerError, "current_parse", err.Error())
				return
			}
		} else {
			currentTbl = &pricing.PricingTable{}
		}

		// preview table.
		previewTbl, err := pricing.ParsePricingTable(req.Changes)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "preview_parse", err.Error())
			return
		}

		tenor := pricing.Tenor(req.Tenor)
		if tenor == "" {
			tenor = pricing.TenorSpot
		}
		now := time.Now()
		raw := quote.Quote{Pair: session.Pair(req.Pair), Bid: req.SampleBid, Ask: req.SampleAsk, TS: now}

		before := currentTbl.ApplyAt(raw, req.Profile, tenor, now)
		after := previewTbl.ApplyAt(raw, req.Profile, tenor, now)

		out := PricingPreviewResponse{
			Profile:        req.Profile.Key(),
			Pair:           req.Pair,
			Tenor:          string(tenor),
			CurrentVersion: currentTbl.Version,
			PreviewVersion: previewTbl.Version,
			Before:         legBidAsk{Bid: before.Bid, Ask: before.Ask, Spread: before.Ask - before.Bid},
			After:          legBidAsk{Bid: after.Bid, Ask: after.Ask, Spread: after.Ask - after.Bid},
		}
		out.DeltaBid = out.After.Bid - out.Before.Bid
		out.DeltaAsk = out.After.Ask - out.Before.Ask
		out.DeltaSpread = out.After.Spread - out.Before.Spread

		writeJSON(w, http.StatusOK, out)
	}
}

// PricingPreviewMatrixRequest — N profile × M pair 매트릭스 시뮬.
//
// SampleQuotes: pair 별 raw bid/ask. 미지정 pair 는 매트릭스에서 skip.
type PricingPreviewMatrixRequest struct {
	Profiles     []session.Profile      `json:"profiles"`
	Pairs        []string               `json:"pairs"`
	Tenor        string                 `json:"tenor,omitempty"`
	SampleQuotes map[string]sampleQuote `json:"sample_quotes"` // pair → bid/ask
	Changes      json.RawMessage        `json:"changes"`
}

type sampleQuote struct {
	Bid float64 `json:"bid"`
	Ask float64 `json:"ask"`
}

// PricingPreviewMatrixResponse — 매트릭스 row 들 + 두 version.
type PricingPreviewMatrixResponse struct {
	CurrentVersion int64                    `json:"current_version"`
	PreviewVersion int64                    `json:"preview_version"`
	Tenor          string                   `json:"tenor"`
	Rows           []PricingPreviewResponse `json:"rows"`
	Skipped        []string                 `json:"skipped,omitempty"` // sample_quote 없는 pair
}

// PreviewPricingMatrix — POST /v1/admin/pricing/preview-matrix.
//
// 동일 ApplyAt 로직 — 단일 단위 PreviewPricing 의 N×M 확장. profiles × pairs
// 의 모든 조합을 같은 currentTbl / previewTbl 로 계산해 운영자가 변경 영향을
// 한눈에 비교 가능.
//
// 한도: profiles × pairs ≤ 500 (DoS 방어). 더 큰 매트릭스는 chunk 분할.
func PreviewPricingMatrix(deps *PricingDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Cli == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "no_etcd", "etcd 미구성")
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20)) // 4MB cap
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "read", err.Error())
			return
		}
		var req PricingPreviewMatrixRequest
		if err := json.Unmarshal(body, &req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad_json", err.Error())
			return
		}
		if len(req.Profiles) == 0 || len(req.Pairs) == 0 {
			writeJSONError(w, http.StatusBadRequest, "validation",
				"profiles + pairs 둘 다 비어 있으면 안 됨")
			return
		}
		if len(req.Profiles)*len(req.Pairs) > 500 {
			writeJSONError(w, http.StatusBadRequest, "too_large",
				"profiles × pairs 한도 500 초과 — chunk 분할 필요")
			return
		}
		if len(req.Changes) == 0 {
			writeJSONError(w, http.StatusBadRequest, "validation", "changes 필요")
			return
		}

		// 현재 etcd table.
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		resp, err := deps.Cli.Get(ctx, deps.key())
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "etcd", err.Error())
			return
		}
		var currentTbl *pricing.PricingTable
		if len(resp.Kvs) > 0 {
			currentTbl, err = pricing.ParsePricingTable(resp.Kvs[0].Value)
			if err != nil {
				writeJSONError(w, http.StatusInternalServerError, "current_parse", err.Error())
				return
			}
		} else {
			currentTbl = &pricing.PricingTable{}
		}
		previewTbl, err := pricing.ParsePricingTable(req.Changes)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "preview_parse", err.Error())
			return
		}

		tenor := pricing.Tenor(req.Tenor)
		if tenor == "" {
			tenor = pricing.TenorSpot
		}
		now := time.Now()

		out := PricingPreviewMatrixResponse{
			CurrentVersion: currentTbl.Version,
			PreviewVersion: previewTbl.Version,
			Tenor:          string(tenor),
			Rows:           make([]PricingPreviewResponse, 0, len(req.Profiles)*len(req.Pairs)),
		}
		for _, pair := range req.Pairs {
			sq, ok := req.SampleQuotes[pair]
			if !ok || sq.Bid <= 0 || sq.Ask <= 0 {
				out.Skipped = append(out.Skipped, pair)
				continue
			}
			raw := quote.Quote{Pair: session.Pair(pair), Bid: sq.Bid, Ask: sq.Ask, TS: now}
			for _, prof := range req.Profiles {
				before := currentTbl.ApplyAt(raw, prof, tenor, now)
				after := previewTbl.ApplyAt(raw, prof, tenor, now)
				row := PricingPreviewResponse{
					Profile:        prof.Key(),
					Pair:           pair,
					Tenor:          string(tenor),
					CurrentVersion: currentTbl.Version,
					PreviewVersion: previewTbl.Version,
					Before:         legBidAsk{Bid: before.Bid, Ask: before.Ask, Spread: before.Ask - before.Bid},
					After:          legBidAsk{Bid: after.Bid, Ask: after.Ask, Spread: after.Ask - after.Bid},
				}
				row.DeltaBid = row.After.Bid - row.Before.Bid
				row.DeltaAsk = row.After.Ask - row.Before.Ask
				row.DeltaSpread = row.After.Spread - row.Before.Spread
				out.Rows = append(out.Rows, row)
			}
		}
		writeJSON(w, http.StatusOK, out)
	}
}

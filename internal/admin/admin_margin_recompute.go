package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/winwaysystems/wtg/pkg/pricing"
	"github.com/winwaysystems/wtg/pkg/quote"
	"github.com/winwaysystems/wtg/pkg/session"
)

// 분쟁/감사 backfill endpoint — 운영자가 "이 기간에 이 마진 테이블을 적용했더라면
// 고객이 어떤 시세를 받았을까?" 를 시각적으로 검증.
//
// 흐름:
//   1. quote_bars hypertable 에서 [from, to) 범위의 raw 1m 봉 조회.
//   2. 각 봉의 close_bid/close_ask 에 대해 PricingTable.Apply (선택된 Profile).
//   3. 봉당 마진 (customer - raw) 누적 → 통계 + 샘플 N 건 반환.
//
// 입력 PricingTable 우선순위:
//   - request.table_override 가 있으면 그것 (가상 시나리오)
//   - 없으면 etcd 의 현재 키 (mci-admin 이 직접 GET)

const (
	marginMaxBars        = 10000 // 단일 호출 최대 봉 수 — 운영 abuse 차단
	marginDefaultSamples = 10
	marginMaxSamples     = 200
)

// MarginRecomputeDeps — 핸들러 의존성.
type MarginRecomputeDeps struct {
	Cli     *clientv3.Client // etcd — PricingTable 현재값 조회
	Pool    *pgxpool.Pool    // TimescaleDB — quote_bars
	EtcdKey string           // default "wtg/pricing/table"
	Logger  *slog.Logger
	Audit   *AuditRing
}

type marginRecomputeRequest struct {
	From time.Time    `json:"from"`
	To   time.Time    `json:"to"`
	Pair session.Pair `json:"pair"`
	// Profile (legacy v1) / Profiles (v3 — 다중 비교).
	// 둘 다 있으면 Profiles 우선. 둘 다 비면 400.
	Profile       session.Profile          `json:"profile,omitempty"`
	Profiles      []session.Profile        `json:"profiles,omitempty"`
	TableOverride *pricing.PricingTableDoc `json:"table_override,omitempty"`
	SampleLimit   int                      `json:"sample_limit,omitempty"`
}

// marginOHLC — 봉 1개의 OHLC bid/ask (raw 또는 customer).
type marginOHLC struct {
	OpenBid  float64 `json:"open_bid"`
	OpenAsk  float64 `json:"open_ask"`
	HighBid  float64 `json:"high_bid"`
	HighAsk  float64 `json:"high_ask"`
	LowBid   float64 `json:"low_bid"`
	LowAsk   float64 `json:"low_ask"`
	CloseBid float64 `json:"close_bid"`
	CloseAsk float64 `json:"close_ask"`
}

type marginRecomputeSample struct {
	OpenedAt time.Time `json:"opened_at"`
	// Legacy v1 close 기준 필드 (UI 의 close-only 보기와 backward 호환).
	RawBid      float64 `json:"raw_bid"`
	RawAsk      float64 `json:"raw_ask"`
	CustomerBid float64 `json:"customer_bid"`
	CustomerAsk float64 `json:"customer_ask"`
	BidMargin   float64 `json:"bid_margin"` // customer_bid - raw_bid (close, 보통 ≤ 0)
	AskMargin   float64 `json:"ask_margin"` // customer_ask - raw_ask (close, 보통 ≥ 0)
	// v2 — OHLC 전체. 차트 시각화 / 봉 export 용.
	Raw      marginOHLC `json:"raw"`
	Customer marginOHLC `json:"customer"`
}

type marginRecomputeStats struct {
	BidMarginAvg float64 `json:"bid_margin_avg"`
	BidMarginMax float64 `json:"bid_margin_max"`
	BidMarginMin float64 `json:"bid_margin_min"`
	AskMarginAvg float64 `json:"ask_margin_avg"`
	AskMarginMax float64 `json:"ask_margin_max"`
	AskMarginMin float64 `json:"ask_margin_min"`
}

// marginProfileResult — Profile 1개의 통계 + 샘플.
type marginProfileResult struct {
	Profile session.Profile         `json:"profile"`
	Stats   marginRecomputeStats    `json:"stats"`
	Samples []marginRecomputeSample `json:"samples"`
}

type marginRecomputeResponse struct {
	BarsProcessed int                   `json:"bars_processed"`
	TableVersion  int64                 `json:"table_version"`
	TableSource   string                `json:"table_source"` // "etcd" / "override"
	Pair          session.Pair          `json:"pair"`
	From          time.Time             `json:"from"`
	To            time.Time             `json:"to"`
	Results       []marginProfileResult `json:"results"`

	// Legacy v1 backward compat — results 길이 1 일 때만 채움.
	Profile *session.Profile        `json:"profile,omitempty"`
	Stats   *marginRecomputeStats   `json:"stats,omitempty"`
	Samples []marginRecomputeSample `json:"samples,omitempty"`
}

// PostMarginRecompute — POST /v1/admin/margin/recompute.
func PostMarginRecompute(deps *MarginRecomputeDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Pool == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "no_chart_dsn",
				"mci-admin 의 --chart-dsn 미설정 — 마진 재계산 endpoint 비활성")
			return
		}
		var req marginRecomputeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad_json", err.Error())
			return
		}
		if req.From.IsZero() || req.To.IsZero() {
			writeJSONError(w, http.StatusBadRequest, "validation", "from / to 필수")
			return
		}
		if !req.From.Before(req.To) {
			writeJSONError(w, http.StatusBadRequest, "validation", "from < to 여야 함")
			return
		}
		if req.Pair == "" {
			writeJSONError(w, http.StatusBadRequest, "validation", "pair 필수")
			return
		}
		// Profile / Profiles 정규화 — 항상 1+ 개의 profile 배열.
		profiles := req.Profiles
		if len(profiles) == 0 {
			if req.Profile.Channel == "" || req.Profile.Site == "" {
				writeJSONError(w, http.StatusBadRequest, "validation",
					"profile 또는 profiles 필수 (channel / site / tier)")
				return
			}
			profiles = []session.Profile{req.Profile}
		}
		for i, p := range profiles {
			if p.Channel == "" || p.Site == "" {
				writeJSONError(w, http.StatusBadRequest, "validation",
					fmt.Sprintf("profiles[%d] channel / site 필수", i))
				return
			}
		}
		if len(profiles) > 10 {
			writeJSONError(w, http.StatusBadRequest, "validation",
				"profiles 최대 10 개")
			return
		}
		sampleLimit := req.SampleLimit
		if sampleLimit <= 0 {
			sampleLimit = marginDefaultSamples
		}
		if sampleLimit > marginMaxSamples {
			sampleLimit = marginMaxSamples
		}

		// 1) PricingTable 결정 — override 우선.
		tbl, tableSource, err := loadPricingTable(r.Context(), deps, req.TableOverride)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "pricing_load", err.Error())
			return
		}

		// 2) quote_bars 조회 — OHLC 전체.
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		rows, err := deps.Pool.Query(ctx, `
			SELECT opened_at,
			       open_bid, open_ask,
			       high_bid, high_ask,
			       low_bid,  low_ask,
			       close_bid, close_ask
			FROM quote_bars
			WHERE pair = $1
			  AND tf   = '1m'
			  AND opened_at >= $2
			  AND opened_at <  $3
			ORDER BY opened_at ASC
			LIMIT $4
		`, string(req.Pair), req.From, req.To, marginMaxBars)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "db", err.Error())
			return
		}
		defer rows.Close()

		// 3) 적용 + 통계 — profile 별 누적기.
		type accum struct {
			profile      session.Profile
			stats        marginRecomputeStats
			bidSum       float64
			askSum       float64
			samples      []marginRecomputeSample
			sampleStride int
		}
		accs := make([]*accum, len(profiles))
		for i, p := range profiles {
			accs[i] = &accum{
				profile: p,
				stats: marginRecomputeStats{
					BidMarginMax: math.Inf(-1),
					BidMarginMin: math.Inf(+1),
					AskMarginMax: math.Inf(-1),
					AskMarginMin: math.Inf(+1),
				},
				samples: make([]marginRecomputeSample, 0, sampleLimit),
			}
		}
		count := 0

		// applyPoint — profile 별 bid/ask 점에 PricingTable.ApplyAt.
		// P2 — 봉의 openedAt 기준 time window 매칭. 분쟁 재산정 시 당시 시점의
		// margin 을 재현해야 하므로 ApplyAt 필수.
		applyPoint := func(p session.Profile, bid, ask float64, ts time.Time) (float64, float64) {
			cq := tbl.ApplyAt(quote.Quote{Pair: req.Pair, Bid: bid, Ask: ask, TS: ts},
				p, pricing.TenorSpot, ts)
			return cq.Bid, cq.Ask
		}

		for rows.Next() {
			var openedAt time.Time
			var raw marginOHLC
			if err := rows.Scan(&openedAt,
				&raw.OpenBid, &raw.OpenAsk,
				&raw.HighBid, &raw.HighAsk,
				&raw.LowBid, &raw.LowAsk,
				&raw.CloseBid, &raw.CloseAsk,
			); err != nil {
				writeJSONError(w, http.StatusInternalServerError, "scan", err.Error())
				return
			}
			count++

			for _, a := range accs {
				// OHLC 4 점 PricingTable.Apply (profile 별).
				var cust marginOHLC
				cust.OpenBid, cust.OpenAsk = applyPoint(a.profile, raw.OpenBid, raw.OpenAsk, openedAt)
				cust.HighBid, cust.HighAsk = applyPoint(a.profile, raw.HighBid, raw.HighAsk, openedAt)
				cust.LowBid, cust.LowAsk = applyPoint(a.profile, raw.LowBid, raw.LowAsk, openedAt)
				cust.CloseBid, cust.CloseAsk = applyPoint(a.profile, raw.CloseBid, raw.CloseAsk, openedAt)

				bidDiff := cust.CloseBid - raw.CloseBid
				askDiff := cust.CloseAsk - raw.CloseAsk
				a.bidSum += bidDiff
				a.askSum += askDiff
				if bidDiff > a.stats.BidMarginMax {
					a.stats.BidMarginMax = bidDiff
				}
				if bidDiff < a.stats.BidMarginMin {
					a.stats.BidMarginMin = bidDiff
				}
				if askDiff > a.stats.AskMarginMax {
					a.stats.AskMarginMax = askDiff
				}
				if askDiff < a.stats.AskMarginMin {
					a.stats.AskMarginMin = askDiff
				}
				if a.sampleStride == 0 {
					a.sampleStride = 1
				}
				if len(a.samples) < sampleLimit && count%a.sampleStride == 0 {
					a.samples = append(a.samples, marginRecomputeSample{
						OpenedAt:    openedAt,
						RawBid:      raw.CloseBid,
						RawAsk:      raw.CloseAsk,
						CustomerBid: cust.CloseBid,
						CustomerAsk: cust.CloseAsk,
						BidMargin:   bidDiff,
						AskMargin:   askDiff,
						Raw:         raw,
						Customer:    cust,
					})
				}
			}
		}
		if rows.Err() != nil {
			writeJSONError(w, http.StatusInternalServerError, "rows", rows.Err().Error())
			return
		}

		// 누적기 → response.
		results := make([]marginProfileResult, len(accs))
		profileKeys := make([]string, len(accs))
		for i, a := range accs {
			if count == 0 {
				a.stats = marginRecomputeStats{}
			} else {
				a.stats.BidMarginAvg = a.bidSum / float64(count)
				a.stats.AskMarginAvg = a.askSum / float64(count)
			}
			results[i] = marginProfileResult{
				Profile: a.profile,
				Stats:   a.stats,
				Samples: a.samples,
			}
			profileKeys[i] = a.profile.Key()
		}

		// audit.
		if deps.Audit != nil {
			rd := &RoutingDeps{Logger: deps.Logger, Audit: deps.Audit}
			auditLog(rd, r, "MARGIN_RECOMPUTE",
				slog.String("pair", string(req.Pair)),
				slog.Any("profiles", profileKeys),
				slog.Int("bars", count),
				slog.String("source", tableSource),
			)
		}

		resp := marginRecomputeResponse{
			BarsProcessed: count,
			TableVersion:  tbl.Version,
			TableSource:   tableSource,
			Pair:          req.Pair,
			From:          req.From,
			To:            req.To,
			Results:       results,
		}
		// Legacy v1 backward compat — 단일 profile 일 때 top-level field 도 채움.
		if len(results) == 1 {
			p := results[0].Profile
			resp.Profile = &p
			resp.Stats = &results[0].Stats
			resp.Samples = results[0].Samples
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

func loadPricingTable(ctx context.Context, deps *MarginRecomputeDeps,
	override *pricing.PricingTableDoc) (*pricing.PricingTable, string, error) {
	if override != nil {
		return pricing.BuildPricingTable(*override), "override", nil
	}
	key := deps.EtcdKey
	if key == "" {
		key = "wtg/pricing/table"
	}
	if deps.Cli == nil {
		return nil, "", errNoEtcd
	}
	c, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	resp, err := deps.Cli.Get(c, key)
	if err != nil {
		return nil, "", err
	}
	if len(resp.Kvs) == 0 {
		// 빈 테이블 — version 0 + margin 없음. raw==customer.
		return &pricing.PricingTable{}, "etcd_empty", nil
	}
	tbl, err := pricing.ParsePricingTable(resp.Kvs[0].Value)
	if err != nil {
		return nil, "", err
	}
	return tbl, "etcd", nil
}

var errNoEtcd = errAdmin("etcd client 없음 — table_override 필수")

type errAdmin string

func (e errAdmin) Error() string { return string(e) }

package admin

import (
	"context"
	"encoding/json"
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
	Cli       *clientv3.Client // etcd — PricingTable 현재값 조회
	Pool      *pgxpool.Pool    // TimescaleDB — quote_bars
	EtcdKey   string           // default "wtg/pricing/table"
	Logger    *slog.Logger
	Audit     *AuditRing
}

type marginRecomputeRequest struct {
	From          time.Time                `json:"from"`
	To            time.Time                `json:"to"`
	Pair          session.Pair             `json:"pair"`
	Profile       session.Profile          `json:"profile"`
	TableOverride *pricing.PricingTableDoc `json:"table_override,omitempty"`
	SampleLimit   int                      `json:"sample_limit,omitempty"`
}

type marginRecomputeSample struct {
	OpenedAt    time.Time `json:"opened_at"`
	RawBid      float64   `json:"raw_bid"`
	RawAsk      float64   `json:"raw_ask"`
	CustomerBid float64   `json:"customer_bid"`
	CustomerAsk float64   `json:"customer_ask"`
	BidMargin   float64   `json:"bid_margin"` // customer_bid - raw_bid (보통 ≤ 0)
	AskMargin   float64   `json:"ask_margin"` // customer_ask - raw_ask (보통 ≥ 0)
}

type marginRecomputeStats struct {
	BidMarginAvg float64 `json:"bid_margin_avg"`
	BidMarginMax float64 `json:"bid_margin_max"`
	BidMarginMin float64 `json:"bid_margin_min"`
	AskMarginAvg float64 `json:"ask_margin_avg"`
	AskMarginMax float64 `json:"ask_margin_max"`
	AskMarginMin float64 `json:"ask_margin_min"`
}

type marginRecomputeResponse struct {
	BarsProcessed int                     `json:"bars_processed"`
	TableVersion  int64                   `json:"table_version"`
	TableSource   string                  `json:"table_source"` // "etcd" / "override"
	Profile       session.Profile         `json:"profile"`
	Pair          session.Pair            `json:"pair"`
	From          time.Time               `json:"from"`
	To            time.Time               `json:"to"`
	Stats         marginRecomputeStats    `json:"stats"`
	Samples       []marginRecomputeSample `json:"samples"`
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
		if req.Profile.Channel == "" || req.Profile.Site == "" {
			writeJSONError(w, http.StatusBadRequest, "validation",
				"profile (channel / site / tier) 필수")
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

		// 2) quote_bars 조회.
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		rows, err := deps.Pool.Query(ctx, `
			SELECT opened_at, close_bid, close_ask
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

		// 3) 적용 + 통계.
		stats := marginRecomputeStats{
			BidMarginMax: math.Inf(-1),
			BidMarginMin: math.Inf(+1),
			AskMarginMax: math.Inf(-1),
			AskMarginMin: math.Inf(+1),
		}
		var bidSum, askSum float64
		samples := make([]marginRecomputeSample, 0, sampleLimit)
		count := 0
		sampleStride := 0

		for rows.Next() {
			var openedAt time.Time
			var closeBid, closeAsk float64
			if err := rows.Scan(&openedAt, &closeBid, &closeAsk); err != nil {
				writeJSONError(w, http.StatusInternalServerError, "scan", err.Error())
				return
			}
			cq := tbl.Apply(quote.Quote{
				Pair: req.Pair,
				Bid:  closeBid,
				Ask:  closeAsk,
				TS:   openedAt,
			}, req.Profile, pricing.TenorSpot)
			bidDiff := cq.Bid - closeBid
			askDiff := cq.Ask - closeAsk

			bidSum += bidDiff
			askSum += askDiff
			if bidDiff > stats.BidMarginMax {
				stats.BidMarginMax = bidDiff
			}
			if bidDiff < stats.BidMarginMin {
				stats.BidMarginMin = bidDiff
			}
			if askDiff > stats.AskMarginMax {
				stats.AskMarginMax = askDiff
			}
			if askDiff < stats.AskMarginMin {
				stats.AskMarginMin = askDiff
			}
			count++

			// 샘플 stride — 전체에서 sampleLimit 개 고르게.
			if sampleStride == 0 {
				sampleStride = 1
			}
			if len(samples) < sampleLimit && count%sampleStride == 0 {
				samples = append(samples, marginRecomputeSample{
					OpenedAt:    openedAt,
					RawBid:      closeBid,
					RawAsk:      closeAsk,
					CustomerBid: cq.Bid,
					CustomerAsk: cq.Ask,
					BidMargin:   bidDiff,
					AskMargin:   askDiff,
				})
			}
		}
		if rows.Err() != nil {
			writeJSONError(w, http.StatusInternalServerError, "rows", rows.Err().Error())
			return
		}
		if count == 0 {
			stats = marginRecomputeStats{} // Inf 정리.
		} else {
			stats.BidMarginAvg = bidSum / float64(count)
			stats.AskMarginAvg = askSum / float64(count)
		}

		// audit.
		if deps.Audit != nil {
			rd := &RoutingDeps{Logger: deps.Logger, Audit: deps.Audit}
			auditLog(rd, r, "MARGIN_RECOMPUTE",
				slog.String("pair", string(req.Pair)),
				slog.String("profile", req.Profile.Key()),
				slog.Int("bars", count),
				slog.String("source", tableSource),
			)
		}

		writeJSON(w, http.StatusOK, marginRecomputeResponse{
			BarsProcessed: count,
			TableVersion:  tbl.Version,
			TableSource:   tableSource,
			Profile:       req.Profile,
			Pair:          req.Pair,
			From:          req.From,
			To:            req.To,
			Stats:         stats,
			Samples:       samples,
		})
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

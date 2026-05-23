package chart

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/winwaysystems/wtg/pkg/quote"
	"github.com/winwaysystems/wtg/pkg/session"
)

// BarDTO 는 quote.Bar 의 외부 노출용 JSON 모양.
// 내부 타입(quote.Bar)과 분리하여 API 안정성 확보 — 내부 모델은 자유롭게 진화.
type BarDTO struct {
	OpenedAt  time.Time `json:"opened_at"`
	ClosedAt  time.Time `json:"closed_at"`
	OpenBid   float64   `json:"open_bid"`
	OpenAsk   float64   `json:"open_ask"`
	HighBid   float64   `json:"high_bid"`
	HighAsk   float64   `json:"high_ask"`
	LowBid    float64   `json:"low_bid"`
	LowAsk    float64   `json:"low_ask"`
	CloseBid  float64   `json:"close_bid"`
	CloseAsk  float64   `json:"close_ask"`
	TickCount int       `json:"tick_count"`
}

// chartResponse 는 GET /v1/chart 의 응답 envelope.
type chartResponse struct {
	Pair  session.Pair    `json:"pair"`
	TF    quote.Timeframe `json:"tf"`
	From  time.Time       `json:"from"`
	To    time.Time       `json:"to"`
	Count int             `json:"count"`
	Bars  []BarDTO        `json:"bars"`
}

// errorResponse 는 표준 에러 envelope.
type errorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// chartParams 는 파싱된 요청 파라미터.
type chartParams struct {
	Pair  session.Pair
	TF    quote.Timeframe
	From  time.Time
	To    time.Time
	Limit int
}

// parseChartParams 는 URL query 를 파싱·검증한다.
//
// 필수: pair, tf, from, to
// 옵션: limit (default = maxLimit)
//
// 모든 시각은 RFC3339 (예: "2026-05-23T12:00:00Z"). from < to 강제.
// tf 는 PersistentTimeframes 만 허용 (TF1s 는 DB 영속 안 됨).
func parseChartParams(r *http.Request, maxLimit int) (chartParams, error) {
	q := r.URL.Query()
	var p chartParams

	rawPair := strings.TrimSpace(q.Get("pair"))
	if rawPair == "" {
		return p, errors.New("pair 필수")
	}
	p.Pair = session.Pair(rawPair)

	rawTF := strings.TrimSpace(q.Get("tf"))
	if rawTF == "" {
		return p, errors.New("tf 필수")
	}
	tf := quote.Timeframe(rawTF)
	if !tf.Persistent() {
		return p, fmt.Errorf("tf %q 는 영속 timeframe 아님 (지원: 1m,5m,15m,1h,1d)", rawTF)
	}
	p.TF = tf

	from, err := time.Parse(time.RFC3339, q.Get("from"))
	if err != nil {
		return p, fmt.Errorf("from 파싱 실패 (RFC3339 필요): %w", err)
	}
	to, err := time.Parse(time.RFC3339, q.Get("to"))
	if err != nil {
		return p, fmt.Errorf("to 파싱 실패 (RFC3339 필요): %w", err)
	}
	if !to.After(from) {
		return p, errors.New("to 는 from 이후여야 함")
	}
	p.From = from
	p.To = to

	p.Limit = maxLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return p, fmt.Errorf("limit 양의 정수 필요 (got %q)", v)
		}
		if n > maxLimit {
			n = maxLimit
		}
		p.Limit = n
	}
	return p, nil
}

// handleChart 는 GET /v1/chart 핸들러.
func handleChart(repo Repository, maxRows int, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		params, err := parseChartParams(r, maxRows)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}

		bars, err := repo.QueryBars(r.Context(), params.Pair, params.TF, params.From, params.To, params.Limit)
		if err != nil {
			logger.Error("repository.QueryBars 실패",
				slog.String("pair", string(params.Pair)),
				slog.String("tf", string(params.TF)),
				slog.Any("error", err),
			)
			writeError(w, http.StatusInternalServerError, "internal_error", "봉 조회 실패")
			return
		}

		dtos := make([]BarDTO, len(bars))
		for i, b := range bars {
			dtos[i] = barToDTO(b)
		}

		writeJSON(w, http.StatusOK, chartResponse{
			Pair:  params.Pair,
			TF:    params.TF,
			From:  params.From,
			To:    params.To,
			Count: len(dtos),
			Bars:  dtos,
		})
	}
}

// barToDTO — quote.Bar → BarDTO.
func barToDTO(b quote.Bar) BarDTO {
	return BarDTO{
		OpenedAt:  b.OpenedAt,
		ClosedAt:  b.ClosedAt,
		OpenBid:   b.OpenBid,
		OpenAsk:   b.OpenAsk,
		HighBid:   b.HighBid,
		HighAsk:   b.HighAsk,
		LowBid:    b.LowBid,
		LowAsk:    b.LowAsk,
		CloseBid:  b.CloseBid,
		CloseAsk:  b.CloseAsk,
		TickCount: b.TickCount,
	}
}

// writeJSON / writeError — 공용 응답 헬퍼.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, errorResponse{Code: code, Message: msg})
}

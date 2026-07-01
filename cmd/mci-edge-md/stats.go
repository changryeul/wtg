package main

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/winwaysystems/wtg/internal/edge/md"
)

// startStatsServer — HTTP /stats + /healthz + /metrics + /v1/internal/quote.
//
// /v1/internal/quote 는 Phase A 데모용 — 하드코딩 provider 의 quote 를 조회 or
// override 가능. Phase B 에서 mci-price gRPC 로 대체.
func startStatsServer(addr string, srv *md.Server, logger *slog.Logger) {
	mux := http.NewServeMux()
	withCORS := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("/stats", withCORS(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(srv.Stats())
	}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	// Phase A — provider override endpoint (test/데모).
	mux.HandleFunc("POST /v1/internal/quote", withCORS(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Symbol string  `json:"symbol"`
			Bid    float64 `json:"bid"`
			Ask    float64 `json:"ask"`
			Scale  int32   `json:"scale"`
			Size   float64 `json:"size"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, `{"error":"bad_json"}`, http.StatusBadRequest)
			return
		}
		if body.Symbol == "" || body.Scale < 0 {
			http.Error(w, `{"error":"validation","message":"symbol/scale"}`, http.StatusBadRequest)
			return
		}
		if body.Size == 0 {
			body.Size = 1_000_000
		}
		srv.Provider().Set(body.Symbol, md.StaticQuote{
			Bid: body.Bid, Ask: body.Ask, Scale: body.Scale, Size: body.Size,
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	mux.Handle("GET /metrics", md.MetricsHandler())
	logger.Info("stats HTTP listen", slog.String("addr", addr))
	if err := http.ListenAndServe(addr, mux); err != nil {
		logger.Warn("stats server 종료", slog.Any("err", err))
	}
}

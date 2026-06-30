package main

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/winwaysystems/wtg/internal/edge/fix"
)

// startStatsServer — 옵션 HTTP /stats endpoint. 운영 진단용.
func startStatsServer(addr string, srv *fix.Server, logger *slog.Logger) {
	mux := http.NewServeMux()
	mux.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(srv.Stats())
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	logger.Info("stats HTTP listen", slog.String("addr", addr))
	if err := http.ListenAndServe(addr, mux); err != nil {
		logger.Warn("stats server 종료", slog.Any("err", err))
	}
}

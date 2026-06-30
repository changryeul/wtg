package main

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/winwaysystems/wtg/internal/edge/fix"
)

// startStatsServer — HTTP /stats + /healthz + /v1/internal/exec-report.
// 운영 진단 + Phase B-2 drop copy receive endpoint.
//
// pushSecret 이 채워지면 exec-report endpoint 의 X-Push-Secret 검증.
func startStatsServer(addr string, srv *fix.Server, pushSecret string, logger *slog.Logger) {
	mux := http.NewServeMux()
	mux.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(srv.Stats())
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("POST /v1/internal/exec-report", fix.ExecReportHandler(fix.ExecReportHandlerDeps{
		Server: srv,
		Secret: pushSecret,
		Logger: logger,
	}))
	// Phase D — Prometheus metrics endpoint.
	mux.Handle("GET /metrics", fix.MetricsHandler())
	logger.Info("stats HTTP listen",
		slog.String("addr", addr),
		slog.Bool("push_secret", pushSecret != ""))
	if err := http.ListenAndServe(addr, mux); err != nil {
		logger.Warn("stats server 종료", slog.Any("err", err))
	}
}

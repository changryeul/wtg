package fix

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
)

// exec_report_http.go — POST /v1/internal/exec-report endpoint.
//
// 매매 엔진 또는 mci-push 가 channel=FIX 사용자의 ExecutionReport (35=8)
// drop copy 를 mci-edge-fix 로 보내는 endpoint. X-Push-Secret 인증 (mci-push
// 의 HTTP push 와 동일 패턴).
//
// 흐름:
//   POST /v1/internal/exec-report
//     headers: X-Push-Secret: <secret>
//     body: ExecReportPayload JSON
//   → Server.SendExecReport → quickfix.SendToTarget(35=8)

// ExecReportHandlerDeps — handler 의존성.
type ExecReportHandlerDeps struct {
	Server *Server
	Secret string // 빈값이면 인증 skip (dev — 운영 금지)
	Logger *slog.Logger
}

// ExecReportHandler — POST /v1/internal/exec-report.
func ExecReportHandler(deps ExecReportHandlerDeps) http.HandlerFunc {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return func(w http.ResponseWriter, r *http.Request) {
		// X-Push-Secret 인증 (mci-push 와 동일 패턴).
		if deps.Secret != "" {
			got := r.Header.Get("X-Push-Secret")
			if subtle.ConstantTimeCompare([]byte(got), []byte(deps.Secret)) != 1 {
				writeErr(w, http.StatusUnauthorized, "unauthorized",
					"X-Push-Secret 헤더 누락 또는 불일치")
				return
			}
		}
		var p ExecReportPayload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			writeErr(w, http.StatusBadRequest, "bad_json", err.Error())
			return
		}
		if err := p.Validate(); err != nil {
			writeErr(w, http.StatusBadRequest, "validation", err.Error())
			return
		}
		if err := deps.Server.SendExecReport(p); err != nil {
			// session 미활성 / 변환 실패 — 503 (운영자가 재시도 가능).
			logger.Warn("exec-report send 실패",
				slog.String("target", p.TargetSenderCompID),
				slog.String("exec_id", p.ExecID),
				slog.Any("err", err))
			writeErr(w, http.StatusServiceUnavailable, "send_failed", err.Error())
			return
		}
		logger.Info("exec-report 송신",
			slog.String("target", p.TargetSenderCompID),
			slog.String("order_id", p.OrderID),
			slog.String("exec_id", p.ExecID),
			slog.String("exec_type", p.ExecType))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":   code,
		"message": msg,
	})
}

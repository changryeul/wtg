package handlers

import (
	"net/http"
	"time"
)

// Ping 은 헬스체크 엔드포인트. 인증 우회 (middleware/auth.go 의 isPublicPath).
//
// 실제 시스템 상태(broker 연결 등)는 /healthz 에 추가될 수 있다 — 1차는 단순
// liveness 만 200 으로 응답.
func Ping(deps *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "ok",
			"service": "mci-api",
			"time":    time.Now().UTC().Format(time.RFC3339),
		})
	}
}

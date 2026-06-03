package handlers

import (
	"bytes"
	"crypto/sha256"
	"log/slog"
	"net/http"

	"github.com/winwaysystems/wtg/internal/api/middleware"
	"github.com/winwaysystems/wtg/pkg/idempotency"
)

// reserveIdempotency — Idempotency-Key 헤더 + store 둘 다 활성 시 reserve.
//
// 반환:
//   - idemKey: store 안 사용된 정규화 key (usid|header). idemActive=true 일 때만 유효.
//   - idemActive: leader path — 호출자가 응답 후 Commit/Rollback 책임.
//   - handled: Cached / Conflict / InFlight → 응답 이미 보냄. 호출자는 즉시 return.
//
// 헤더 부재 / store nil / Reserve 실패 (fail-open) → (idemActive=false, handled=false).
func reserveIdempotency(
	w http.ResponseWriter, r *http.Request, deps *Deps,
	usid string, bodyBytes []byte, recordAlias func(bool),
) (idemKey string, idemActive, handled bool) {
	if deps.Idempotency == nil {
		return "", false, false
	}
	hdr := r.Header.Get("Idempotency-Key")
	if hdr == "" {
		return "", false, false
	}
	idemKey = idempotency.MakeKey(usid, hdr)
	bodyHash := sha256.Sum256(bodyBytes)
	st, cached, err := deps.Idempotency.Reserve(r.Context(), idemKey, bodyHash)
	if err != nil {
		// store 자체 실패 — fail-open (운영 안전성). 정상 처리 진행 + log warn.
		deps.Logger.WarnContext(r.Context(), "idempotency Reserve 실패 — 비활성 진행",
			slog.String("rid", middleware.RequestIDFromContext(r.Context())),
			slog.Any("error", err))
		return "", false, false
	}
	switch st {
	case idempotency.StatusCached:
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Idempotency-Cached", "true")
		w.WriteHeader(cached.StatusCode)
		_, _ = w.Write(cached.Body)
		recordAlias(cached.StatusCode >= 400)
		return idemKey, false, true
	case idempotency.StatusConflict:
		writeError(w, http.StatusConflict, "idempotency_conflict",
			"동일 Idempotency-Key 에 다른 request body — 새 key 사용 필요")
		recordAlias(true)
		return idemKey, false, true
	case idempotency.StatusInFlight:
		writeError(w, http.StatusConflict, "idempotency_in_flight",
			"동일 Idempotency-Key 의 다른 요청 처리 중 — 잠시 후 재시도")
		recordAlias(true)
		return idemKey, false, true
	}
	// StatusMiss — leader path.
	return idemKey, true, false
}

// captureWriter — response status + body 를 capture 해서 caller (handler)
// 가 Idempotency Commit 시 그대로 캐시. http.ResponseWriter 의 embedding 으로
// 모든 미설정 method 는 underlying 으로 passthrough.
type captureWriter struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (c *captureWriter) WriteHeader(s int) {
	c.status = s
	c.ResponseWriter.WriteHeader(s)
}

func (c *captureWriter) Write(b []byte) (int, error) {
	c.body.Write(b)
	return c.ResponseWriter.Write(b)
}

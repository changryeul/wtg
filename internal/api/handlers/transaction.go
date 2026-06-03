package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/winwaysystems/wtg/internal/api/middleware"
	"github.com/winwaysystems/wtg/internal/api/transform"
	"github.com/winwaysystems/wtg/pkg/policy"
)

// extractSymbol 은 envelope.Data (raw JSON) 안의 symbol 필드를 추출한다.
//
// 정책 엔진의 BlockedSymbols 검사용. WTG 가 페이로드를 "해석" 하는 것은 아니고
// 단지 운영 차단 키 매칭만 — 매매 엔진의 비즈니스 처리에는 영향 없음.
// data 가 JSON 이 아니거나 symbol 이 없으면 빈 문자열.
func extractSymbol(data json.RawMessage) string {
	if len(data) == 0 {
		return ""
	}
	var probe struct {
		Symbol string `json:"symbol"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return ""
	}
	return probe.Symbol
}

// Transaction 은 모든 매매 transaction 을 broker 로 그대로 통과시키는
// generic passthrough 핸들러다 (POST /v1/tx).
//
// transaction 별 핸들러를 별도로 만들지 않는다 (인증 위임 원칙과 동일 맥락).
// 자세한 배경은 docs/conventions.md 와 메모리의 passthrough 패턴 참조.
//
// 흐름:
//  1. JWT/DevMode 인증 통과 (Principal 추출)
//  2. JSON envelope 디코딩 + transport-level 검증
//  3. transform.Envelope.BuildFrame 으로 MyMQ frame 구성
//  4. mq.Call() — broker 가 매매 엔진에 라우팅
//  5. 응답 envelope 으로 raw passthrough
func Transaction(deps *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalRequired(w, r)
		if !ok {
			return
		}

		var env transform.Envelope
		if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
			writeError(w, http.StatusBadRequest, "bad_json", err.Error())
			return
		}
		if err := env.ValidateRequest(); err != nil {
			writeError(w, http.StatusBadRequest, "validation", err.Error())
			return
		}
		// alias 별 호출 시작 시각 — 응답 직전까지 latency 측정.
		callStart := time.Now()
		recordAlias := func(isErr bool) {
			deps.AliasMetrics.RecordCall(env.Alias, time.Since(callStart), isErr)
		}

		// 운영 정책 검사 — kill switch / 정비창 / 차단 심볼·routing-key.
		// 비즈니스 거부는 매매 엔진이 담당, 본 검사는 운영 차원만 (auth.md §1).
		if deps.Policy != nil {
			req := policy.Request{
				Usid:       p.Usid,
				Channel:    p.Channel,
				Alias:      env.Alias,
				Exchange:   env.Exchange,
				RoutingKey: env.RoutingKey,
				Symbol:     extractSymbol(env.Data),
			}
			if d := deps.Policy.Check(req); !d.Allowed {
				deps.Logger.WarnContext(r.Context(), "정책 차단",
					slog.String("usid", p.Usid),
					slog.String("reason", d.Reason),
					slog.String("rkey", env.RoutingKey),
					slog.String("rid", middleware.RequestIDFromContext(r.Context())),
				)
				// kill_switch / maintenance → 503, 차단 심볼/rkey → 403.
				status := http.StatusForbidden
				if d.Reason == policy.ReasonKillSwitch || d.Reason == policy.ReasonMaintenance {
					status = http.StatusServiceUnavailable
				}
				recordAlias(true)
				writeError(w, status, d.Reason, d.Message)
				return
			}
		}

		// W3C tracecontext trace_id 우선 (16B = mqhdr.trcid 전체).
		// 없으면 X-Request-ID 8B 폴백 (trcid[0..7] 만).
		traceIDHex := middleware.TraceIDHexFromContext(r.Context())
		if traceIDHex == "" {
			traceIDHex = middleware.RequestIDFromContext(r.Context())
		}
		frame, err := env.BuildFrame(0, p.Usid, traceIDHex, deps.Routes)
		if err != nil {
			recordAlias(true)
			if errors.Is(err, transform.ErrUnknownAlias) {
				writeError(w, http.StatusNotFound, "unknown_alias", err.Error())
				return
			}
			writeError(w, http.StatusBadRequest, "build_frame", err.Error())
			return
		}
		// SessionMode 면 Principal 에 cookie_t 가 들어와 있다 — 매매 엔진 권한
		// 검증에 필요. DevMode 에서는 nil 이라 attach 안 됨.
		if p.Cookie != nil {
			frame.Cookie = p.Cookie
		}

		callCtx, cancel := context.WithTimeout(r.Context(), deps.CallTimeout)
		defer cancel()
		reply, err := deps.MQ.Call(callCtx, frame)
		if err != nil {
			deps.Logger.WarnContext(r.Context(), "broker Call 실패",
				slog.String("path", r.URL.Path),
				slog.String("usid", p.Usid),
				slog.String("xchg", env.Exchange),
				slog.String("rkey", env.RoutingKey),
				slog.String("rid", middleware.RequestIDFromContext(r.Context())),
				slog.Any("error", err),
			)
			status, code, msg := mapBrokerError(err)
			recordAlias(true)
			writeError(w, status, code, msg)
			return
		}

		// 비즈니스 에러 (errn != 0) 도 표준 매핑.
		// envelope 에 errn/errm/data 모두 포함시켜 클라이언트가 디테일을 받을 수 있게.
		if mqErr := reply.AsError(); mqErr != nil {
			status, _, _ := mapBrokerError(mqErr)
			recordAlias(true)
			writeJSON(w, status, transform.FromReply(reply))
			return
		}

		recordAlias(false)
		writeJSON(w, http.StatusOK, transform.FromReply(reply))
	}
}

// Package push — Phase-1 PoC: broker 없이 unsolicited 주입 받는 HTTP endpoint.
//
// 운영 svc 가 broker 의존을 점차 줄이도록, mci-push 가 HTTP POST 로도 push
// 메시지를 받을 수 있게 한다. 기존 broker subscribe 와 병행 — dispatcher 가
// 두 source 의 메시지를 동일 fan-out 흐름으로 처리.
package push

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/winwaysystems/wtg/pkg/mymq"
)

// HTTPPushRequest — POST /v1/internal/push 의 body.
//
// 의도:
//   - user 가 비어있으면 전체 broadcast (LogonID="")
//   - user 가 채워져 있으면 user-targeted (FC_PUSH 흐름과 동일)
//   - func/subc 를 명시 안 하면 user 있으면 FCPush/SubPush, 없으면 FCCast/SubBroadcast
//   - data 는 JSON 그대로 또는 raw — envelope.data 필드로 그대로 전달
type HTTPPushRequest struct {
	User string          `json:"user,omitempty"` // 대상 LogonID. 빈값 = broadcast
	Func uint8           `json:"func,omitempty"` // default: user 있으면 13(FCPush), 없으면 4(FCCast)
	Subc uint8           `json:"subc,omitempty"` // default: user 있으면 54(SubPush), 없으면 50(SubBroadcast)
	Data json.RawMessage `json:"data"`           // payload — 비어있으면 null
}

// HTTPPushResponse — 발사 결과.
type HTTPPushResponse struct {
	Injected  bool   `json:"injected"`
	Func      uint8  `json:"func"`
	Subc      uint8  `json:"subc"`
	User      string `json:"user,omitempty"`
	BodySize  int    `json:"body_size"`
}

// HTTPPushHandler — POST /v1/internal/push.
//
// 인증 (두 계층 독립 — 운영자가 하나 이상 활성하면 됨):
//  1. mTLS — server cfg 의 HTTPTLSClientCAFile 활성 시 TLS handshake 단계에서
//     자동 검증. 핸들러는 r.TLS.PeerCertificates 의 CN 을 audit log 에 기록만.
//  2. shared secret — `X-Push-Secret` 헤더가 secret 과 일치해야. secret 빈값이면
//     이 계층 비활성. subtle.ConstantTimeCompare 로 timing-safe.
//
// 둘 다 비활성 시 (secret 빈 + mTLS 없음) 인증 wide-open — dev 전용 (서버 부팅 시
// warn log). 운영은 둘 중 하나 (또는 둘 다) 활성 권장.
//
// 동작:
//  1. body decode
//  2. HTTPPushRequest → mymq.Unsolicited 변환 (BroadcastHeader prefix + body)
//  3. dispatcher.Inject — non-blocking. buffer full 시 503.
//
// 호출자 (운영 svc 또는 admin tester) 는 broker 우회 path 로 동일 fan-out 결과.
func HTTPPushHandler(disp *Dispatcher, secret string) http.HandlerFunc {
	return HTTPPushHandlerWithLogger(disp, secret, nil)
}

// HTTPPushHandlerWithLogger — HTTPPushHandler + audit logger.
// logger != nil 이면 mTLS client cert 의 CN/SAN 을 INFO log 로 audit.
func HTTPPushHandlerWithLogger(disp *Dispatcher, secret string, logger *slog.Logger) http.HandlerFunc {
	return HTTPPushHandlerDeps(HTTPPushDeps{
		Dispatcher: disp,
		Secret:     secret,
		Logger:     logger,
	})
}

// HTTPPushDeps — HTTPPushHandlerDeps 의 의존성. Phase 2.5 metrics hook 포함.
type HTTPPushDeps struct {
	Dispatcher *Dispatcher
	Secret     string
	Logger     *slog.Logger
	// OnInject — 각 요청 후 호출 (cn, result) — metrics hook. nil OK.
	// cn   — mTLS client CN (없으면 "anonymous")
	// result — "ok" | "unauthorized" | "bad_json" | "inject_full"
	OnInject func(cn, result string)
}

// HTTPPushHandlerDeps — POST /v1/internal/push 의 통합 entry point.
// metrics hook 으로 CN 별 inject counter / 인증 실패 / 채널 full 등을 집계 가능.
func HTTPPushHandlerDeps(deps HTTPPushDeps) http.HandlerFunc {
	disp := deps.Dispatcher
	secret := deps.Secret
	logger := deps.Logger
	emit := func(cn, result string) {
		if deps.OnInject != nil {
			deps.OnInject(cn, result)
		}
	}
	return func(w http.ResponseWriter, r *http.Request) {
		// mTLS audit + CN 추출 (cardinality 보호 — CN 없으면 "anonymous").
		peerCN := "anonymous"
		if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
			peerCN = r.TLS.PeerCertificates[0].Subject.CommonName
			if peerCN == "" {
				peerCN = "unknown"
			}
			if logger != nil {
				logger.Info("push: mTLS client",
					slog.String("cn", peerCN),
					slog.String("remote", r.RemoteAddr),
				)
			}
		}
		if secret != "" {
			got := r.Header.Get("X-Push-Secret")
			if subtle.ConstantTimeCompare([]byte(got), []byte(secret)) != 1 {
				emit(peerCN, "unauthorized")
				writePushJSONErr(w, http.StatusUnauthorized, "unauthorized",
					"X-Push-Secret 헤더 누락 또는 불일치")
				return
			}
		}
		var req HTTPPushRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			emit(peerCN, "bad_json")
			writePushJSONErr(w, http.StatusBadRequest, "bad_json", err.Error())
			return
		}

		// Func/Subc default — user 명시 여부로 분기.
		fn, subc := req.Func, req.Subc
		if fn == 0 {
			if req.User != "" {
				fn = byte(mymq.FCPush)
			} else {
				fn = byte(mymq.FCCast)
			}
		}
		if subc == 0 {
			if req.User != "" {
				subc = byte(mymq.SubPush)
			} else {
				subc = byte(mymq.SubBroadcast)
			}
		}

		// payload — 비면 default 메시지.
		payload := []byte(req.Data)
		if len(payload) == 0 {
			payload = []byte(`null`)
		}

		// Unsolicited 구성 — broker subscribe path 와 동등.
		// Prefix.LogonID = req.User (단순 fan-out 분기에만 사용).
		var prefix mymq.BroadcastHeader
		copy(prefix.LogonID[:], req.User)
		prefix.Function = fn
		prefix.SubFunction = subc

		msg := &mymq.Unsolicited{
			Header: mymq.Header{
				Func: mymq.Func(fn),
				Subc: mymq.Subc(subc),
			},
			Prefix: &prefix,
			Body:   payload,
		}

		if !disp.Inject(msg) {
			emit(peerCN, "inject_full")
			writePushJSONErr(w, http.StatusServiceUnavailable, "inject_full",
				"dispatcher inject channel full — 잠시 후 재시도")
			return
		}

		emit(peerCN, "ok")
		_ = json.NewEncoder(w).Encode(HTTPPushResponse{
			Injected: true,
			Func:     fn,
			Subc:     subc,
			User:     req.User,
			BodySize: len(payload),
		})
	}
}

func writePushJSONErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":   code,
		"message": msg,
	})
}

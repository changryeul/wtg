// Package push — Phase-1 PoC: broker 없이 unsolicited 주입 받는 HTTP endpoint.
//
// 운영 svc 가 broker 의존을 점차 줄이도록, mci-push 가 HTTP POST 로도 push
// 메시지를 받을 수 있게 한다. 기존 broker subscribe 와 병행 — dispatcher 가
// 두 source 의 메시지를 동일 fan-out 흐름으로 처리.
package push

import (
	"crypto/subtle"
	"encoding/json"
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
// 인증: 헤더 `X-Push-Secret` 가 cfg.PushSecret 과 일치해야 함. secret 빈값이면
// 인증 disable (dev 전용). subtle.ConstantTimeCompare 로 timing-safe.
//
// 동작:
//  1. body decode
//  2. HTTPPushRequest → mymq.Unsolicited 변환 (BroadcastHeader prefix + body)
//  3. dispatcher.Inject — non-blocking. buffer full 시 503.
//
// 호출자 (운영 svc 또는 admin tester) 는 broker 우회 path 로 동일 fan-out 결과.
func HTTPPushHandler(disp *Dispatcher, secret string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if secret != "" {
			got := r.Header.Get("X-Push-Secret")
			if subtle.ConstantTimeCompare([]byte(got), []byte(secret)) != 1 {
				writePushJSONErr(w, http.StatusUnauthorized, "unauthorized",
					"X-Push-Secret 헤더 누락 또는 불일치")
				return
			}
		}
		var req HTTPPushRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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
			writePushJSONErr(w, http.StatusServiceUnavailable, "inject_full",
				"dispatcher inject channel full — 잠시 후 재시도")
			return
		}

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

package admin

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/winwaysystems/wtg/pkg/mymq"
	pushsdk "github.com/winwaysystems/wtg/pkg/push"
)

// pushTestRequest 는 POST /v1/admin/push-test 의 본문.
//
// user  : 메시지를 받을 대상 사용자 logonID. mci-push 가 ws fan-out 시 이 값으로
//
//	Registry 를 조회하므로, ws connect 시 X-WTG-User 와 동일해야 한다.
//
// data  : 페이로드. JSON 이면 그대로, string 이면 raw bytes 로 broadcast prefix 뒤에
//
//	붙는다. 비면 default 메시지.
type pushTestRequest struct {
	User string          `json:"user"`
	Data json.RawMessage `json:"data,omitempty"`
	// Phase-2: source 토글. "broker" (default) 또는 "http".
	//   - broker : mci-admin 의 broker connection 으로 발사 (legacy path)
	//   - http   : pkg/push 의 client 로 mci-push HTTP 직접 호출 (broker 우회)
	Source string `json:"source,omitempty"`
}

// pushTestResponse — 발사 결과.
type pushTestResponse struct {
	Sent      bool   `json:"sent"`
	Source    string `json:"source"` // "broker" | "http"
	Func      uint8  `json:"func"`
	Subc      uint8  `json:"subc"`
	BodySize  int    `json:"body_size"`
	TargetUID string `json:"target_uid"`
}

// PushTestHandler 는 POST /v1/admin/push-test — mci-admin 의 broker connection 을
// 통해 user-targeted unsolicited 메시지(FC_PUSH/SubPush) 를 발사한다.
//
// 흐름:
//  1. body 의 user → broadcast prefix.LogonID 채움
//  2. body 의 data → broadcast prefix(80B) 뒤에 그대로 붙여 FrameInput.Body 구성
//  3. mci-admin 의 broker connection.Send() — 응답 없는 단방향
//  4. broker 가 자체 routing 으로 mci-push (QF_UNSOL_HDR sub) 에 전달
//  5. mci-push.Dispatcher 가 prefix.LogonID 로 ws Registry fan-out
//
// 인증/IP 화이트리스트는 admin chain 가 통과시킨 상태이며, NoBroker 모드에선 503.
//
// 운영 안전: 이 endpoint 는 직접 broker push 발사가 가능하므로 DevMode 검증용.
// 운영 mci-admin 에선 enable 하지 말 것 (현재 조건 = mq != nil 이면 활성).
// PushTestDeps — PushTestHandler 의 의존성. Phase-2 에 source toggle 추가.
type PushTestDeps struct {
	BrokerClient *mymq.Client     // broker source (legacy path). nil = broker source 비활성
	HTTPClient   *pushsdk.Client  // mci-push HTTP source (Phase-2 신규). nil = http source 비활성
	Logger       *slog.Logger
}

func PushTestHandler(deps *PushTestDeps) http.HandlerFunc {
	if deps == nil || (deps.BrokerClient == nil && deps.HTTPClient == nil) {
		return func(w http.ResponseWriter, r *http.Request) {
			writePushErr(w, http.StatusServiceUnavailable, "no_source",
				"broker connection 도 HTTP push client 도 미설정 — push 발사 불가")
		}
	}
	return func(w http.ResponseWriter, r *http.Request) {
		var req pushTestRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writePushErr(w, http.StatusBadRequest, "bad_json", err.Error())
			return
		}
		if req.User == "" {
			writePushErr(w, http.StatusBadRequest, "validation",
				"user 필수 (대상 logonID, ws connect 시 X-WTG-User 와 동일)")
			return
		}

		// source 결정 — body 명시 → 명시 우선. 빈값이면 broker 우선 (legacy 호환).
		source := strings.ToLower(strings.TrimSpace(req.Source))
		if source == "" {
			if deps.BrokerClient != nil {
				source = "broker"
			} else {
				source = "http"
			}
		}

		// 페이로드 결정 — JSON 이면 그대로, 비면 default.
		payload := []byte(req.Data)
		if len(payload) == 0 {
			payload = []byte(`"hello-from-admin-push-test"`)
		}

		switch source {
		case "broker":
			if deps.BrokerClient == nil {
				writePushErr(w, http.StatusServiceUnavailable, "broker_disconnected",
					"broker source 비활성 (NoBroker 모드 또는 connection 끊김)")
				return
			}
			// Broadcast prefix 80 byte 작성.
			var hdr mymq.BroadcastHeader
			copy(hdr.LogonID[:], req.User)
			hdr.Function = byte(mymq.FCPush)
			hdr.SubFunction = byte(mymq.SubPush)
			body := make([]byte, mymq.BroadcastPrefixSize+len(payload))
			mymq.EncodeBroadcastHeader(body[:mymq.BroadcastPrefixSize], &hdr)
			copy(body[mymq.BroadcastPrefixSize:], payload)
			in := &mymq.FrameInput{
				Func: mymq.FCPush, Subc: mymq.SubPush, Body: body,
			}
			if err := deps.BrokerClient.Send(in); err != nil {
				deps.Logger.Warn("push-test broker send 실패",
					slog.String("user", req.User), slog.Any("err", err))
				writePushErr(w, http.StatusBadGateway, "send_failed", err.Error())
				return
			}
			deps.Logger.Info("push-test 발사",
				slog.String("source", "broker"),
				slog.String("user", req.User),
				slog.Int("body_size", len(body)))
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(pushTestResponse{
				Sent: true, Source: "broker",
				Func: byte(mymq.FCPush), Subc: byte(mymq.SubPush),
				BodySize: len(body), TargetUID: req.User,
			})

		case "http":
			if deps.HTTPClient == nil {
				writePushErr(w, http.StatusServiceUnavailable, "http_disabled",
					"HTTP push source 비활성 — --push-url 설정 필요")
				return
			}
			res, err := deps.HTTPClient.Push(r.Context(), pushsdk.Message{
				User: req.User,
				Data: payload,
			})
			if err != nil {
				deps.Logger.Warn("push-test http send 실패",
					slog.String("user", req.User), slog.Any("err", err))
				writePushErr(w, http.StatusBadGateway, "send_failed", err.Error())
				return
			}
			deps.Logger.Info("push-test 발사",
				slog.String("source", "http"),
				slog.String("user", req.User),
				slog.Int("body_size", res.BodySize))
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(pushTestResponse{
				Sent: res.Injected, Source: "http",
				Func: res.Func, Subc: res.Subc,
				BodySize: res.BodySize, TargetUID: req.User,
			})

		default:
			writePushErr(w, http.StatusBadRequest, "bad_source",
				"source 는 'broker' 또는 'http' 만 가능. 받은 값: "+source)
		}
	}
}

func writePushErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":   code,
		"message": msg,
	})
}

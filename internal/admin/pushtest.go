package admin

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/winwaysystems/wtg/pkg/mymq"
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
}

// pushTestResponse — 발사 결과.
type pushTestResponse struct {
	Sent      bool   `json:"sent"`
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
func PushTestHandler(client *mymq.Client, logger *slog.Logger) http.HandlerFunc {
	if client == nil {
		return func(w http.ResponseWriter, r *http.Request) {
			writePushErr(w, http.StatusServiceUnavailable, "broker_disconnected",
				"mci-admin 이 NoBroker 모드 — push 발사 불가")
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

		// 페이로드 결정 — JSON 이면 그대로, 비면 default.
		payload := []byte(req.Data)
		if len(payload) == 0 {
			payload = []byte(`"hello-from-admin-push-test"`)
		}

		// Broadcast prefix 80 byte 작성.
		var hdr mymq.BroadcastHeader
		copy(hdr.LogonID[:], req.User) // 16 byte 까지만 자동 truncate
		hdr.Function = byte(mymq.FCPush)
		hdr.SubFunction = byte(mymq.SubPush)

		body := make([]byte, mymq.BroadcastPrefixSize+len(payload))
		mymq.EncodeBroadcastHeader(body[:mymq.BroadcastPrefixSize], &hdr)
		copy(body[mymq.BroadcastPrefixSize:], payload)

		in := &mymq.FrameInput{
			Func: mymq.FCPush,
			Subc: mymq.SubPush,
			Body: body,
		}
		if err := client.Send(in); err != nil {
			logger.Warn("push-test send 실패",
				slog.String("user", req.User),
				slog.Any("err", err),
			)
			writePushErr(w, http.StatusBadGateway, "send_failed", err.Error())
			return
		}
		logger.Info("push-test 발사",
			slog.String("user", req.User),
			slog.Int("body_size", len(body)),
		)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(pushTestResponse{
			Sent:      true,
			Func:      byte(mymq.FCPush),
			Subc:      byte(mymq.SubPush),
			BodySize:  len(body),
			TargetUID: req.User,
		})
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

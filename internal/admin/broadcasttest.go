package admin

import (
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/winwaysystems/wtg/pkg/mymq"
)

// broadcastTestRequest — POST body.
//
// exchange : 발사 대상 exchange (필수). FC_CAST/SubBroadcast 시 broker 가
//
//	exchange 매칭된 모든 subscriber 에 fan-out.
//
// routing_key : 선택. 비면 broker 가 모든 routing_key 에 매칭 (fan-out 폭 큼).
// chan        : 선택. broadcast prefix 의 Chan 필드 (정책 분기 hint).
// data        : 페이로드. JSON 그대로 또는 string 으로 broadcast prefix 80B 뒤에 붙음.
//
//	비면 default 메시지.
type broadcastTestRequest struct {
	Exchange   string          `json:"exchange"`
	RoutingKey string          `json:"routing_key,omitempty"`
	Chan       string          `json:"chan,omitempty"`
	Data       json.RawMessage `json:"data,omitempty"`
}

// broadcastTestResponse — 발사 결과.
type broadcastTestResponse struct {
	Sent       bool   `json:"sent"`
	Func       uint8  `json:"func"`
	Subc       uint8  `json:"subc"`
	BodySize   int    `json:"body_size"`
	BodyHex    string `json:"body_hex,omitempty"` // prefix 80B + payload — 디버깅용
	Exchange   string `json:"exchange"`
	RoutingKey string `json:"routing_key,omitempty"`
}

// BroadcastTestHandler — POST /v1/admin/broadcast-test.
//
// mci-admin 의 broker connection 으로 FC_CAST/SubBroadcast 를 발사한다.
// broker 의 publish_packet 가 exchange 매칭된 모든 subscriber (mci-push /
// mci-price / mci-edge-* / 운영 svc) 에게 fan-out 한다. PushTestHandler 와
// 짝 — Push 는 user-targeted, Broadcast 는 exchange-targeted (all).
//
// 흐름:
//  1. body 의 exchange/routing_key/chan → broadcast prefix 80B 채움
//  2. data → 그대로 페이로드 (JSON or raw)
//  3. FrameInput.Func=FCCast / Subc=SubBroadcast / Xchg=exchange /
//     Rkey=routing_key / Body=[prefix80B + payload]
//  4. mci-admin 의 broker connection.Send (단방향)
//
// 정책 검사는 적용 X (admin 직접 발사 — DevMode 검증용). NoBroker 모드 시 503.
//
// 운영 안전: 이 endpoint 는 broker 의 모든 subscriber 에 도달하는 메시지를
// 발사하므로 운영 mci-admin 에서는 비활성 권장 (현재 조건 = client != nil).
func BroadcastTestHandler(client *mymq.Client, logger *slog.Logger) http.HandlerFunc {
	if client == nil {
		return func(w http.ResponseWriter, r *http.Request) {
			writeBcastErr(w, http.StatusServiceUnavailable, "broker_disconnected",
				"mci-admin 이 NoBroker 모드 — broadcast 발사 불가")
		}
	}
	return func(w http.ResponseWriter, r *http.Request) {
		var req broadcastTestRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeBcastErr(w, http.StatusBadRequest, "bad_json", err.Error())
			return
		}
		if req.Exchange == "" {
			writeBcastErr(w, http.StatusBadRequest, "validation",
				"exchange 필수 — broker 의 fan-out 대상")
			return
		}

		// 페이로드 결정 — JSON 이면 그대로, 비면 default.
		payload := []byte(req.Data)
		if len(payload) == 0 {
			payload = []byte(`"hello-from-admin-broadcast-test"`)
		}

		// Broadcast prefix 80 byte 작성 — Exchange/RoutingKey 정보가 prefix 에 채움.
		// FC_CAST 는 user-targeted 가 아니므로 LogonID 비움.
		var hdr mymq.BroadcastHeader
		copy(hdr.Exchange[:], req.Exchange)
		copy(hdr.Chan[:], req.Chan)
		hdr.Function = byte(mymq.FCCast)
		hdr.SubFunction = byte(mymq.SubBroadcast)

		body := make([]byte, mymq.BroadcastPrefixSize+len(payload))
		mymq.EncodeBroadcastHeader(body[:mymq.BroadcastPrefixSize], &hdr)
		copy(body[mymq.BroadcastPrefixSize:], payload)

		in := &mymq.FrameInput{
			Func: mymq.FCCast,
			Subc: mymq.SubBroadcast,
			Xchg: req.Exchange,
			Rkey: req.RoutingKey,
			Body: body,
		}
		if err := client.Send(in); err != nil {
			logger.Warn("broadcast-test send 실패",
				slog.String("exchange", req.Exchange),
				slog.String("rkey", req.RoutingKey),
				slog.Any("err", err),
			)
			writeBcastErr(w, http.StatusBadGateway, "send_failed", err.Error())
			return
		}
		logger.Info("broadcast-test 발사",
			slog.String("exchange", req.Exchange),
			slog.String("rkey", req.RoutingKey),
			slog.Int("body_size", len(body)),
		)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(broadcastTestResponse{
			Sent:       true,
			Func:       byte(mymq.FCCast),
			Subc:       byte(mymq.SubBroadcast),
			BodySize:   len(body),
			BodyHex:    hex.EncodeToString(body),
			Exchange:   req.Exchange,
			RoutingKey: req.RoutingKey,
		})
	}
}

func writeBcastErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":   code,
		"message": msg,
	})
}

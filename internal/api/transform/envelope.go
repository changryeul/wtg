// Package transform 은 mci-api 의 HTTP JSON envelope 처리 계층.
//
// transaction 별 핸들러를 만들지 않는다 (feedback_passthrough_pattern 참조).
// 모든 트랜잭션은 단일 generic envelope 으로 매매 엔진에 그대로 통과되고,
// 엔진의 응답이 클라이언트로 그대로 회신된다.
//
// data 필드는 WTG 입장에서 불투명한 페이로드이다. 매매 엔진이 정의한 스키마를
// 클라이언트가 직접 만들어 보내고, 엔진의 응답을 그대로 받아 사용한다.
package transform

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/winwaysystems/wtg/pkg/mymq"
	"github.com/winwaysystems/wtg/pkg/routing"
)

// Envelope 은 모든 transaction 요청/응답 공통 구조.
//
// 요청 측 필드 (클라이언트 → mci-api):
//
//	alias        : (옵션) 라우팅 룰 alias. 지정 시 exchange/routing_key 무시되고
//	               Registry 의 룰로 치환된다. 미지정/미등록이면 envelope 의
//	               exchange/routing_key 가 그대로 사용 (passthrough).
//	exchange     : MyMQ exchange (예: "ORDER", "ADMIN")
//	routing_key  : 라우팅 키 (= 매매 엔진의 transaction code, 예: "NEW", "CANCEL")
//	keyc         : 'S'/'P'/'N' (페이지네이션, 보통 생략)
//	pkey         : 이전 페이지 키 (옵션)
//	nkey         : 다음 페이지 키 (옵션)
//	data         : 엔진이 이해하는 페이로드 (raw JSON, WTG 미해석)
//
// 응답 측 필드 (mci-api → 클라이언트):
//
//	errn, errm   : 매매 엔진 에러 (errn=0 이면 정상)
//	data         : 엔진이 보낸 페이로드 raw JSON
//	pkey, nkey   : 엔진이 알려주는 페이지 키
type Envelope struct {
	Alias      string          `json:"alias,omitempty"`
	Exchange   string          `json:"exchange,omitempty"`
	RoutingKey string          `json:"routing_key,omitempty"`
	Keyc       string          `json:"keyc,omitempty"`
	Pkey       string          `json:"pkey,omitempty"`
	Nkey       string          `json:"nkey,omitempty"`
	Data       json.RawMessage `json:"data,omitempty"`

	// 응답 전용 필드.
	Errn uint32 `json:"errn,omitempty"`
	Errm string `json:"errm,omitempty"`
}

// ValidateRequest 는 transport-level 정합성만 검증한다.
// 비즈니스 검증은 매매 엔진에 위임 — 여기서는 routing key 가 필수인지 정도.
//
// alias 가 지정되면 envelope 의 exchange/routing_key 는 어차피 Registry 룰로
// 치환되므로 길이 검증을 생략한다 (raw 값은 무시되는 잔여 필드).
func (e *Envelope) ValidateRequest() error {
	if e.Alias == "" && e.RoutingKey == "" {
		return errors.New("alias 또는 routing_key 필요")
	}
	if len(e.Alias) > 64 {
		return errors.New("alias too long")
	}
	if e.Alias == "" {
		if len(e.RoutingKey) > mymq.LRkey {
			return errors.New("routing_key too long")
		}
		if len(e.Exchange) > mymq.LXchg {
			return errors.New("exchange too long")
		}
	}
	if len(e.Pkey) > mymq.LSkey || len(e.Nkey) > mymq.LSkey {
		return errors.New("pkey/nkey too long")
	}
	return nil
}

// keycValue 는 envelope 의 keyc 필드를 mymq.Keyc 로 변환한다.
// 비어있으면 KeySend (가장 흔한 값).
func (e *Envelope) keycValue() mymq.Keyc {
	if len(e.Keyc) == 0 {
		return mymq.KeySend
	}
	switch e.Keyc[0] {
	case 'S', 's':
		return mymq.KeySend
	case 'P', 'p':
		return mymq.KeyPrev
	case 'N', 'n':
		return mymq.KeyNext
	}
	return mymq.KeySend
}

// ErrUnknownAlias 는 alias 가 지정되었지만 Registry 에 활성 룰이 없을 때.
var ErrUnknownAlias = errors.New("alias 미등록 또는 비활성")

// BuildFrame 은 envelope 를 mymq.FrameInput 으로 변환한다.
// data 페이로드는 raw JSON bytes 그대로 frame body 가 된다.
//
// reg 가 nil 이 아니고 envelope 에 Alias 가 있으면 Registry 에서 룰을 찾아
// exchange/routing_key 를 치환한다. alias 가 명시되었는데 미등록/비활성이면
// ErrUnknownAlias — 클라이언트가 명시한 alias 는 신뢰할 수 없으면 거부 (보수적).
//
// usid 는 cookie_t 복원에 사용될 사용자 ID (Principal.Cookie 가 별도 경로로
// 첨부되므로 여기서는 사용 안 함).
func (e *Envelope) BuildFrame(ckey uint32, usid, traceIDHex string, reg routing.Registry) (*mymq.FrameInput, error) {
	if err := e.ValidateRequest(); err != nil {
		return nil, err
	}
	exchange := e.Exchange
	rkey := e.RoutingKey
	if e.Alias != "" {
		rule, err := routing.Resolve(reg, e.Alias)
		if err != nil {
			return nil, fmt.Errorf("%w: %q", ErrUnknownAlias, e.Alias)
		}
		exchange = rule.Exchange
		rkey = rule.RoutingKey
	}
	body := DataBytes(e.Data)
	in := &mymq.FrameInput{
		Func:    mymq.FCTran,
		Subc:    mymq.SubTranMsg,
		Dirf:    mymq.DirForward,
		Keyc:    e.keycValue(),
		Xchg:    exchange,
		Rkey:    rkey,
		Ckey:    ckey,
		Body:    body,
		TraceID: mymq.TraceIDFromHex(traceIDHex),
	}
	if e.Pkey != "" {
		in.Pkey = []byte(e.Pkey)
	}
	if e.Nkey != "" {
		in.Nkey = []byte(e.Nkey)
	}
	_ = usid
	return in, nil
}

// FromReply 는 mymq.Reply 를 응답 envelope 로 매핑한다.
// data 는 reply body 를 그대로 raw JSON 으로 담는다 — WTG 가 해석하지 않는다.
//
// 매매 엔진이 JSON 이외의 포맷(예: FIX-style 바이너리)으로 응답하는 경우엔
// data 가 base64 string 으로 가도록 추후 옵션을 추가할 수 있다 (현재 가정:
// 엔진이 JSON 을 보낸다).
func FromReply(reply *mymq.Reply) *Envelope {
	if reply == nil {
		return &Envelope{}
	}
	env := &Envelope{
		Errn: reply.Errn,
		Errm: reply.ErrMsg,
	}
	if len(reply.Body) > 0 {
		// Body 가 valid JSON 이면 그대로, 아니면 string 으로 감싸서 노출.
		if json.Valid(reply.Body) {
			env.Data = json.RawMessage(reply.Body)
		} else {
			b, _ := json.Marshal(string(reply.Body))
			env.Data = b
		}
	}
	return env
}

// DataBytes 는 envelope 의 data 필드를 frame body bytes 로 변환한다.
//
//   - JSON 문자열 (`"..."`) → 문자열 내용 그대로 (따옴표/이스케이프 해제).
//     고정폭 전문 (COMHDR+struct) 을 문자열로 실어 보내는 경로 — 따옴표가
//     body 에 섞이면 전 필드가 1바이트씩 밀린다.
//   - JSON object / array / 숫자 등 → raw JSON bytes 그대로 (JSON 엔진용).
//   - 비어있으면 빈 body.
func DataBytes(data json.RawMessage) []byte {
	if len(data) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		return []byte(s)
	}
	return []byte(data)
}

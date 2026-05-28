package quote

import (
	"encoding/binary"
	"encoding/json"
	"errors"
)

// pushdata wire 레이아웃 (mymq.h):
//
//	struct pushdata {
//	    long      mkid;          // 8 bytes (Linux x86_64) — market id
//	    pushmsg_t pushmsg;       // 1544 bytes
//	};
//	struct pushmsg {
//	    uint32_t  seqn;          // 4
//	    char      symb[20];      // 20
//	    uint32_t  mask;          // 4
//	    uint8_t   type;          // 1
//	    uint8_t   flag;          // 1
//	    uint16_t  msgl;          // 2
//	    char      msgb[1512];    // 1512
//	};
//
// 합계: 8 + 1544 = 1552 bytes. 정수 필드는 빅엔디안.
//
// cooker (C) 와 quote-forwarder (Go) 가 동일하게 사용하는 producer 측 wire
// spec — broker 는 이 raw bytes 를 그대로 subscriber 의 mymq.Unsolicited.Body
// 로 전달한다 (broadcast prefix 80B 는 별도로 떼어내짐). mci-price 의
// DecodePushData 가 역방향 파서.
const (
	LSymb        = 20
	MaxPushLen   = 1512
	PushmsgSize  = 4 + LSymb + 4 + 1 + 1 + 2 + MaxPushLen // 1544
	PushdataSize = 8 + PushmsgSize                        // 1552
)

// 인코딩 에러.
var (
	ErrPushdataPayloadTooLong = errors.New("quote: pushdata payload 가 maxPushLen 초과")
)

// PushdataOptions 는 EncodePushdata 의 헤더 필드 셋팅을 제어한다. 모두 옵션.
// Symbol 만은 mci-price 의 SymbolMap lookup 키이므로 사실상 필수.
type PushdataOptions struct {
	MarketID uint64 // pushdata.mkid
	SeqNum   uint32 // pushmsg.seqn
	Symbol   string // pushmsg.symb — 외부 심볼 (NUL pad, LSymb 까지 truncate)
	Mask     uint32 // pushmsg.mask
	Type     uint8  // pushmsg.type
	Flag     uint8  // pushmsg.flag
}

// EncodePushdata 는 raw payload (보통 v1 JSON envelope) 를 pushdata wire
// bytes 로 감싼다. 반환된 bytes 는 항상 PushdataSize (1552) 고정 길이로,
// msgb 의 사용되지 않은 영역은 NUL padding.
//
// payload 길이가 MaxPushLen 을 넘으면 ErrPushdataPayloadTooLong.
func EncodePushdata(opts PushdataOptions, payload []byte) ([]byte, error) {
	if len(payload) > MaxPushLen {
		return nil, ErrPushdataPayloadTooLong
	}
	buf := make([]byte, PushdataSize)
	off := 0

	binary.BigEndian.PutUint64(buf[off:off+8], opts.MarketID)
	off += 8

	binary.BigEndian.PutUint32(buf[off:off+4], opts.SeqNum)
	off += 4

	// symbol — NUL pad. truncate to LSymb if 더 길면.
	sym := opts.Symbol
	if len(sym) > LSymb {
		sym = sym[:LSymb]
	}
	copy(buf[off:off+LSymb], sym)
	off += LSymb

	binary.BigEndian.PutUint32(buf[off:off+4], opts.Mask)
	off += 4

	buf[off] = opts.Type
	off++
	buf[off] = opts.Flag
	off++

	binary.BigEndian.PutUint16(buf[off:off+2], uint16(len(payload)))
	off += 2

	copy(buf[off:off+len(payload)], payload)
	return buf, nil
}

// EncodePushdataBatch — 다수 v1 envelope 를 JSON 배열로 직렬화해서 pushdata
// msgb 에 박는다. broker publish 1 회로 N tick 을 전달 (broker publisher
// thread 가 ceiling 인 환경에서 throughput 우회).
//
// pushmsg.symb 는 첫 envelope 의 Sym 으로 (정보 전용 — mci-price 는 per-
// envelope sym 으로 SymbolMap lookup). seqn 은 첫 envelope 의 Seq.
// payload (`[...]`) 가 MaxPushLen(1512B) 을 넘으면 ErrPushdataPayloadTooLong.
//
// 단일 envelope 의 경우 호출자가 EncodePushdataV1 직접 사용 권장 (단일 객체
// 형태가 cooker C wire 와 호환). 본 함수는 N ≥ 2 일 때 사용.
func EncodePushdataBatch(envs []JSONEnvelope) ([]byte, error) {
	if len(envs) == 0 {
		return nil, ErrEnvelopeEmpty
	}
	body, err := json.Marshal(envs)
	if err != nil {
		return nil, err
	}
	first := envs[0]
	return EncodePushdata(PushdataOptions{
		SeqNum: uint32(first.Seq),
		Symbol: first.Sym,
	}, body)
}

// EncodePushdataV1 는 v1 JSON envelope 을 직렬화 → pushdata 로 감싸는 편의 함수.
// pushmsg.symb 는 env.Sym 으로, seqn 은 env.Seq 로 자동 채움.
func EncodePushdataV1(env JSONEnvelope) ([]byte, error) {
	body, err := EncodeJSONEnvelope(env)
	if err != nil {
		return nil, err
	}
	return EncodePushdata(PushdataOptions{
		SeqNum: uint32(env.Seq),
		Symbol: env.Sym,
	}, body)
}

package price

import (
	"encoding/binary"
	"errors"
	"strings"
	"time"

	"github.com/winwaysystems/wtg/pkg/mymq"
	"github.com/winwaysystems/wtg/pkg/quote"
)

// pushdata wire 레이아웃 정의는 pkg/quote/pushdata.go 가 단일 진실. 호환성을
// 위해 producer (cooker/quote-forwarder) 와 consumer (mci-price) 가 모두 같은
// 상수를 사용한다. 아래는 가독성을 위한 로컬 alias — 변경하지 말 것.
const (
	pushmsgSize  = quote.PushmsgSize
	pushdataSize = quote.PushdataSize
	maxPushLen   = quote.MaxPushLen
	lSymb        = quote.LSymb
)

// Tick 은 mci-price 가 정규화한 시세 단위.
//
// pushdata.msgb 안에 들어있는 실제 시세 페이로드(bid/ask/size 등)는 cooker 의
// 포맷에 의존하므로 1차 prototype 에서는 raw bytes 로 보존만 한다. 운영팀
// 사양 합의 후 별도 codec 으로 파싱.
type Tick struct {
	MarketID uint64    // mkid
	Symbol   string    // pushmsg.symb (NUL-trim)
	SeqNum   uint32    // pushmsg.seqn
	Mask     uint32    // pushmsg.mask (event flags)
	Type     uint8     // pushmsg.type
	Flag     uint8     // pushmsg.flag
	Body     []byte    // pushmsg.msgb[:msgl] — cooker payload (raw)
	Received time.Time // mci-price 가 디코딩한 wallclock 시각
}

// 디코딩 에러.
var (
	ErrTooShortPushData = errors.New("price: pushdata 본문이 짧음")
)

// DecodePushData 는 broker 가 보낸 pushdata raw bytes 를 Tick 으로 파싱한다.
// raw bytes 는 broker broadcast prefix(80B) 가 이미 분리된 상태여야 한다
// (mymq.Unsolicited.Body 가 그 상태로 도달).
func DecodePushData(raw []byte) (*Tick, error) {
	if len(raw) < pushdataSize {
		return nil, ErrTooShortPushData
	}
	t := &Tick{Received: time.Now()}

	off := 0
	t.MarketID = binary.BigEndian.Uint64(raw[off : off+8])
	off += 8

	// pushmsg 부분
	t.SeqNum = binary.BigEndian.Uint32(raw[off : off+4])
	off += 4
	t.Symbol = trimNul(string(raw[off : off+lSymb]))
	off += lSymb
	t.Mask = binary.BigEndian.Uint32(raw[off : off+4])
	off += 4
	t.Type = raw[off]
	off++
	t.Flag = raw[off]
	off++
	msgl := binary.BigEndian.Uint16(raw[off : off+2])
	off += 2

	// msgb 의 유효 길이는 msgl. 1512 buffer 에서 msgl 만 잘라낸다.
	if int(msgl) > maxPushLen {
		msgl = maxPushLen
	}
	if off+int(msgl) > len(raw) {
		// raw 가 잘려있으면 가용 분만.
		msgl = uint16(len(raw) - off)
	}
	if msgl > 0 {
		t.Body = make([]byte, msgl)
		copy(t.Body, raw[off:off+int(msgl)])
	}
	return t, nil
}

// EncodePushData 는 Tick 을 wire 레이아웃 bytes 로 직렬화한다 (mock/테스트용).
//
// 실 운영에서는 cooker 가 직접 pushdata 구조를 채워 broker 로 publish 하므로
// 이 함수는 테스트 또는 mock broker 에서만 사용한다.
func EncodePushData(t *Tick) []byte {
	buf := make([]byte, pushdataSize)
	off := 0
	binary.BigEndian.PutUint64(buf[off:off+8], t.MarketID)
	off += 8
	binary.BigEndian.PutUint32(buf[off:off+4], t.SeqNum)
	off += 4
	copy(buf[off:off+lSymb], t.Symbol)
	off += lSymb
	binary.BigEndian.PutUint32(buf[off:off+4], t.Mask)
	off += 4
	buf[off] = t.Type
	off++
	buf[off] = t.Flag
	off++
	bodyLen := len(t.Body)
	if bodyLen > maxPushLen {
		bodyLen = maxPushLen
	}
	binary.BigEndian.PutUint16(buf[off:off+2], uint16(bodyLen))
	off += 2
	if bodyLen > 0 {
		copy(buf[off:off+bodyLen], t.Body)
	}
	return buf
}

// FromUnsolicited 는 mymq.Unsolicited 메시지에서 Tick 을 추출한다.
// PRICE exchange 필터링은 호출자 책임이며 이 함수는 단순히 Body 디코딩.
func FromUnsolicited(msg *mymq.Unsolicited) (*Tick, error) {
	if msg == nil {
		return nil, ErrTooShortPushData
	}
	return DecodePushData(msg.Body)
}

// trimNul 은 NUL byte 까지의 prefix 만 반환.
func trimNul(s string) string {
	if i := strings.IndexByte(s, 0); i >= 0 {
		return s[:i]
	}
	return s
}

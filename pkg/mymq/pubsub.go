package mymq

// Broadcast prefix (mqio.h 의 struct broadcast) — FC_CAST / FC_PUSH / FC_SIGNAL
// 의 본문 앞에 붙는 80바이트 헤더. 수신자의 큐 플래그에 QF_UNSOL_HDR 가
// 켜져 있을 때만 클라이언트에 노출된다.
//
//   ipaddr        char[24]   목적지 호스트 (비면 = 모두)
//   exchange      char[16]
//   chan          char[16]   채널 이름
//   user          char[16]
//   logon_id      char[16]   대상 사용자 (push 시 특정 사용자)
//   function      uint8      FC_CAST / FC_PUSH / FC_SIGNAL
//   sub_function  uint8      BROADCAST(50) / UNICAST / PUSH / KILL / EXIT
//   via_net       uint8      1 = 원격 출처
//   debug         uint8

const broadcastPrefixSize = 24 + 16 + 16 + 16 + 16 + 1 + 1 + 1 + 1

// BroadcastPrefixSize 는 broadcast prefix 가 차지하는 바이트 수 (80).
const BroadcastPrefixSize = broadcastPrefixSize

// BroadcastHeader 는 FC_CAST/FC_PUSH 본문 앞 80바이트 prefix 의 파싱 뷰.
type BroadcastHeader struct {
	IPAddr      [24]byte
	Exchange    [16]byte
	Chan        [16]byte
	User        [16]byte
	LogonID     [16]byte
	Function    uint8
	SubFunction uint8
	ViaNet      uint8
	Debug       uint8
}

// IPAddrString 은 IPAddr 필드를 NUL-trim 한 string 으로 반환.
func (h *BroadcastHeader) IPAddrString() string { return trimNulString(h.IPAddr[:]) }

// ExchangeString 은 Exchange 필드를 NUL-trim 한 string 으로 반환.
func (h *BroadcastHeader) ExchangeString() string { return trimNulString(h.Exchange[:]) }

// ChanString 은 Chan 필드를 NUL-trim 한 string 으로 반환.
func (h *BroadcastHeader) ChanString() string { return trimNulString(h.Chan[:]) }

// UserString 은 User 필드를 NUL-trim 한 string 으로 반환.
func (h *BroadcastHeader) UserString() string { return trimNulString(h.User[:]) }

// LogonIDString 은 LogonID 필드를 NUL-trim 한 string 으로 반환.
func (h *BroadcastHeader) LogonIDString() string { return trimNulString(h.LogonID[:]) }

// EncodeBroadcastHeader 는 80바이트 broadcast prefix 를 dst 에 기록한다.
func EncodeBroadcastHeader(dst []byte, h *BroadcastHeader) {
	_ = dst[broadcastPrefixSize-1]
	off := 0
	copy(dst[off:off+24], h.IPAddr[:])
	off += 24
	copy(dst[off:off+16], h.Exchange[:])
	off += 16
	copy(dst[off:off+16], h.Chan[:])
	off += 16
	copy(dst[off:off+16], h.User[:])
	off += 16
	copy(dst[off:off+16], h.LogonID[:])
	off += 16
	dst[off] = h.Function
	dst[off+1] = h.SubFunction
	dst[off+2] = h.ViaNet
	dst[off+3] = h.Debug
}

// DecodeBroadcastHeader 는 80바이트 broadcast prefix 를 파싱한다.
func DecodeBroadcastHeader(src []byte) (BroadcastHeader, error) {
	var h BroadcastHeader
	if len(src) < broadcastPrefixSize {
		return h, ErrFrameTooShort
	}
	off := 0
	copy(h.IPAddr[:], src[off:off+24])
	off += 24
	copy(h.Exchange[:], src[off:off+16])
	off += 16
	copy(h.Chan[:], src[off:off+16])
	off += 16
	copy(h.User[:], src[off:off+16])
	off += 16
	copy(h.LogonID[:], src[off:off+16])
	off += 16
	h.Function = src[off]
	h.SubFunction = src[off+1]
	h.ViaNet = src[off+2]
	h.Debug = src[off+3]
	return h, nil
}

// SplitBroadcast 는 80바이트 broadcast prefix 를 본문에서 분리한다.
// 파싱된 헤더와 페이로드 슬라이스(입력 버퍼와 메모리 공유)를 반환한다.
// 입력이 prefix 보다 짧으면 nil 헤더 + 입력 그대로 반환 — 호출자는 prefix
// 가 없는 unsolicited 메시지로 처리한다.
func SplitBroadcast(body []byte) (*BroadcastHeader, []byte, error) {
	if len(body) < broadcastPrefixSize {
		return nil, body, nil
	}
	h, err := DecodeBroadcastHeader(body[:broadcastPrefixSize])
	if err != nil {
		return nil, body, err
	}
	return &h, body[broadcastPrefixSize:], nil
}

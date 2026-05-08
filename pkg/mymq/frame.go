package mymq

import (
	"errors"
	"fmt"
)

// 프레임 레이아웃 (mqhdr_t 고정 헤더 오프셋, mq.h 기준):
//
//   0..3   msgl       uint32 BE   전체 길이 (자기 포함)
//   4      func       uint8       function code
//   5      subc       uint8       sub-function
//   6      nvia       uint8       navi 엔트리 개수
//   7      dirf       uint8       방향
//   8      msgf       uint8       메시지 플래그
//   9      ctlf       uint8       제어 플래그
//   10     reserved   uint8
//   11     keyc       uint8       key action
//   12..19 xchg[8]                목적지 exchange
//   20..35 rkey[16]               목적지 routing key
//   36..39 ckey       uint32 BE   correlation_id 로 활용
//   40..43 clid       uint32 BE   client id
//   44..51 wkey[8]                window key (UI window id)
//   52..55 chan[4]                origin channel type
//   56..59 errn       uint32 BE   에러 코드
//   60..63 coff       WHERE       cookie 위치 (zipf:1 + doff:3)
//   64..67 soff       WHERE       symbol 위치
//   68..71 errm       SZOFF       에러 메시지 (len:2 + off:2)
//   72..75 pkey       SZOFF       이전 키
//   76..79 nkey       SZOFF       다음 키
//   80..83 body       WHERE       본문 위치

// 고정 헤더 안의 각 필드 오프셋.
const (
	offLength   = 0
	offFunc     = 4
	offSubc     = 5
	offNvia     = 6
	offDirf     = 7
	offMsgf     = 8
	offCtlf     = 9
	offReserved = 10
	offKeyc     = 11
	offXchg     = 12
	offRkey     = 20
	offCkey     = 36
	offClid     = 40
	offWkey     = 44
	offChan     = 52
	offErrn     = 56
	offCoff     = 60
	offSoff     = 64
	offErrm     = 68
	offPkey     = 72
	offNkey     = 76
	offBody     = 80
)

// 프레임 파싱 시 발생할 수 있는 에러.
var (
	ErrFrameTooShort = errors.New("mymq: 프레임이 고정 헤더보다 짧음")
	ErrFrameTooLong  = errors.New("mymq: 프레임이 MaxMsgSize 초과")
	ErrInvalidLength = errors.New("mymq: 헤더의 length 와 버퍼 길이 불일치")
	ErrInvalidNavi   = errors.New("mymq: 잘못된 navigation 엔트리 개수")
	ErrInvalidOffset = errors.New("mymq: 가변 영역 오프셋이 범위를 벗어남")
)

// EncodeHeader 는 84바이트 고정 헤더를 dst 에 기록한다.
// Length 필드는 h.Length 에서 가져온다. 일반적으로 호출자는 EncodeFrame 안에서
// 전체 프레임을 조립한 뒤 마지막에 Length 를 채운다.
func EncodeHeader(dst []byte, h *Header) error {
	if len(dst) < HdrSize {
		return ErrFrameTooShort
	}
	putU32(dst[offLength:offLength+4], h.Length)
	dst[offFunc] = byte(h.Func)
	dst[offSubc] = byte(h.Subc)
	dst[offNvia] = h.Nvia
	dst[offDirf] = byte(h.Dirf)
	dst[offMsgf] = h.Msgf
	dst[offCtlf] = h.Ctlf
	dst[offReserved] = 0
	dst[offKeyc] = byte(h.Keyc)
	copy(dst[offXchg:offXchg+LXchg], h.Xchg[:])
	copy(dst[offRkey:offRkey+LRkey], h.Rkey[:])
	putU32(dst[offCkey:offCkey+4], h.Ckey)
	putU32(dst[offClid:offClid+4], h.Clid)
	copy(dst[offWkey:offWkey+8], h.Wkey[:])
	copy(dst[offChan:offChan+4], h.Chan[:])
	putU32(dst[offErrn:offErrn+4], h.Errn)
	encodeWhere(dst[offCoff:offCoff+4], h.CoffZipf, h.CoffOff)
	encodeWhere(dst[offSoff:offSoff+4], h.SoffZipf, h.SoffOff)
	encodeSzoff(dst[offErrm:offErrm+4], h.ErrmLen, h.ErrmOff)
	encodeSzoff(dst[offPkey:offPkey+4], h.PkeyLen, h.PkeyOff)
	encodeSzoff(dst[offNkey:offNkey+4], h.NkeyLen, h.NkeyOff)
	encodeWhere(dst[offBody:offBody+4], h.BodyZipf, h.BodyOff)
	return nil
}

// DecodeHeader 는 src 로부터 84바이트 고정 헤더를 파싱한다.
func DecodeHeader(src []byte) (Header, error) {
	var h Header
	if len(src) < HdrSize {
		return h, ErrFrameTooShort
	}
	h.Length = getU32(src[offLength : offLength+4])
	h.Func = Func(src[offFunc])
	h.Subc = Subc(src[offSubc])
	h.Nvia = src[offNvia]
	h.Dirf = Dirf(src[offDirf])
	h.Msgf = src[offMsgf]
	h.Ctlf = src[offCtlf]
	h.Keyc = Keyc(src[offKeyc])
	copy(h.Xchg[:], src[offXchg:offXchg+LXchg])
	copy(h.Rkey[:], src[offRkey:offRkey+LRkey])
	h.Ckey = getU32(src[offCkey : offCkey+4])
	h.Clid = getU32(src[offClid : offClid+4])
	copy(h.Wkey[:], src[offWkey:offWkey+8])
	copy(h.Chan[:], src[offChan:offChan+4])
	h.Errn = getU32(src[offErrn : offErrn+4])
	h.CoffZipf, h.CoffOff = decodeWhere(src[offCoff : offCoff+4])
	h.SoffZipf, h.SoffOff = decodeWhere(src[offSoff : offSoff+4])
	h.ErrmLen, h.ErrmOff = decodeSzoff(src[offErrm : offErrm+4])
	h.PkeyLen, h.PkeyOff = decodeSzoff(src[offPkey : offPkey+4])
	h.NkeyLen, h.NkeyOff = decodeSzoff(src[offNkey : offNkey+4])
	h.BodyZipf, h.BodyOff = decodeWhere(src[offBody : offBody+4])
	return h, nil
}

// FrameInput 은 송신할 프레임의 논리적 내용을 표현한다.
// 호출자가 이 구조체를 채우면 EncodeFrame 이 wire 바이트로 변환한다.
type FrameInput struct {
	Func Func
	Subc Subc
	Dirf Dirf
	Keyc Keyc
	Msgf uint8 // 부가 플래그 (MsgfNwc/MsgfHdr/...). MsgfErr 는 자동 설정.
	Ctlf uint8

	Xchg string
	Rkey string
	Ckey uint32
	Clid uint32
	Wkey [8]byte
	Chan [4]byte

	Navis []Navi // navigation 엔트리 (최대 MaxVia 개)

	Errn   uint32
	ErrMsg string // 비어있지 않으면 MsgfErr 가 자동으로 켜지고 ERRM 영역에 인코딩됨

	Pkey []byte // 최대 LSkey 길이
	Nkey []byte // 최대 LSkey 길이

	Cookie *Cookie // non-nil 이면 COOKIE 영역 동봉 (압축 없음)
	// (SYMB 영역은 송신 미지원 — 수신만 지원)

	Body []byte // 본문 페이로드 (현재는 raw, 압축 미적용)
}

// EncodeFrame 은 FrameInput 으로부터 완전한 wire 프레임을 만든다.
// 반환되는 슬라이스는 4바이트 length prefix 를 포함한 전체 프레임이다.
func EncodeFrame(in *FrameInput) ([]byte, error) {
	if len(in.Navis) > MaxVia {
		return nil, ErrInvalidNavi
	}
	if len(in.Pkey) > LSkey || len(in.Nkey) > LSkey {
		return nil, fmt.Errorf("mymq: pkey/nkey exceed L_SKEY (%d)", LSkey)
	}

	// Compute total size up front.
	size := HdrSize
	size += NaviSize * len(in.Navis)

	errmLen := 0
	if in.ErrMsg != "" {
		errmLen = len(in.ErrMsg)
		size += errmLen
	}
	pkeyLen := len(in.Pkey)
	if pkeyLen > 0 {
		size += pkeyLen
	}
	nkeyLen := len(in.Nkey)
	if nkeyLen > 0 {
		size += nkeyLen
	}
	if in.Cookie != nil {
		size += CookieWire
	}
	bodyLen := len(in.Body)
	size += bodyLen

	if size > MaxMsgSize {
		return nil, ErrFrameTooLong
	}

	buf := make([]byte, size)

	// 헤더 빌드 — Length 필드는 가변 영역 다 채운 후 마지막에 설정.
	var hdr Header
	hdr.Func = in.Func
	hdr.Subc = in.Subc
	hdr.Dirf = in.Dirf
	hdr.Msgf = in.Msgf
	hdr.Ctlf = in.Ctlf
	hdr.Keyc = in.Keyc
	padCopyString(hdr.Xchg[:], in.Xchg)
	padCopyString(hdr.Rkey[:], in.Rkey)
	hdr.Ckey = in.Ckey
	hdr.Clid = in.Clid
	hdr.Wkey = in.Wkey
	hdr.Chan = in.Chan
	hdr.Errn = in.Errn
	hdr.Nvia = uint8(len(in.Navis))

	// 가변 영역 커서 (고정 헤더 + navi 배열 다음부터 시작).
	cur := HdrSize + NaviSize*len(in.Navis)

	// ERRM
	if errmLen > 0 {
		hdr.Msgf |= MsgfErr
		hdr.ErrmLen = uint16(errmLen)
		hdr.ErrmOff = uint16(cur)
		copy(buf[cur:cur+errmLen], in.ErrMsg)
		cur += errmLen
	}

	// PKEY / NKEY
	if pkeyLen > 0 {
		hdr.PkeyLen = uint16(pkeyLen)
		hdr.PkeyOff = uint16(cur)
		copy(buf[cur:cur+pkeyLen], in.Pkey)
		cur += pkeyLen
	}
	if nkeyLen > 0 {
		hdr.NkeyLen = uint16(nkeyLen)
		hdr.NkeyOff = uint16(cur)
		copy(buf[cur:cur+nkeyLen], in.Nkey)
		cur += nkeyLen
	}

	// COOKIE (압축 없이 평문으로만)
	if in.Cookie != nil {
		hdr.CoffZipf = 0
		hdr.CoffOff = uint32(cur)
		EncodeCookie(buf[cur:cur+CookieWire], in.Cookie)
		cur += CookieWire
	}

	// 본문 (Phase 1 에서는 raw, 압축 미적용)
	if bodyLen > 0 {
		hdr.BodyZipf = 0
		hdr.BodyOff = uint32(cur)
		copy(buf[cur:cur+bodyLen], in.Body)
		cur += bodyLen
	}

	hdr.Length = uint32(cur)
	if err := EncodeHeader(buf[:HdrSize], &hdr); err != nil {
		return nil, err
	}

	// 헤더 직후에 navi 배열 인코딩.
	naviOff := HdrSize
	for i := range in.Navis {
		EncodeNavi(buf[naviOff:naviOff+NaviSize], &in.Navis[i])
		naviOff += NaviSize
	}

	return buf[:cur], nil
}

// DecodedFrame 은 수신된 프레임의 파싱 뷰.
type DecodedFrame struct {
	Header   Header
	Navis    []Navi
	ErrMsg   string // Header.Msgf 에 MsgfErr 가 켜져 있을 때만 채워짐
	Pkey     []byte
	Nkey     []byte
	Cookie   *Cookie // CoffOff > 0 이고 압축이 아닐 때만 (압축 미지원 단계)
	Body     []byte  // 본문 raw 바이트 (BodyZipf != 0 이면 압축 슬라이스)
	BodyZipf Zipf    // 0 = raw, 그 외에는 호출자가 직접 압축 해제
}

// DecodeFrame 은 완전한 wire 프레임을 파싱한다.
// frame 슬라이스는 length prefix 를 포함한 전체이며, frame[0:4] 의 length 필드가
// frame 전체 길이와 일치해야 한다.
//
// 반환되는 DecodedFrame 의 슬라이스 필드들은 입력 frame 과 메모리를 공유한다.
// 입력 버퍼의 생명주기 밖으로 살아남아야 하면 호출자가 복사해야 한다.
func DecodeFrame(frame []byte) (*DecodedFrame, error) {
	if len(frame) < HdrSize {
		return nil, ErrFrameTooShort
	}
	hdr, err := DecodeHeader(frame[:HdrSize])
	if err != nil {
		return nil, err
	}
	if int(hdr.Length) != len(frame) {
		return nil, fmt.Errorf("%w: header=%d buffer=%d", ErrInvalidLength, hdr.Length, len(frame))
	}

	df := &DecodedFrame{Header: hdr, BodyZipf: Zipf(hdr.BodyZipf)}

	// Navis
	if hdr.Nvia > 0 {
		if int(hdr.Nvia) > MaxVia {
			return nil, ErrInvalidNavi
		}
		need := HdrSize + NaviSize*int(hdr.Nvia)
		if need > len(frame) {
			return nil, ErrInvalidOffset
		}
		df.Navis = make([]Navi, hdr.Nvia)
		off := HdrSize
		for i := uint8(0); i < hdr.Nvia; i++ {
			df.Navis[i] = DecodeNavi(frame[off : off+NaviSize])
			off += NaviSize
		}
	}

	// ERRM
	if hdr.ErrmLen > 0 && hdr.ErrmOff > 0 {
		end := int(hdr.ErrmOff) + int(hdr.ErrmLen)
		if end > len(frame) {
			return nil, ErrInvalidOffset
		}
		df.ErrMsg = string(frame[hdr.ErrmOff:end])
	}

	// PKEY / NKEY
	if hdr.PkeyLen > 0 && hdr.PkeyOff > 0 {
		end := int(hdr.PkeyOff) + int(hdr.PkeyLen)
		if end > len(frame) {
			return nil, ErrInvalidOffset
		}
		df.Pkey = frame[hdr.PkeyOff:end]
	}
	if hdr.NkeyLen > 0 && hdr.NkeyOff > 0 {
		end := int(hdr.NkeyOff) + int(hdr.NkeyLen)
		if end > len(frame) {
			return nil, ErrInvalidOffset
		}
		df.Nkey = frame[hdr.NkeyOff:end]
	}

	// COOKIE (현재는 압축 안 된 것만 파싱)
	if hdr.CoffOff > 0 {
		if hdr.CoffZipf != 0 {
			// 압축된 쿠키 — Phase 1 에서는 패스. 추후 호출자가 직접 압축 해제.
		} else {
			end := int(hdr.CoffOff) + CookieWire
			if end > len(frame) {
				return nil, ErrInvalidOffset
			}
			c := DecodeCookie(frame[hdr.CoffOff:end])
			df.Cookie = &c
		}
	}

	// 본문 (raw 슬라이스 반환; BodyZipf != 0 이면 호출자가 압축 해제)
	if hdr.BodyOff > 0 {
		if int(hdr.BodyOff) > len(frame) {
			return nil, ErrInvalidOffset
		}
		df.Body = frame[hdr.BodyOff:]
	}

	return df, nil
}

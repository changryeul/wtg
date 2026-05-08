package mymq

import "encoding/binary"

// MyMQ wire 필드는 모두 big-endian (network byte order) 이다.
// 이 파일의 헬퍼들은 src/inc/mq.h 의 매크로에 대응한다:
//
//   CHAR2INT / INT2CHAR     - 4바이트 uint32 BE
//   CHAR2SHORT / SHORT2CHAR - 2바이트 uint16 BE
//   CHAR2OFF / OFF2CHAR     - 3바이트 uint24 BE (WHERE.doff 에 사용)

// putU32 는 uint32 를 big-endian 으로 기록한다.
func putU32(b []byte, v uint32) {
	binary.BigEndian.PutUint32(b, v)
}

// getU32 는 big-endian uint32 를 읽는다.
func getU32(b []byte) uint32 {
	return binary.BigEndian.Uint32(b)
}

// putU16 는 uint16 을 big-endian 으로 기록한다.
func putU16(b []byte, v uint16) {
	binary.BigEndian.PutUint16(b, v)
}

// getU16 은 big-endian uint16 을 읽는다.
func getU16(b []byte) uint16 {
	return binary.BigEndian.Uint16(b)
}

// putU24 는 24비트 unsigned 정수를 big-endian 3바이트로 기록한다.
// WHERE 구조의 doff 필드(데이터 오프셋)에 사용된다.
func putU24(b []byte, v uint32) {
	b[0] = byte(v >> 16)
	b[1] = byte(v >> 8)
	b[2] = byte(v)
}

// getU24 는 big-endian 24비트 unsigned 정수를 읽는다.
func getU24(b []byte) uint32 {
	return uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2])
}

// WHERE 는 mq.h 의 4바이트 인코딩 (zipf:1 + doff:3).
//
//	struct {
//	    uint8_t zipf;
//	    uint8_t doff[3];
//	} WHERE;
//
// doff 는 프레임 버퍼 안에서의 바이트 오프셋이다.
func encodeWhere(b []byte, zipf uint8, off uint32) {
	_ = b[3] // 경계 체크 (compiler 힌트)
	b[0] = zipf
	putU24(b[1:4], off)
}

func decodeWhere(b []byte) (zipf uint8, off uint32) {
	_ = b[3]
	zipf = b[0]
	off = getU24(b[1:4])
	return
}

// SZOFF 는 mq.h 의 4바이트 인코딩 (len:2 + off:2).
//
//	struct {
//	    uint8_t len[2];
//	    uint8_t off[2];
//	} SZOFF;
//
// 짧은 필드(에러메시지, pkey/nkey)의 위치를 표시한다.
func encodeSzoff(b []byte, length, off uint16) {
	_ = b[3]
	putU16(b[0:2], length)
	putU16(b[2:4], off)
}

func decodeSzoff(b []byte) (length, off uint16) {
	_ = b[3]
	length = getU16(b[0:2])
	off = getU16(b[2:4])
	return
}

// padCopyString 은 src 를 dst 로 복사하고 남는 부분을 0 으로 채운다.
// 고정 길이 문자열 필드(xchg/rkey 등)에 사용된다.
func padCopyString(dst []byte, src string) {
	n := copy(dst, src)
	for i := n; i < len(dst); i++ {
		dst[i] = 0
	}
}

// trimNul 은 b 에서 첫 NUL 바이트 직전까지의 prefix 를 반환한다.
// NUL 이 없으면 전체 슬라이스를 그대로 반환. 고정 길이 이름 필드 읽을 때 사용.
func trimNul(b []byte) []byte {
	for i, c := range b {
		if c == 0 {
			return b[:i]
		}
	}
	return b
}

// trimNulString 은 trimNul 의 Go string 반환 버전.
func trimNulString(b []byte) string {
	return string(trimNul(b))
}

// EncodeNavi 는 Navi 엔트리를 32바이트로 직렬화한다.
func EncodeNavi(dst []byte, n *Navi) {
	_ = dst[NaviSize-1]
	copy(dst[0:LXchg], n.Xchg[:])
	copy(dst[LXchg:LXchg+LRkey], n.Rkey[:])
	putU32(dst[LXchg+LRkey:LXchg+LRkey+4], n.Scid)
	dst[LXchg+LRkey+4] = n.Iama
	dst[LXchg+LRkey+5] = n.Eatt
	dst[LXchg+LRkey+6] = n.Zipf
	dst[LXchg+LRkey+7] = n.Ncid
}

// DecodeNavi 는 32바이트 Navi 엔트리를 파싱한다.
func DecodeNavi(src []byte) Navi {
	_ = src[NaviSize-1]
	var n Navi
	copy(n.Xchg[:], src[0:LXchg])
	copy(n.Rkey[:], src[LXchg:LXchg+LRkey])
	n.Scid = getU32(src[LXchg+LRkey : LXchg+LRkey+4])
	n.Iama = src[LXchg+LRkey+4]
	n.Eatt = src[LXchg+LRkey+5]
	n.Zipf = src[LXchg+LRkey+6]
	n.Ncid = src[LXchg+LRkey+7]
	return n
}

// EncodeCookie 는 cookie_t 를 압축되지 않은 wire 레이아웃으로 직렬화한다.
// 레이아웃: usid[16] + name[12] + maca[24] + pcip[20] + svip[20] + clid[4 BE] + coki[256] = 352 bytes
func EncodeCookie(dst []byte, c *Cookie) {
	_ = dst[CookieWire-1]
	off := 0
	copy(dst[off:off+16], c.Usid[:])
	off += 16
	copy(dst[off:off+12], c.Name[:])
	off += 12
	copy(dst[off:off+24], c.Maca[:])
	off += 24
	copy(dst[off:off+20], c.Pcip[:])
	off += 20
	copy(dst[off:off+20], c.Svip[:])
	off += 20
	putU32(dst[off:off+4], c.Clid)
	off += 4
	copy(dst[off:off+CookieSize], c.Coki[:])
}

// DecodeCookie 는 압축되지 않은 wire 레이아웃에서 cookie_t 를 파싱한다.
func DecodeCookie(src []byte) Cookie {
	_ = src[CookieWire-1]
	var c Cookie
	off := 0
	copy(c.Usid[:], src[off:off+16])
	off += 16
	copy(c.Name[:], src[off:off+12])
	off += 12
	copy(c.Maca[:], src[off:off+24])
	off += 24
	copy(c.Pcip[:], src[off:off+20])
	off += 20
	copy(c.Svip[:], src[off:off+20])
	off += 20
	c.Clid = getU32(src[off : off+4])
	off += 4
	copy(c.Coki[:], src[off:off+CookieSize])
	return c
}

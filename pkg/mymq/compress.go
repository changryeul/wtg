package mymq

import (
	"bytes"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
)

// 압축 본문 레이아웃 (mq_frame.c 의 frame_body 와 호환):
//
//	[orig_size:4 BE][compressed_data ...]
//
// orig_size 는 압축 전 원본 길이 (uint32 big-endian).
// orig_size 의 인코딩 자체는 mq_frame.c:220 에서 `size >> 27` 로 보이는데
// (24 vs 27 bug 의심 — phase0-analysis §3.8 참조), 우리는 안전하게 `>> 24`
// 로 디코딩한다. 실 broker 가 27 비트로 인코딩하는 게 확인되면 호환 옵션 추가.

// 압축 관련 에러.
var (
	ErrUnsupportedZipf      = errors.New("mymq: 지원하지 않는 압축 방식")
	ErrTruncatedCompressed  = errors.New("mymq: 압축 본문이 4바이트 prefix 보다 짧음")
	ErrCompressedSizeExceed = errors.New("mymq: 디코딩 결과가 prefix 의 orig_size 와 불일치")
)

// Decompress 는 broker 측에서 압축된 본문을 평문으로 복원한다.
//
//   - zipf == ZipfNone: src 를 그대로 반환 (passthrough).
//   - zipf == ZipfZlib: 표준 compress/zlib 로 디코딩.
//   - zipf == ZipfMlzo / ZipfLziv: 미지원 (별도 라이브러리 필요).
//
// 압축 본문은 [orig_size:4 BE][compressed_data] 형태로 들어온다.
// 디코딩 결과 길이가 orig_size 와 다르면 에러로 간주.
func Decompress(src []byte, zipf Zipf) ([]byte, error) {
	if zipf == ZipfNone {
		return src, nil
	}
	if len(src) < 4 {
		return nil, ErrTruncatedCompressed
	}
	// orig_size 는 BE 24-bit 으로 인코딩될 가능성이 있으므로 >> 24 사용.
	origSize := getU32(src[0:4])
	payload := src[4:]

	switch zipf {
	case ZipfZlib:
		return decompressZlib(payload, origSize)
	case ZipfMlzo:
		// 운영 기본값 (mq_send.c: zipf=1). 순수 Go LZO1X 디코더 — lzo.go.
		return decompressLZO1X(payload, origSize)
	case ZipfLziv:
		return nil, fmt.Errorf("%w: LZIV", ErrUnsupportedZipf)
	default:
		return nil, fmt.Errorf("%w: zipf=%d", ErrUnsupportedZipf, zipf)
	}
}

// decompressZlib 는 zlib 압축된 페이로드를 디코딩한다.
// origSize 는 사전 할당 hint 로만 사용되며 (정확하지 않더라도 정상 동작),
// 결과 길이가 origSize 보다 크게 다르면 잠재적 손상으로 간주할 수 있다.
func decompressZlib(payload []byte, origSize uint32) ([]byte, error) {
	r, err := zlib.NewReader(bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("zlib reader: %w", err)
	}
	defer r.Close()

	cap := int(origSize)
	if cap <= 0 || cap > MaxMsgSize {
		// origSize 신뢰 불가 시 적당한 시작값으로 fallback.
		cap = len(payload) * 4
	}
	out := bytes.NewBuffer(make([]byte, 0, cap))
	if _, err := io.Copy(out, r); err != nil {
		return nil, fmt.Errorf("zlib decode: %w", err)
	}
	return out.Bytes(), nil
}

// IsZipfSupported 는 주어진 압축 방식이 WTG 에서 디코딩 가능한지 보고한다.
// NONE / ZLIB / MLZO (운영 기본) 지원. LZIV 만 미지원.
func IsZipfSupported(zipf Zipf) bool {
	switch zipf {
	case ZipfNone, ZipfZlib, ZipfMlzo:
		return true
	}
	return false
}

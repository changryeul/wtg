package mymq

import (
	"bytes"
	"testing"
)

// dispatch 는 unexported 라 외부에서 직접 호출 불가하지만,
// decompressBody helper 만 공개적으로 검증한다 (압축 자동 해제 동작).

func TestDecompressBodyNonePassthrough(t *testing.T) {
	c := &Client{}
	df := &DecodedFrame{
		Body:     []byte("raw payload"),
		BodyZipf: ZipfNone,
	}
	got := c.decompressBody(df)
	if !bytes.Equal(got, df.Body) {
		t.Errorf("ZipfNone 은 그대로: %q", got)
	}
}

func TestDecompressBodyZlibAuto(t *testing.T) {
	original := []byte("자동 압축 해제 테스트")
	frame := makeZlibFrame(t, original)

	c := &Client{}
	df := &DecodedFrame{
		Body:     frame,
		BodyZipf: ZipfZlib,
	}
	got := c.decompressBody(df)
	if !bytes.Equal(got, original) {
		t.Errorf("자동 해제 실패: got=%q want=%q", got, original)
	}
}

func TestDecompressBodyFallbackOnError(t *testing.T) {
	// 미지원 압축 방식이면 raw 그대로 반환 + warn (테스트는 raw 반환만 확인).
	c := &Client{}
	raw := []byte{0, 0, 0, 4, 'a', 'b', 'c', 'd'}
	df := &DecodedFrame{
		Body:     raw,
		BodyZipf: ZipfMlzo, // 미지원
	}
	got := c.decompressBody(df)
	if !bytes.Equal(got, raw) {
		t.Errorf("실패 시 raw 그대로 반환해야 함: got %q", got)
	}
}

func TestDecompressBodyEmpty(t *testing.T) {
	c := &Client{}
	df := &DecodedFrame{Body: nil, BodyZipf: ZipfZlib}
	got := c.decompressBody(df)
	if got != nil {
		t.Errorf("empty body 는 그대로 nil: %v", got)
	}
}

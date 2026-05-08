package mymq

import (
	"bytes"
	"compress/zlib"
	"errors"
	"strings"
	"testing"
)

// makeZlibFrame 은 broker 가 보내는 압축 본문 레이아웃을 생성한다:
// [orig_size:4 BE][zlib compressed]
func makeZlibFrame(t *testing.T, orig []byte) []byte {
	t.Helper()
	var compressed bytes.Buffer
	w := zlib.NewWriter(&compressed)
	if _, err := w.Write(orig); err != nil {
		t.Fatalf("zlib write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("zlib close: %v", err)
	}
	prefix := make([]byte, 4)
	putU32(prefix, uint32(len(orig)))
	return append(prefix, compressed.Bytes()...)
}

func TestDecompressNonePassthrough(t *testing.T) {
	in := []byte("plain payload")
	got, err := Decompress(in, ZipfNone)
	if err != nil {
		t.Fatalf("ZipfNone 패스: %v", err)
	}
	if !bytes.Equal(got, in) {
		t.Errorf("ZipfNone 은 그대로 반환해야 함: %q", got)
	}
}

func TestDecompressZlibRoundTrip(t *testing.T) {
	original := []byte(strings.Repeat("WTG-payload-", 100)) // ~1.2KB, 압축률 좋음
	frame := makeZlibFrame(t, original)

	got, err := Decompress(frame, ZipfZlib)
	if err != nil {
		t.Fatalf("Decompress: %v", err)
	}
	if !bytes.Equal(got, original) {
		t.Errorf("round-trip 불일치 (orig=%d got=%d)", len(original), len(got))
	}
}

func TestDecompressZlibSmallPayload(t *testing.T) {
	// 작은 본문 — 압축률 낮을 수 있지만 round-trip 은 보장.
	original := []byte(`{"symbol":"USDKRW","bid":1300.5,"ask":1300.7}`)
	frame := makeZlibFrame(t, original)

	got, err := Decompress(frame, ZipfZlib)
	if err != nil {
		t.Fatalf("Decompress: %v", err)
	}
	if !bytes.Equal(got, original) {
		t.Errorf("round-trip 불일치")
	}
}

func TestDecompressTruncated(t *testing.T) {
	// 4바이트보다 작으면 즉시 에러.
	short := []byte{0, 0}
	if _, err := Decompress(short, ZipfZlib); !errors.Is(err, ErrTruncatedCompressed) {
		t.Errorf("ErrTruncatedCompressed 기대, got %v", err)
	}
}

func TestDecompressUnsupportedAlgo(t *testing.T) {
	// MLZO 와 LZIV 는 미지원.
	frame := []byte{0, 0, 0, 4, 'a', 'b', 'c', 'd'}
	for _, z := range []Zipf{ZipfMlzo, ZipfLziv, Zipf(99)} {
		_, err := Decompress(frame, z)
		if !errors.Is(err, ErrUnsupportedZipf) {
			t.Errorf("zipf=%d → ErrUnsupportedZipf 기대, got %v", z, err)
		}
	}
}

func TestDecompressZlibBadPayload(t *testing.T) {
	// prefix 는 valid, 본문은 garbage → zlib reader 실패.
	bad := []byte{0, 0, 0, 100, 0xFF, 0xFF, 0xFF, 0xFF}
	if _, err := Decompress(bad, ZipfZlib); err == nil {
		t.Error("garbage zlib 본문은 에러 기대")
	}
}

func TestIsZipfSupported(t *testing.T) {
	cases := []struct {
		zipf Zipf
		want bool
	}{
		{ZipfNone, true},
		{ZipfZlib, true},
		{ZipfMlzo, false},
		{ZipfLziv, false},
		{Zipf(255), false},
	}
	for _, c := range cases {
		if got := IsZipfSupported(c.zipf); got != c.want {
			t.Errorf("IsZipfSupported(%d) = %v, want %v", c.zipf, got, c.want)
		}
	}
}

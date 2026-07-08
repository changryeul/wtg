package mymq

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// golden 데이터 — 실 EC2 엔진 (W1101T01) 응답을 mymq C minilzo 로 해제한 쌍.
//
//	testdata/lzo_reply_compressed.bin : [orig4B][LZO1X 스트림] (137B, wire 그대로)
//	testdata/lzo_reply_plain.bin      : C lzo1x_decompress_safe 출력 (10752B)
func loadLZOGolden(t *testing.T) (compressed, plain []byte) {
	t.Helper()
	var err error
	compressed, err = os.ReadFile("testdata/lzo_reply_compressed.bin")
	if err != nil {
		t.Fatalf("compressed golden 로드: %v", err)
	}
	plain, err = os.ReadFile("testdata/lzo_reply_plain.bin")
	if err != nil {
		t.Fatalf("plain golden 로드: %v", err)
	}
	return compressed, plain
}

func TestDecompressMLZOGolden(t *testing.T) {
	compressed, plain := loadLZOGolden(t)

	got, err := Decompress(compressed, ZipfMlzo)
	if err != nil {
		t.Fatalf("Decompress(MLZO): %v", err)
	}
	if len(got) != len(plain) {
		t.Fatalf("길이 불일치: got=%d want=%d", len(got), len(plain))
	}
	if !bytes.Equal(got, plain) {
		// 첫 불일치 위치 찾기 — 디버깅 편의.
		for i := range got {
			if got[i] != plain[i] {
				t.Fatalf("바이트 불일치 at %d: got=%#x want=%#x", i, got[i], plain[i])
			}
		}
	}

	// COMHDR 의미 검증 — 오프셋은 win/src/inc/com/comhdr.h.
	if !strings.HasPrefix(string(got[0:16]), "W1101T01") { // trxc
		t.Errorf("trxc 이상: %q", got[0:16])
	}
	if !strings.HasPrefix(string(got[74:104]), "admin01") { // usid
		t.Errorf("usid 이상: %q", got[74:104])
	}
}

func TestDecompressLZO1XCorrupt(t *testing.T) {
	compressed, _ := loadLZOGolden(t)

	cases := map[string][]byte{
		"빈 입력":        {},
		"prefix 만":    compressed[:4],
		"스트림 절단":      compressed[:len(compressed)/2],
		"EOF 마커 제거":   compressed[:len(compressed)-3],
		"prefix 만 유효": append(append([]byte{}, compressed[:4]...), 0xff, 0xff, 0xff),
	}
	for name, in := range cases {
		if _, err := Decompress(in, ZipfMlzo); err == nil {
			t.Errorf("%s: 에러 기대했으나 nil", name)
		}
	}

	// orig_size 위조 (실제보다 작게) — 출력 초과로 에러여야 한다.
	forged := append([]byte{}, compressed...)
	forged[0], forged[1], forged[2], forged[3] = 0, 0, 0, 16
	if _, err := Decompress(forged, ZipfMlzo); err == nil {
		t.Error("orig_size 위조: 에러 기대했으나 nil")
	}
}

func TestIsZipfSupportedMLZO(t *testing.T) {
	if !IsZipfSupported(ZipfMlzo) {
		t.Error("ZipfMlzo 는 지원으로 보고돼야 함")
	}
	if IsZipfSupported(ZipfLziv) {
		t.Error("ZipfLziv 는 미지원이어야 함")
	}
}

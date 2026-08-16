package pricekrx

import (
	"encoding/binary"
	"fmt"
	"math"
	"testing"

	wire "github.com/winwaysystems/wtg/pkg/krx"
	"github.com/winwaysystems/wtg/pkg/krxshm"
)

func put(b []byte, off, n int, s string) {
	for i := 0; i < n; i++ {
		b[off+i] = ' '
	}
	if len(s) > n {
		s = s[:n]
	}
	copy(b[off:off+n], s)
}

// buildA006F/buildA306F — 최소 원 TR (coherent code) — 오프셋은 pkg/krx codec 기준.
func mkA006F(code string, bprc, yprc float64) []byte {
	b := make([]byte, wire.SZMaster)
	put(b, 0, len(b), "")
	put(b, 0, 5, "A006F")
	put(b, 27, 12, code)
	put(b, 45, 1, "F")
	put(b, 397, 11, fmt.Sprintf("%.2f", bprc))
	put(b, 748, 11, fmt.Sprintf("%.2f", yprc))
	return b
}
func mkA306F(code string, cprc float64) []byte {
	b := make([]byte, wire.SZA306F)
	put(b, 0, len(b), "")
	put(b, 0, 5, "A306F")
	put(b, 17, 12, code)
	put(b, 47, 9, fmt.Sprintf("%.2f", cprc))
	return b
}

// TestHubIngestSHM — A006F(마스터)+A306F(체결) Ingest → SHM(FSISE_T) 에 전일대비 계산된
// 현재가가 정확히 적재되는지 (버퍼 Writer, mmap 불요).
func TestHubIngestSHM(t *testing.T) {
	buf := make([]byte, krxshm.ShmSize)
	w, err := krxshm.NewWriter(buf)
	if err != nil {
		t.Fatal(err)
	}
	h := New(w)

	if _, ok, err := h.Ingest(mkA006F("101V6000", 265.00, 265.50)); err != nil || !ok {
		t.Fatalf("A006F ingest ok=%v err=%v", ok, err)
	}
	if _, ok, err := h.Ingest(mkA306F("101V6000", 265.75)); err != nil || !ok {
		t.Fatalf("A306F ingest ok=%v err=%v", ok, err)
	}
	mn, qn := h.Stats()
	if mn != 1 || qn != 1 {
		t.Errorf("stats masters=%d quotes=%d want 1/1", mn, qn)
	}

	// SHM slot0(정렬 단일 종목) FSISE_T 검증 — 오프셋은 pkg/krxshm 과 동일 (엔트리 128, fsise 2048).
	entry := 128
	f := entry + 2048
	rd := func(o int) float64 { return math.Float64frombits(binary.LittleEndian.Uint64(buf[f+o:])) }
	if code := string(buf[entry : entry+8]); code != "101V6000" {
		t.Errorf("slot0 futCd=%q", code)
	}
	if rd(32) != 265.75 { // ePrc
		t.Errorf("ePrc=%v want 265.75", rd(32))
	}
	if rd(64) != 265.50 { // yPrc
		t.Errorf("yPrc=%v want 265.50", rd(64))
	}
	if d := rd(72); math.Abs(d-0.25) > 1e-9 { // diff
		t.Errorf("diff=%v want 0.25", d)
	}
	if buf[f+103] != '+' { // sign
		t.Errorf("sign=%q want +", buf[f+103])
	}
	if binary.LittleEndian.Uint32(buf[36:]) != 1 { // useN
		t.Errorf("useN=%d want 1", binary.LittleEndian.Uint32(buf[36:]))
	}
}

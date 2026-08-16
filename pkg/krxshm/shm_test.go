package krxshm

import (
	"encoding/binary"
	"math"
	"testing"
)

// TestLayoutUpdate — Layout(정렬 slot + futCd/헤더) + Update(FSISE_T poke)가 검증된
// linux 오프셋에 정확히 기록되는지 (버퍼 직접 poke, mmap 불필요).
func TestLayoutUpdate(t *testing.T) {
	buf := make([]byte, ShmSize)
	w, err := NewWriter(buf)
	if err != nil {
		t.Fatal(err)
	}
	// 정렬: 101V6000 < 105V3000 → slot 0,1
	if err := w.Layout(map[string]string{"105V3000": "5V3000", "101V6000": "1V6000"}); err != nil {
		t.Fatal(err)
	}
	if got := binary.LittleEndian.Uint32(buf[offMaxN:]); got != MaxItem {
		t.Errorf("maxN=%d want %d", got, MaxItem)
	}
	if got := binary.LittleEndian.Uint32(buf[offUseN:]); got != 2 {
		t.Errorf("useN=%d want 2", got)
	}
	// slot0 futCd = 101V6000 (정렬 첫번째)
	if c := string(buf[entryOff(0) : entryOff(0)+8]); c != "101V6000" {
		t.Errorf("slot0 futCd=%q want 101V6000", c)
	}
	if c := string(buf[entryOff(1) : entryOff(1)+8]); c != "105V3000" {
		t.Errorf("slot1 futCd=%q want 105V3000", c)
	}

	// Update 101V6000
	q := Quote{Code: "101V6000", Last: 265.75, PrevClose: 265.50, Diff: 0.25,
		Rate: 0.0942, Settle: 265.30, Sign: "+", BasePrc: 265.00, AccVol: 12345, Halt: false}
	if err := w.Update(q); err != nil {
		t.Fatal(err)
	}
	f := entryOff(0) + offFsise
	rd := func(o int) float64 { return math.Float64frombits(binary.LittleEndian.Uint64(buf[f+o:])) }
	if rd(fsEPrc) != 265.75 {
		t.Errorf("ePrc=%v want 265.75", rd(fsEPrc))
	}
	if rd(fsYPrc) != 265.50 {
		t.Errorf("yPrc=%v want 265.50", rd(fsYPrc))
	}
	if rd(fsDiff) != 0.25 {
		t.Errorf("diff=%v want 0.25", rd(fsDiff))
	}
	if rd(fsSPrc) != 265.30 {
		t.Errorf("sPrc=%v want 265.30", rd(fsSPrc))
	}
	if rate := math.Float32frombits(binary.LittleEndian.Uint32(buf[f+fsRate:])); math.Abs(float64(rate)-0.0942) > 1e-6 {
		t.Errorf("rate=%v want 0.0942", rate)
	}
	if buf[f+fsSign] != '+' {
		t.Errorf("sign=%q want +", buf[f+fsSign])
	}
	if v := binary.LittleEndian.Uint64(buf[f+fsAVol:]); v != 12345 {
		t.Errorf("aVol=%d want 12345", v)
	}
	if buf[f+fsHalt] != 0x20 {
		t.Errorf("halt=%q want space", buf[f+fsHalt])
	}

	// 미배치 종목 Update 는 에러
	if err := w.Update(Quote{Code: "999X9999"}); err == nil {
		t.Error("미배치 종목인데 에러 없음")
	}
}

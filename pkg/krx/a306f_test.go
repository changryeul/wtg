package krx

import (
	"encoding/json"
	"fmt"
	"testing"
)

func buildA306F() []byte {
	b := make([]byte, SZA306F)
	for i := range b {
		b[i] = ' '
	}
	put := func(off, n int, s string) {
		for i := 0; i < n; i++ {
			b[off+i] = ' '
		}
		if len(s) > n {
			s = s[:n]
		}
		copy(b[off:off+n], s)
	}
	put(0, 5, "A306F")                                 // TR코드
	put(17, 12, "101V6000")                            // code
	put(35, 12, "090005123456")                        // time
	put(47, 9, fmt.Sprintf("%9.2f", 265.75))           // cprc 체결가
	put(56, 9, fmt.Sprintf("%9d", 3))                  // cvol
	put(65, 9, fmt.Sprintf("%9.2f", 265.75))           // nprc 근월물
	put(74, 9, fmt.Sprintf("%9.2f", 266.10))           // fprc 원월물
	put(83, 9, fmt.Sprintf("%9.2f", 265.50))           // oprc 시가
	put(92, 9, fmt.Sprintf("%9.2f", 265.75))           // hprc 고가
	put(101, 9, fmt.Sprintf("%9.2f", 265.00))          // lprc 저가
	put(110, 9, fmt.Sprintf("%9.2f", 265.70))          // pprc 직전가
	put(119, 12, fmt.Sprintf("%12d", 12345))           // tvol
	put(131, 22, fmt.Sprintf("%22.2f", 3271500000.00)) // tamt
	put(153, 1, "2")                                   // ftcd 매수
	put(154, 9, fmt.Sprintf("%9.2f", 291.00))          // uldp 동적상한
	put(163, 9, fmt.Sprintf("%9.2f", 240.00))          // lldp 동적하한
	return b
}

func TestDecodeA306F(t *testing.T) {
	ft, err := DecodeA306F(buildA306F())
	if err != nil {
		t.Fatalf("DecodeA306F: %v", err)
	}
	if ft.Kind != "fut.trade" || ft.Code != "101V6000" || ft.Time != "090005123456" {
		t.Errorf("헤더: %+v", ft)
	}
	if ft.Last != 265.75 || ft.Cprc != 265.75 || ft.Cvol != 3 {
		t.Errorf("체결: last=%v cprc=%v cvol=%v", ft.Last, ft.Cprc, ft.Cvol)
	}
	if ft.Open != 265.50 || ft.High != 265.75 || ft.Low != 265.00 {
		t.Errorf("OHL: %v/%v/%v", ft.Open, ft.High, ft.Low)
	}
	if ft.NearPrc != 265.75 || ft.FarPrc != 266.10 {
		t.Errorf("근원월: %v/%v", ft.NearPrc, ft.FarPrc)
	}
	if ft.Tvol != 12345 || ft.Tamt != 3271500000.00 {
		t.Errorf("누적: tvol=%v tamt=%v", ft.Tvol, ft.Tamt)
	}
	if ft.Bs != "2" || ft.UpLimit != 291.00 || ft.DnLimit != 240.00 {
		t.Errorf("bs/상하한: %q/%v/%v", ft.Bs, ft.UpLimit, ft.DnLimit)
	}
	// 직전대비 — decode-time (cprc 265.75 vs pprc 265.70).
	if ft.PrevTradePrc != 265.70 {
		t.Errorf("직전가=%v, want 265.70", ft.PrevTradePrc)
	}
	if d := ft.Cprc - ft.PrevTradePrc; ft.Cdiff-d > 1e-9 || d-ft.Cdiff > 1e-9 {
		t.Errorf("cdiff=%v, want %v", ft.Cdiff, d)
	}
	if ft.Csign != "+" {
		t.Errorf("csign=%q, want +", ft.Csign)
	}
	// 전일대비/정산가는 마스터·H306F join 전이라 0.
	if ft.Diff != 0 || ft.PrevClose != 0 || ft.Settle != 0 {
		t.Errorf("join 전 필드가 0 아님: diff=%v prevClose=%v settle=%v", ft.Diff, ft.PrevClose, ft.Settle)
	}
	js, _ := json.Marshal(ft)
	t.Logf("JSON = %s", js)
}

func TestDecodeA306F_Guards(t *testing.T) {
	if _, err := DecodeA306F(make([]byte, 50)); err == nil {
		t.Error("길이 미달 무에러")
	}
	b := buildA306F()
	copy(b[0:5], "B606F")
	if _, err := DecodeA306F(b); err == nil {
		t.Error("A306F 아닌데 무에러")
	}
}

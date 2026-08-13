package krx

import (
	"encoding/json"
	"fmt"
	"math"
	"testing"

	wire "github.com/winwaysystems/wtg/pkg/krx"
)

// putField — 고정폭 프레임 헬퍼 (공백 채운 뒤 s 복사).
func putField(b []byte, off, n int, s string) {
	for i := 0; i < n; i++ {
		b[off+i] = ' '
	}
	if len(s) > n {
		s = s[:n]
	}
	copy(b[off:off+n], s)
}

// buildA006FMin — code + 기준가(bprc@397) + 전일종가(yprc@748) 만 채운 최소 마스터.
func buildA006FMin(code string, base, prevClose float64) []byte {
	b := make([]byte, wire.SZMaster)
	for i := range b {
		b[i] = ' '
	}
	putField(b, 0, 5, "A006F")
	putField(b, 27, 12, code)
	putField(b, 397, 11, fmt.Sprintf("%-11.02f", base))      // bprc 기준가
	putField(b, 748, 11, fmt.Sprintf("%-11.02f", prevClose)) // yprc 전일종가
	return b
}

// buildA306FMin — code + 체결가(cprc@47) 만 채운 최소 체결.
func buildA306FMin(code string, last float64) []byte {
	b := make([]byte, wire.SZA306F)
	for i := range b {
		b[i] = ' '
	}
	putField(b, 0, 5, "A306F")
	putField(b, 17, 12, code)
	putField(b, 35, 12, "090005123456")
	putField(b, 47, 9, fmt.Sprintf("%9.2f", last)) // cprc
	return b
}

// TestMasterJoinEnrichment — A006F(전일종가) 캐시 후 A306F 체결이 diff/rate/prevClose/sign 으로
// enrich 되는지 검증. 마스터 미도착 시엔 0 유지도 확인.
func TestMasterJoinEnrichment(t *testing.T) {
	srv := NewServer(nil)
	const code = "101V6000"

	// 1) 마스터 도착 전 체결 — enrich 안 됨 (diff/prevClose 0).
	if _, _, _, err := srv.IngestA306F(buildA306FMin(code, 265.75)); err != nil {
		t.Fatalf("A306F(pre-master): %v", err)
	}
	pre := srv.masters.GetFut(code)
	if pre != nil {
		t.Fatalf("마스터 없어야 하는데 캐시됨: %+v", pre)
	}

	// 2) 마스터 도착 — 전일종가 265.50 캐시.
	if _, _, _, err := srv.IngestA006F(buildA006FMin(code, 265.00, 265.50)); err != nil {
		t.Fatalf("A006F: %v", err)
	}
	if m := srv.masters.GetFut(code); m == nil || m.PrevClose != 265.50 || m.BasePrc != 265.00 {
		t.Fatalf("마스터 캐시 실패: %+v", m)
	}

	// 3) 마스터 도착 후 체결 — enrich 검증.
	ft, err := wire.DecodeA306F(buildA306FMin(code, 265.75))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	enrichFutTrade(ft, srv.masters.GetFut(code))
	if ft.PrevClose != 265.50 {
		t.Errorf("prevClose=%v, want 265.50", ft.PrevClose)
	}
	if ft.BasePrc != 265.00 {
		t.Errorf("basePrc=%v, want 265.00", ft.BasePrc)
	}
	if math.Abs(ft.Diff-0.25) > 1e-9 {
		t.Errorf("diff=%v, want 0.25", ft.Diff)
	}
	wantRate := 0.25 / 265.50 * 100.0
	if math.Abs(ft.Rate-wantRate) > 1e-9 {
		t.Errorf("rate=%v, want %v", ft.Rate, wantRate)
	}
	if ft.Sign != "+" {
		t.Errorf("sign=%q, want +", ft.Sign)
	}

	// JSON 에 실제로 반영되는지 (web 이 받는 최종 형태).
	js, _ := json.Marshal(ft)
	t.Logf("enriched fut.trade = %s", js)
}

// TestEnrichGroundTruth — C 피드 fut_real.c set_fsise_diff 정답지 수식/가드 대사.
// 각 케이스는 (전일종가 yprc, 기준가 bprc, 체결가 last) → 기대 (diff, rate, sign).
func TestEnrichGroundTruth(t *testing.T) {
	cases := []struct {
		name              string
		yprc, bprc, last  float64
		wantDiff, wantRat float64
		wantSign          string
	}{
		{"상승", 265.50, 265.00, 265.75, 0.25, 0.25 / 265.50 * 100, "+"},
		{"하락", 265.50, 265.00, 265.00, -0.50, -0.50 / 265.50 * 100, "-"},
		{"보합_yprc==eprc", 265.50, 265.00, 265.50, 0, 0, " "},
		{"전일종가0_기준가대체", 0, 100.00, 101.00, 1.00, 1.00 / 100.00 * 100, "+"},
		{"체결가0_가드", 265.50, 265.00, 0, 0, 0, " "},
		{"둘다0_가드", 0, 0, 100.00, 0, 0, " "},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ft := &wire.FutTrade{Code: "T", Last: c.last}
			enrichFutTrade(ft, &wire.FutMaster{Code: "T", PrevClose: c.yprc, BasePrc: c.bprc})
			if math.Abs(ft.Diff-c.wantDiff) > 1e-9 {
				t.Errorf("diff=%v, want %v", ft.Diff, c.wantDiff)
			}
			if math.Abs(ft.Rate-c.wantRat) > 1e-9 {
				t.Errorf("rate=%v, want %v", ft.Rate, c.wantRat)
			}
			if ft.Sign != c.wantSign {
				t.Errorf("sign=%q, want %q", ft.Sign, c.wantSign)
			}
			// prevClose 는 (C 와 동일) 대체 전 원 전일종가.
			if ft.PrevClose != c.yprc {
				t.Errorf("prevClose=%v, want %v (원 전일종가)", ft.PrevClose, c.yprc)
			}
		})
	}
}

// TestDirSign — 방향부호 3-way.
func TestDirSign(t *testing.T) {
	cases := []struct {
		diff float64
		want string
	}{{1.5, "+"}, {-0.5, "-"}, {0, " "}}
	for _, c := range cases {
		if got := dirSign(c.diff); got != c.want {
			t.Errorf("dirSign(%v)=%q, want %q", c.diff, got, c.want)
		}
	}
}

// TestEnrichNilMaster — 마스터 nil 이면 원값 유지 (패닉 없음).
func TestEnrichNilMaster(t *testing.T) {
	ft := &wire.FutTrade{Code: "X", Last: 100}
	enrichFutTrade(ft, nil)
	if ft.Diff != 0 || ft.PrevClose != 0 || ft.Sign != "" {
		t.Errorf("nil 마스터인데 변형됨: %+v", ft)
	}
}

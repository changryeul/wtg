// Package mdsshim 은 mci-mds-shim 의 W950x 고정폭 전문 코덱 —
// mds query-server (W9500) 대체 AP 의 wire 층이다.
// 전환 계획: docs/mds-replacement-plan.md Stage 2 (wire 호환 트랙).
//
// W9504A01 (수동 스왑포인트/마진 등록) 의 wire 명세 출처:
//   - 입력: win/src/inc/trn/W2006A01.h 의 W2006A01_I (trn W2006A01 이 tp call)
//   - 응답: mds/W9500/W9504A01.c 의 W9504A01_out_t
//   - 값 스케일: mds 는 STR2DBL(field) / 10^zdiv (fold->zdiv, 심볼별 소수 자릿수)
package mdsshim

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/winwaysystems/wtg/pkg/pricing"
)

// RegTp 는 W2006A01 의 등록구분이다.
type RegTp int

const (
	RegTpRegister   RegTp = 1 // 등록
	RegTpUnregister RegTp = 2 // 해제 — mds 는 전 tenor 의 수동 스왑을 NaN 으로 초기화
)

// W2006A01_I 고정폭 레이아웃 (바이트).
const (
	w9504HdrLen = 1 + 7 + 1 + 6 // regTp + crncPairId + mrgnTcd + grid01_cnt
	w9504RecLen = 3 + 8*10      // tnrId + 10바이트 수치 필드 8개
	w9504OutLen = 8             // W9504A01_out_t.crncPairId
)

// W9504Record 는 tenor 1건의 수동 입력값이다 (wire 원시 스케일 그대로).
type W9504Record struct {
	Tenor      string // mds tnrId: SPT/TOD/TOM/W01/M01/M02/M03/M06/Y01
	BidSwapPnt float64
	AskSwapPnt float64
	// 마진 4종 + MIN 2종은 mds W9504A01 이 소비하지 않는다 (trn 이 DB 에
	// 직접 저장) — shim 도 대칭으로 무시한다. 필요해지면 필드 추가.
}

// W9504Request 는 파싱된 W2006A01 전문이다.
type W9504Request struct {
	RegTp   RegTp
	Pair    string // 예: "USD/KRW" (7자 필드)
	MrgnTcd string // 1:BID-ASK 2:MID-MIN/MAX
	Records []W9504Record
}

// SwapUpdate 는 pkg/pricing 의 공용 갱신 단위 별칭 —
// Tenor 는 WTG 표기 (etc/pricing.json 컨벤션: 1W/1M/...).
type SwapUpdate = pricing.SwapUpdate

// mdsTenorToWTG 는 mds tnrId → WTG pricing tenor 표기 매핑.
// mds tenor2index 카탈로그 (mat/mds/mds.h TENOR_*) 의 9개 전체를 커버한다.
var mdsTenorToWTG = map[string]string{
	"SPT": "SPT",
	"TOD": "TOD",
	"TOM": "TOM",
	"W01": "1W",
	"M01": "1M",
	"M02": "2M",
	"M03": "3M",
	"M06": "6M",
	"Y01": "1Y",
}

// ParseW2006A01 은 trn 발 고정폭 전문을 파싱한다.
func ParseW2006A01(b []byte) (*W9504Request, error) {
	if len(b) < w9504HdrLen {
		return nil, fmt.Errorf("mdsshim: W2006A01 헤더 미달 (%d < %d)", len(b), w9504HdrLen)
	}
	regTp, err := strconv.Atoi(field(b, 0, 1))
	if err != nil || (regTp != int(RegTpRegister) && regTp != int(RegTpUnregister)) {
		return nil, fmt.Errorf("mdsshim: regTp 불량 %q", field(b, 0, 1))
	}
	nrec, err := strconv.Atoi(field(b, 9, 6))
	if err != nil {
		return nil, fmt.Errorf("mdsshim: grid01_cnt 불량 %q", field(b, 9, 6))
	}
	if want := w9504HdrLen + nrec*w9504RecLen; len(b) < want {
		return nil, fmt.Errorf("mdsshim: 본문 부족 — 선언 %d건 = %dB, 수신 %dB", nrec, want, len(b))
	}

	req := &W9504Request{
		RegTp:   RegTp(regTp),
		Pair:    field(b, 1, 7),
		MrgnTcd: field(b, 8, 1),
	}
	for i := 0; i < nrec; i++ {
		off := w9504HdrLen + i*w9504RecLen
		req.Records = append(req.Records, W9504Record{
			Tenor:      field(b, off, 3),
			BidSwapPnt: numField(b, off+3, 10),
			AskSwapPnt: numField(b, off+13, 10),
		})
	}
	return req, nil
}

// SwapUpdates 는 레코드를 WTG 반영 단위로 변환한다. 값은 mds 와 동일하게
// 10^zdiv 로 나눈다 (zdiv = 심볼 소수 자릿수, symbols 카탈로그에서 조회).
// 두 번째 반환값은 tenor 매핑 실패로 skip 한 건수.
func (r *W9504Request) SwapUpdates(zdiv int) ([]SwapUpdate, int) {
	scale := math.Pow10(zdiv)
	var ups []SwapUpdate
	skipped := 0
	for _, rec := range r.Records {
		wtgTenor, ok := mdsTenorToWTG[rec.Tenor]
		if !ok {
			skipped++
			continue
		}
		ups = append(ups, SwapUpdate{
			Pair:  r.Pair,
			Tenor: wtgTenor,
			Bid:   rec.BidSwapPnt / scale,
			Ask:   rec.AskSwapPnt / scale,
		})
	}
	return ups, skipped
}

// BuildW9504A01Reply 는 W9504A01_out_t (crncPairId[8]) 응답 전문을 만든다.
func BuildW9504A01Reply(pair string) []byte {
	out := make([]byte, w9504OutLen)
	for i := range out {
		out[i] = ' '
	}
	copy(out, pair)
	return out
}

// field 는 고정폭 필드를 trim 해 반환한다.
func field(b []byte, off, w int) string {
	return strings.TrimSpace(string(b[off : off+w]))
}

// numField 는 고정폭 수치 필드를 파싱한다 — 빈 필드/불량은 0 (mds atof 동일).
func numField(b []byte, off, w int) float64 {
	v, err := strconv.ParseFloat(field(b, off, w), 64)
	if err != nil {
		return 0
	}
	return v
}

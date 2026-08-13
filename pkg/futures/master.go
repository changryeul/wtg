package futures

import (
	"fmt"
	"strings"
)

// SZMaster 는 A006F(파생 종목정보, IFMSBTD0022) 전문 크기 (A006F.h A006F_T, endc 끝 1318).
const SZMaster = 1318

// A006F 는 KRX 원 마스터 TR. 앞 5바이트가 TR코드 "A006F" (datc[2]"A0"+infc[3]"06F").
// 130여 필드 중 web 표시에 필요한 핵심만 추린다. 오프셋은 A006F.h 주석의 누적 END
// (1-indexed) 기준: 필드 [end-len, end).

// FutMaster 는 선물/옵션 종목 기본정보 JSON envelope.
type FutMaster struct {
	Kind          string  `json:"kind"`          // "fut.master"
	Code          string  `json:"code"`          // 표준종목코드
	ShortCode     string  `json:"shortCode"`     // 단축코드 iscd
	Name          string  `json:"name"`          // 종목약명 ksnm
	NameFull      string  `json:"nameFull"`      // 종목명 klnm
	OptType       string  `json:"optType"`       // 선물옵션구분 focd (F:선물 C:콜 P:풋)
	Underlying    string  `json:"underlying"`    // 기초자산ID uaid
	UnderlyingCd  string  `json:"underlyingCd"`  // 기초자산종목코드 uacd
	ListDate      string  `json:"listDate"`      // 상장일 ltdt
	LastTradeDate string  `json:"lastTradeDate"` // 최종거래일 tddt
	Expiry        string  `json:"expiry"`        // 만기일 exdt
	Strike        float64 `json:"strike"`        // 행사가격 eprc (옵션)
	ExerciseType  string  `json:"exerciseType"`  // 권리행사유형 recd (A:미국 E:유럽)
	AtmType       string  `json:"atmType"`       // ATM구분 atmc (0:선물 1:ATM 2:ITM 3:OTM)
	BasePrc       float64 `json:"basePrc"`       // 기준가격 bprc
	PrevClose     float64 `json:"prevClose"`     // 전일종가 yprc
	UpLimit       float64 `json:"upLimit"`       // 상한가 upl1
	DnLimit       float64 `json:"dnLimit"`       // 하한가 lpl1
	Unit          float64 `json:"unit"`          // 거래단위
	Mult          float64 `json:"mult"`          // 거래승수
	PrevOI        int64   `json:"prevOI"`        // 전일미결제약정 pdoi
	IV            float64 `json:"iv"`            // 내재변동성 ipvl (옵션)
	Halt          bool    `json:"halt"`          // 거래정지 halt
}

// DecodeA006F 는 A006F 원 마스터 전문(≥1318B) → FutMaster (핵심 필드).
func DecodeA006F(b []byte) (*FutMaster, error) {
	if len(b) < SZMaster {
		return nil, fmt.Errorf("futures: A006F 길이 미달 (%d < %d)", len(b), SZMaster)
	}
	// 앞 5바이트 TR코드 확인 (datc[2]+infc[3] = "A006F").
	if tr := fstr(b, 0, 5); tr != "A006F" {
		return nil, fmt.Errorf("futures: A006F 아님 (tr=%q)", tr)
	}
	return &FutMaster{
		Kind:          "fut.master",
		Code:          fstr(b, 27, 12),         // code[12]  (datc2+infc3+seqn8+nofi6+bday8=27)
		OptType:       fstr(b, 45, 1),          // focd  end46
		ShortCode:     fstr(b, 57, 9),          // iscd  end66
		NameFull:      fstr(b, 66, 80),         // klnm  end146
		Name:          fstr(b, 146, 40),        // ksnm  end186
		ListDate:      fstr(b, 309, 8),         // ltdt  end317
		UpLimit:       ffloat(b, 331, 11),      // upl1 end342
		DnLimit:       ffloat(b, 364, 11),      // lpl1 end375
		BasePrc:       ffloat(b, 397, 11),      // bprc end408
		Underlying:    fstr(b, 408, 3),         // uaid  end411
		ExerciseType:  fstr(b, 411, 1),         // recd  end412
		LastTradeDate: fstr(b, 438, 8),         // tddt  end446
		Expiry:        fstr(b, 457, 8),         // exdt  end465
		Strike:        ffloat(b, 465, 18),      // eprc end483
		Unit:          ffloat(b, 484, 22),      // unit end506
		Mult:          ffloat(b, 506, 22),      // mult end528
		UnderlyingCd:  fstr(b, 543, 12),        // uacd  end555
		Halt:          isHalt(fstr(b, 689, 1)), // halt end690
		AtmType:       fstr(b, 730, 1),         // atmc  end731
		PrevClose:     ffloat(b, 748, 11),      // yprc end759
		PrevOI:        fint(b, 825, 12),        // pdoi  end837
		IV:            ffloat(b, 859, 11),      // ipvl end870
	}, nil
}

// isHalt — 거래정지여부 필드 판정. 문서상 "정상/거래정지" 문자열이나 운영은 1자
// (Y/1 = 정지) 가 흔해 공백·0·N·정상 외를 정지로 본다.
func isHalt(s string) bool {
	s = strings.TrimSpace(s)
	switch s {
	case "", "0", "N", "정상":
		return false
	default:
		return true
	}
}

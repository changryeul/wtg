// Package fxsync — 외환 운영 DB (TB_FXB_*) 의 마스터 데이터를 WTG etcd 로
// 미러링하는 sync agent.
//
// 운영 SoT 는 DB. WTG 는 read-only 캐시로 동작 — admin UI 는 view 만, 변경은
// 기존 시스템에서. WTG 가 메모리에 들고 다른 svc 가 read.
//
// 진입점은 cmd/fx-sync. Backend interface 로 DB / file mock 둘 다 지원.
package fxsync

// Currency — TB_FXB_CMG005M (통화기본) 의 도메인 매핑.
//
// DB column 매핑:
//   Code          ← CRCD            (CHAR 3)      통화코드 (USD/KRW/JPY)
//   Name          ← CRCD_NM         (VARCHAR 30)  통화명
//   RefCode       ← CRCD_REF_VL     (VARCHAR 3)   ISO 4217 numeric 등 참조값
//   DecimalPlaces ← DCPU_RCN        (NUMBER 5)    호가 표시 소수자리
//   PrecisionKind ← DCPN_PCSN_DCD   (CHAR 1)      정밀도 구분 (반올림 정책)
//   SortOrder     ← RPSN_SQC        (NUMBER 7)    표시 순서
//   Active        ← USE_YN          (CHAR 1)      Y/N
type Currency struct {
	Code          string `json:"code"`
	Name          string `json:"name"`
	RefCode       string `json:"ref_code,omitempty"`
	DecimalPlaces int    `json:"decimal_places"`
	PrecisionKind string `json:"precision_kind,omitempty"`
	SortOrder     int    `json:"sort_order,omitempty"`
	Active        bool   `json:"active"`
}

// Currencies — 정렬된 목록.
type Currencies []Currency

// Pair — TB_FXB_CMG004M (통화페어기본) + TB_FXB_CMG006M (통화별상품정보) 의
// 통합 도메인 매핑. fx-sync 가 두 테이블을 join 해서 본 struct 으로 emit.
//
// DB column 매핑 (CMG004M):
//   ID            ← CRNC_PAIR_ID     (VARCHAR 7)  "USDKRW" 식 식별자
//   Base          ← BASE_CRCD        (CHAR 3)     "USD"
//   Quote         ← COCU_CD          (CHAR 3)     "KRW" (counter currency)
//   QuoteDecimals ← CUS_CLCL_DCPU_RCN (NUMBER)    고객 표시 자릿수
//   EmpDecimals   ← EMP_CLCL_DCPU_RCN (NUMBER)    내부 계산 자릿수
//   ScaleUnit     ← FX_PCRR_UT       (NUMBER)     pair 단위 (JPY=100 인 100엔당 quoting)
//   PLPair        ← PL_CRNC_PAIR_ID  (VARCHAR 7)  PnL 환산 pair
//   SpotDays      ← SPOT_DT_CDNC_VL  (NUMBER)     T+N 영업일 (USD/KRW=2)
//   SortOrder     ← RPSN_SQC         (NUMBER)
//   Active        ← USE_YN
//
// DB column 매핑 (CMG006M — SPOT tenor 기준 한 row 가져옴):
//   Kind          ← MRKT_DATA_DCD    (CHAR 2)     "direct" | "cross" 변환
//   Symbol        ← MRKT_DATA_ORGN_VL (VARCHAR)   direct 일 때 cooker 외부 symbol
//   Cross         ← REF_DFCR_PAIR_ID + REF_WNDL_CRNC_PAIR_ID + FX_XRT_FNNL_MTHD_DCD
//                  cross 일 때만. {leg_a, op_a, leg_b, op_b, scale}.
//   ScaleUnit 이 100 이면 cross.scale 에 자동 반영 (fx-sync 책임).
type Pair struct {
	ID            string `json:"id"`
	Base          string `json:"base"`
	Quote         string `json:"quote"`
	Kind          string `json:"kind"`              // "direct" | "cross"
	Symbol        string `json:"symbol,omitempty"`  // direct only
	Cross         *Cross `json:"cross,omitempty"`   // cross only
	SpotDays      int    `json:"spot_days"`
	ScaleUnit     int    `json:"scale_unit,omitempty"` // 100JPY/KRW 같이 100 단위 quoting
	QuoteDecimals int    `json:"quote_decimals"`
	EmpDecimals   int    `json:"emp_decimals,omitempty"`
	PLPair        string `json:"pl_pair,omitempty"`
	SortOrder     int    `json:"sort_order,omitempty"`
	Active        bool   `json:"active"`
}

// Cross — broken-date 가 아닌 cross 산식. pricing.CrossFormula 와 1:1 매핑.
// fx-sync 가 DB → 본 struct 으로 변환 → mci-price 가 watch 후
// pricing.CrossFormula 로 사용.
type Cross struct {
	LegA  string  `json:"leg_a"`         // "EUR/USD" 또는 "USDEUR" (운영 컨벤션 명확화)
	OpA   string  `json:"op_a"`          // "mul" | "div"
	LegB  string  `json:"leg_b"`
	OpB   string  `json:"op_b"`
	Scale float64 `json:"scale,omitempty"`
}

// Pairs — 정렬된 목록.
type Pairs []Pair

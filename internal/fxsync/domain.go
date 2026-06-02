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
//
//	Code          ← CRCD            (CHAR 3)      통화코드 (USD/KRW/JPY)
//	Name          ← CRCD_NM         (VARCHAR 30)  통화명
//	RefCode       ← CRCD_REF_VL     (VARCHAR 3)   ISO 4217 numeric 등 참조값
//	DecimalPlaces ← DCPU_RCN        (NUMBER 5)    호가 표시 소수자리
//	PrecisionKind ← DCPN_PCSN_DCD   (CHAR 1)      정밀도 구분 (반올림 정책)
//	SortOrder     ← RPSN_SQC        (NUMBER 7)    표시 순서
//	Active        ← USE_YN          (CHAR 1)      Y/N
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
//
//	ID            ← CRNC_PAIR_ID     (VARCHAR 7)  "USDKRW" 식 식별자
//	Base          ← BASE_CRCD        (CHAR 3)     "USD"
//	Quote         ← COCU_CD          (CHAR 3)     "KRW" (counter currency)
//	QuoteDecimals ← CUS_CLCL_DCPU_RCN (NUMBER)    고객 표시 자릿수
//	EmpDecimals   ← EMP_CLCL_DCPU_RCN (NUMBER)    내부 계산 자릿수
//	ScaleUnit     ← FX_PCRR_UT       (NUMBER)     pair 단위 (JPY=100 인 100엔당 quoting)
//	PLPair        ← PL_CRNC_PAIR_ID  (VARCHAR 7)  PnL 환산 pair
//	SpotDays      ← SPOT_DT_CDNC_VL  (NUMBER)     T+N 영업일 (USD/KRW=2)
//	SortOrder     ← RPSN_SQC         (NUMBER)
//	Active        ← USE_YN
//
// DB column 매핑 (CMG006M — SPOT tenor 기준 한 row 가져옴):
//
//	Kind          ← MRKT_DATA_DCD    (CHAR 2)     "direct" | "cross" 변환
//	Symbol        ← MRKT_DATA_ORGN_VL (VARCHAR)   direct 일 때 cooker 외부 symbol
//	Cross         ← REF_DFCR_PAIR_ID + REF_WNDL_CRNC_PAIR_ID + FX_XRT_FNNL_MTHD_DCD
//	               cross 일 때만. {leg_a, op_a, leg_b, op_b, scale}.
//	ScaleUnit 이 100 이면 cross.scale 에 자동 반영 (fx-sync 책임).
type Pair struct {
	ID            string `json:"id"`
	Base          string `json:"base"`
	Quote         string `json:"quote"`
	Kind          string `json:"kind"`             // "direct" | "cross"
	Symbol        string `json:"symbol,omitempty"` // direct only
	Cross         *Cross `json:"cross,omitempty"`  // cross only
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
	LegA  string  `json:"leg_a"` // "EUR/USD" 또는 "USDEUR" (운영 컨벤션 명확화)
	OpA   string  `json:"op_a"`  // "mul" | "div"
	LegB  string  `json:"leg_b"`
	OpB   string  `json:"op_b"`
	Scale float64 `json:"scale,omitempty"`
}

// Pairs — 정렬된 목록.
type Pairs []Pair

// SwapPoint — TB_FXB_CMG021M 의 도메인 매핑.
//
// DB column 매핑:
//
//	Pair      ← CRNC_PAIR_ID   (VARCHAR 7)   "USDKRW"
//	Tenor     ← TNR_ID         (CHAR 3)      "1M" / "1W" / "3M"
//	BidAmount ← FWEB_SWAP_PNT  (NUMBER 15,8) 매수 swap (Forward Exchange Buy)
//	AskAmount ← FWES_SWAP_PNT  (NUMBER 15,8) 매도 swap (Forward Exchange Sell)
//	ApplyFrom ← PNT_APSR_TS    (DATE)        적용 시작 시각 (옵션, 일자 단위 운영)
//	ApplyTo   ← PNT_FNAP_TS    (DATE)        적용 종료 시각 (옵션)
//
// 시점은 후속 단계에서 다룸. 1차는 현재 유효한 swap 만 PricingTable 에 반영.
type SwapPoint struct {
	Pair      string  `json:"pair"`  // PricingTable 의 session.Pair 형식 ("USD/KRW")
	Tenor     string  `json:"tenor"` // "SPOT" | "1W" | "1M" ...
	BidAmount float64 `json:"bid_amount"`
	AskAmount float64 `json:"ask_amount"`
	ApplyFrom string  `json:"apply_from,omitempty"` // RFC3339 또는 빈값
	ApplyTo   string  `json:"apply_to,omitempty"`
}

// SwapPoints — 정렬된 목록.
type SwapPoints []SwapPoint

// HQMargin — TB_FXB_CMG019M (본점마진그룹별마진) 의 도메인 매핑.
//
// 본점 마진은 영업점/본점 사용자 모두에게 적용되는 base. 그룹 (Tier 와 매핑) 별
// 차등.
//
// DB column 매핑:
//
//	Pair      ← CRNC_PAIR_ID
//	Tier      ← HDOM_APLY_GRP_ID (3 byte 그룹 ID; WTG session.Tier 로 매핑)
//	BidAmount ← FX_BNG_SPR  (NUMBER 20,8) 매수 spread
//	AskAmount ← FX_SELL_SPR (NUMBER 20,8) 매도 spread
//	Tenor 는 1차 SPOT 만 사용 (CMG019M 의 TNR_ID 는 forward 별 마진 — 후속).
type HQMargin struct {
	Pair      string  `json:"pair"`
	Tier      string  `json:"tier"` // "" = 와일드카드. "VIP" / "GOLD" / "STD".
	BidAmount float64 `json:"bid_amount"`
	AskAmount float64 `json:"ask_amount"`
}

// HQMargins — 목록.
type HQMargins []HQMargin

// SiteMargin — TB_FXB_CMG015M (표준영업점마진) 의 도메인 매핑.
//
// 영업점 사용자에게 추가 부여되는 마진 (본점 사용자에겐 적용 X — Profile.Site
// 가 BRANCH 일 때만 매칭).
//
// DB column 매핑:
//
//	Pair      ← CRNC_PAIR_ID
//	Channel   ← (보통 "" = 모든 채널)
//	Site      ← "BRANCH" 고정 (CMG015M 는 영업점 전용)
//	BidAmount ← FX_BNG_SPR
//	AskAmount ← FX_SELL_SPR
type SiteMargin struct {
	Pair      string  `json:"pair"`
	Channel   string  `json:"channel,omitempty"`
	Site      string  `json:"site"`
	BidAmount float64 `json:"bid_amount"`
	AskAmount float64 `json:"ask_amount"`
}

// SiteMargins — 목록.
type SiteMargins []SiteMargin

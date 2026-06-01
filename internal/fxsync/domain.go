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

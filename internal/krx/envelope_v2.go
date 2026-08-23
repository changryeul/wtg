package krx

import (
	"encoding/json"
	"strings"
	"time"
)

// 폴리모픽 envelope v2 (KRX 트랙) — FX 엣지와 동일 스키마로 정규화한다.
// docs/unified-quote-edge-design.md §3.
//
//	{"ev":2,"type":"krx.fut.book","asset_class":"FUTURE","symbol":"101V6000",
//	 "ts_unix_nano":...,"data":{ ...원 KRX struct... }}
//
// 정규화 4가지: ① 판별자 kind→type(krx.* 네임스페이스), ② symbol=code,
// ③ 시각 HHMMSS→ts_unix_nano(KST), ④ 자산군 asset_class. legacy(flat)는
// 그대로 유지하고 v2 는 ?ev=2 클라에게만 — 기존 클라 무영향.

// kstLoc — KRX 시각(HHMMSSuuuuuu, 장중 로컬시각)을 절대시각으로 환산할 기준 TZ.
// 로드 실패 시 nil → 시각 변환은 0 반환(엣지는 계속 동작, ts 만 비움).
var kstLoc = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Seoul")
	if err != nil {
		return nil
	}
	return loc
}()

// krxV2Envelope — 안정 헤더 + 자산별 data(원 struct JSON passthrough).
type krxV2Envelope struct {
	EV         int             `json:"ev"`
	Type       string          `json:"type"`
	AssetClass string          `json:"asset_class"`
	Symbol     string          `json:"symbol"`
	TSUnixNano int64           `json:"ts_unix_nano"`
	Data       json.RawMessage `json:"data"`
}

// buildKrxV2 — legacy(flat) JSON 을 v2 envelope 로 감싼다. legacy 에서 kind/time 을
// 추출해 헤더를 채우고, data 에는 원 struct JSON 을 그대로 싣는다(재직렬화 없음 →
// 모든 KRX struct 에 대해 무변경으로 동작). 실패 시 (nil,err) → 호출자가 v1 폴백.
func buildKrxV2(code string, legacy []byte) ([]byte, error) {
	kind, tm, err := krxMeta(legacy)
	if err != nil {
		return nil, err
	}
	env := krxV2Envelope{
		EV:         2,
		Type:       "krx." + kind, // 예: krx.fut.book / krx.bond.trade
		AssetClass: assetClassForKind(kind),
		Symbol:     code,
		TSUnixNano: krxTimeToUnixNano(tm),
		Data:       json.RawMessage(legacy),
	}
	return json.Marshal(env)
}

// krxMeta — legacy struct JSON 에서 kind/time 추출 (v2 헤더·gRPC 이벤트 공용).
func krxMeta(legacy []byte) (kind, tm string, err error) {
	var meta struct {
		Kind string `json:"kind"`
		Time string `json:"time"`
	}
	if e := json.Unmarshal(legacy, &meta); e != nil {
		return "", "", e
	}
	return meta.Kind, meta.Time, nil
}

// assetClassForKind — kind 접두로 자산군 판별. fut.*→FUTURE, bond.*→BOND.
// (옵션 세분화는 후속 — 마스터의 optType 으로 OPTION 분리 가능.)
func assetClassForKind(kind string) string {
	switch {
	case strings.HasPrefix(kind, "fut."):
		return "FUTURE"
	case strings.HasPrefix(kind, "bond."):
		return "BOND"
	default:
		return "UNKNOWN"
	}
}

// krxTimeToUnixNano — KRX 시각 문자열(HHMMSSuuuuuu, 12자리)을 오늘(KST) 기준
// 절대 unix nano 로 환산. 파싱 불가/빈값/TZ 미로드 시 0.
//
// 형식: HH(2) MM(2) SS(2) micro(6). 일부 TR 은 micro 가 짧을 수 있어 앞 6자리만
// 필수로 본다.
func krxTimeToUnixNano(hhmmss string) int64 {
	if kstLoc == nil || len(hhmmss) < 6 {
		return 0
	}
	h := atoi2(hhmmss[0:2])
	m := atoi2(hhmmss[2:4])
	s := atoi2(hhmmss[4:6])
	if h < 0 || m < 0 || s < 0 || h > 23 || m > 59 || s > 60 {
		return 0
	}
	// 마이크로초(있으면) → 나노.
	var nsec int
	if len(hhmmss) >= 12 {
		if us := atoiN(hhmmss[6:12]); us >= 0 {
			nsec = us * 1000
		}
	}
	now := time.Now().In(kstLoc)
	t := time.Date(now.Year(), now.Month(), now.Day(), h, m, s, nsec, kstLoc)
	return t.UnixNano()
}

// atoi2 — 2자리 숫자 파싱 (음수=오류).
func atoi2(s string) int {
	if len(s) != 2 || s[0] < '0' || s[0] > '9' || s[1] < '0' || s[1] > '9' {
		return -1
	}
	return int(s[0]-'0')*10 + int(s[1]-'0')
}

// atoiN — 임의 길이 숫자 파싱 (음수=오류, 공백 허용해 0 취급).
func atoiN(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' {
			c = '0'
		}
		if c < '0' || c > '9' {
			return -1
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// Package quoteid 는 매칭 엔진에 보낼 주문이 참조할 QuoteID 발행 / 검증
// 인프라.
//
// 글로벌 FX 표준 — FIX 4.4 tag 117 (QuoteID), FX Global Code 2024 Principle
// 17 (last-look 투명성), MiFID II RTS 27/28 (best-execution 감사) — 에 맞춰,
// publish 된 모든 quote 에 unique ID + ValidUntilTime 을 부착한다. 클라이언트가
// 보낸 주문은 이 QuoteID 를 들고 오고, 엔진은 Registry 로 검증 → 사용자가
// "본 가격" 과 체결 가격을 동치로 보장한다.
//
// 인스턴스 prefix 는 두 mci-price 가 active-active 로 발급해도 ID 충돌이
// 없도록 한다. FIX MsgSeqNum 동치인 Sequence 는 인스턴스별 monotonic.
//
// Registry 추상화로 Memory (dev/test) 와 Redis (prod) 양쪽 구현. ValidUntil
// 이 지나면 자동 삭제 (Redis TTL / Memory lazy expiry + 옵션 grace).
package quoteid

import (
	"strconv"
	"sync/atomic"
	"time"

	"github.com/winwaysystems/wtg/pkg/session"
)

// QuoteID 는 FIX 4.4 tag 117 호환 문자열.
//
// 형식: "<instance>-<base36_unix_ms>-<base36_seq>"  (예: "A-mq4b3z-1f")
//   - instance — 발급한 mci-price 인스턴스 prefix ("A", "B", ...)
//   - base36_unix_ms — 발급 시각 (~8-9자)
//   - base36_seq — 인스턴스별 monotonic 시퀀스 (~1-7자)
//
// 최대 길이 ~24자 — FIX 4.4 String 범위 (최대 65,535 byte) 에 여유 있게 포함.
type QuoteID string

// Record 는 발행된 quote 의 권위 데이터.
//
// MiFID II RTS 27 best-execution 감사의 원본 — 사용자가 본 가격, 그 시점,
// 어느 Profile 의 stream 이었는지, 만료시각이 모두 저장된다.
//
// Broken-date forward (P5 5단계) 일 때 ValueDateUnixNano + OffsetDays +
// Interpolation* 필드가 채워진다 — 인접 standard tenor 의 swap 을 선형 보간한
// 근거를 audit 으로 보존. Standard tenor (SPOT/1W/1M/...) 일 때는 0/빈값.
type Record struct {
	QuoteID    QuoteID         `json:"quote_id"`
	Pair       session.Pair    `json:"pair"`
	Profile    session.Profile `json:"profile"`
	Tenor      string          `json:"tenor"` // "SPOT" / "1W" / "1M" ... ; broken-date 시 "" 또는 "VD"
	Bid        float64         `json:"bid"`
	Ask        float64         `json:"ask"`
	IssuedAt   int64           `json:"issued_unix_nano"`
	ValidUntil int64           `json:"valid_until_unix_nano"`
	Sequence   uint64          `json:"sequence"`
	Issuer     string          `json:"issuer"` // 인스턴스 prefix

	// P5 5단계 — broken-date 추적용. 표준 tenor 호출은 모두 0/빈값.
	ValueDateUnixNano    int64   `json:"value_date_unix_nano,omitempty"`
	OffsetDays           int     `json:"offset_days,omitempty"`
	InterpolatedFrom     string  `json:"interpolated_from,omitempty"` // 보간 하한 tenor (예: "1W")
	InterpolatedTo       string  `json:"interpolated_to,omitempty"`   // 보간 상한 tenor (예: "1M")
	InterpolationWeight  float64 `json:"interpolation_weight,omitempty"`
	InterpolatedSwapBid  float64 `json:"interpolated_swap_bid,omitempty"`
	InterpolatedSwapAsk  float64 `json:"interpolated_swap_ask,omitempty"`
}

// ValidAt 은 시각 t 가 [IssuedAt, ValidUntil) 범위에 있는지 본다.
// 등호 처리: IssuedAt 포함, ValidUntil 불포함 — 표준 half-open.
func (r Record) ValidAt(t time.Time) bool {
	n := t.UnixNano()
	return n >= r.IssuedAt && n < r.ValidUntil
}

// Generator 는 인스턴스별 단조 증가 QuoteID 발행자.
//
// Sequence 는 atomic uint64 — 이론상 wrap 없음 (1초 100만 발급해도 50만년+).
// 동일 millisecond 내 다중 발급도 seq 가 다르므로 ID 충돌 없음.
type Generator struct {
	instance string
	seq      atomic.Uint64
	now      func() time.Time
}

// NewGenerator 는 instance prefix 를 가진 Generator 를 만든다.
// instance 가 빈값이면 "A" 로 fallback (단일 인스턴스 dev 모드).
func NewGenerator(instance string) *Generator {
	if instance == "" {
		instance = "A"
	}
	return &Generator{instance: instance, now: time.Now}
}

// SetNow 는 테스트용 시간 주입.
func (g *Generator) SetNow(f func() time.Time) {
	if f != nil {
		g.now = f
	}
}

// Instance 는 발급자 prefix 를 반환.
func (g *Generator) Instance() string { return g.instance }

// Next 는 다음 QuoteID 를 발급.
func (g *Generator) Next() QuoteID {
	ms := g.now().UnixMilli()
	s := g.seq.Add(1)
	return QuoteID(g.instance + "-" + strconv.FormatInt(ms, 36) + "-" + strconv.FormatUint(s, 36))
}

// NextSequence 는 현재 sequence 카운터 값을 반환 (직전 Next 가 반환한 seq).
// Record.Sequence 필드에 직접 채울 때 사용.
func (g *Generator) NextSequence() uint64 { return g.seq.Load() }

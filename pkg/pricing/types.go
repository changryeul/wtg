// Package pricing 은 raw FX quote 에 마진(swap point / 본점 / 영업점·채널)을
// 적용해 고객 노출 시세를 산출한다.
//
// 설계 원칙:
//
//   - PricingTable 은 immutable snapshot. 갱신은 통째 교체.
//   - 조회는 atomic.Pointer 기반 lock-free read — hot path 에 mutex 금지.
//   - 마진 가변성은 pkg/session.Profile (Channel, Site, Tier) 로 표현.
//   - 마진 산식: bid 는 차감, ask 는 가산 (고객 불리 방향이 표준).
//   - 모든 CustomerQuote 에 TableVersion 을 포함해 사후 재현(감사) 가능.
//
// 운영에서는 mci-admin 이 etcd 에 마진 데이터를 write 하고, mci-price 가
// watch 로 받아 새 PricingTable 을 빌드해 Store.Replace 로 swap 한다.
//
// 도메인 enum (Channel/Site/Tier/Profile/Pair) 은 pkg/session 에 있다 — 마진
// 적용은 세션 컨텍스트 안에서만 의미가 있으므로 의존 방향은 pricing → session.
package pricing

import (
	"time"

	"github.com/winwaysystems/wtg/pkg/quote"
	"github.com/winwaysystems/wtg/pkg/session"
)

// Quote 는 pkg/quote.Quote 의 alias — 마진 적용 시점에서 입력 타입을
// 간결히 표기하기 위함. 새 코드는 quote.Quote 직접 사용을 권장.
type Quote = quote.Quote

// Tenor 는 만기. SPOT 외에 1W/1M/3M/6M/1Y 등 forward 만기를 지원한다.
// 만기는 시세 자체가 아니라 "어떤 스왑포인트를 적용할지" 만 결정하므로
// pricing 패키지 내부에 정의한다.
type Tenor string

const (
	TenorSpot Tenor = "SPOT"
	Tenor1W   Tenor = "1W"
	Tenor1M   Tenor = "1M"
	Tenor3M   Tenor = "3M"
	Tenor6M   Tenor = "6M"
	Tenor1Y   Tenor = "1Y"
)

// Margin 은 bid/ask 비대칭 마진. 단위는 호가의 절대값(price unit) 기준이며,
// 호출자 책임으로 pips ↔ price unit 변환을 일관되게 사용한다.
//
//   - BidAmount: raw.Bid 에서 차감될 양 (>= 0)
//   - AskAmount: raw.Ask 에 가산될 양 (>= 0)
//
// 스왑포인트는 만기·금리차에 따라 부호가 바뀔 수 있으므로 SwapPoint 용으로
// 사용할 때는 음수도 허용 (부호 포함 그대로 저장).
type Margin struct {
	BidAmount float64
	AskAmount float64
}

// CustomerQuote 는 마진 적용 후 고객에게 노출되는 최종 시세.
// 감사·분쟁 대응을 위해 raw 값과 TableVersion 을 함께 보존한다.
type CustomerQuote struct {
	Pair         session.Pair
	Profile      session.Profile
	Tenor        Tenor
	Bid          float64
	Ask          float64
	TS           time.Time
	RawBid       float64 // 산출 근거가 된 raw bid
	RawAsk       float64 // 산출 근거가 된 raw ask
	TableVersion int64   // 적용된 PricingTable.Version

	// QuoteID — FIX 4.4 tag 117 호환 식별자. PricingConsumer 가 발행 직전에
	// Generator 로 부착. 빈값이면 quoteid 미사용 (dev / 단위 테스트 경로).
	QuoteID string
	// ValidUntil — 토큰 만료시각. 빈 time.Time 이면 미설정 (= QuoteID 빈값과
	// 동치). 매칭 엔진이 거래 시점에 이 시각을 넘기지 않아야 한다.
	ValidUntil time.Time
}

// Package quote 는 raw FX 시세의 도메인 타입과 메모리 캐시를 제공한다.
//
// 의존 그래프 (단방향):
//
//	pkg/session  (도메인 enum: Pair 등)
//	    ↑
//	pkg/quote    (Quote, RingBuffer, Bar, Timeframe, SymbolMap)
//	    ↑
//	pkg/pricing  (Quote 에 마진 적용 → CustomerQuote)
//
// 책임 경계:
//
//   - Quote        : 시장 raw 시세 1 tick (bid/ask). 마진 무관.
//   - RingBuffer   : 심볼별 최근 N tick 메모리 보관 (챠트 초기 응답).
//   - Bar          : OHLC 봉 (timeframe 단위). DB 영속 대상.
//   - SymbolMap    : 외부 심볼 ↔ session.Pair 매핑.
//
// 마진 적용은 pkg/pricing 의 책임 — 이 패키지는 raw 시세만 다룬다.
package quote

import (
	"time"

	"github.com/winwaysystems/wtg/pkg/session"
)

// Quote 는 raw 시세 한 tick. 마진 적용 전 상태이며 모든 Profile 의
// 공통 입력이다.
type Quote struct {
	Pair session.Pair
	Bid  float64
	Ask  float64
	TS   time.Time
}

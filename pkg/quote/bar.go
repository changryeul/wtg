package quote

import (
	"fmt"
	"time"

	"github.com/winwaysystems/wtg/pkg/session"
)

// Timeframe 은 OHLC 봉의 시간 단위.
//
// 영속화 대상:
//   - TF1s 는 **메모리만** (RingBuffer 와 함께 사용, 챠트 초기 응답용).
//   - TF1m 이상은 DB 에 영속 (TimescaleDB, docs/chart-schema.md 참조).
//
// 봉 경계는 UTC 기준 — 시간대에 상관없이 globally consistent.
type Timeframe string

const (
	TF1s  Timeframe = "1s"
	TF1m  Timeframe = "1m"
	TF5m  Timeframe = "5m"
	TF15m Timeframe = "15m"
	TF1h  Timeframe = "1h"
	TF1d  Timeframe = "1d"
)

// AllTimeframes 는 시스템이 지원하는 모든 timeframe (선언 순서 = 짧은→긴).
// Aggregator 가 모든 TF 를 동시에 누적할 때 사용.
var AllTimeframes = []Timeframe{TF1s, TF1m, TF5m, TF15m, TF1h, TF1d}

// PersistentTimeframes 는 DB 에 영속되는 timeframe (TF1s 제외).
// Archiver 가 이 목록만 INSERT 한다.
var PersistentTimeframes = []Timeframe{TF1m, TF5m, TF15m, TF1h, TF1d}

// Duration 은 timeframe 의 wall-clock 길이.
func (t Timeframe) Duration() time.Duration {
	switch t {
	case TF1s:
		return time.Second
	case TF1m:
		return time.Minute
	case TF5m:
		return 5 * time.Minute
	case TF15m:
		return 15 * time.Minute
	case TF1h:
		return time.Hour
	case TF1d:
		return 24 * time.Hour
	default:
		return 0
	}
}

// Persistent 는 이 timeframe 의 봉이 DB 영속 대상인지 반환한다.
// TF1s 는 메모리 전용, 나머지는 영속.
func (t Timeframe) Persistent() bool {
	return t != TF1s && t.Duration() > 0
}

// BucketStart 는 ts 가 속한 봉의 canonical 시작 시각을 반환한다 (UTC).
// 같은 봉에 속하는 모든 tick 은 동일한 BucketStart 를 가지므로 PRIMARY KEY
// 충돌(멀티 인스턴스 동시 INSERT) 시 안전한 dedup 키로 작용한다.
func (t Timeframe) BucketStart(ts time.Time) time.Time {
	d := t.Duration()
	if d <= 0 {
		return ts.UTC()
	}
	// time.Truncate 는 Unix epoch 기준 정렬 — UTC 자정 정렬과 일치한다.
	return ts.UTC().Truncate(d)
}

// Validate 는 timeframe 이 지원되는 값인지 검사.
func (t Timeframe) Validate() error {
	if t.Duration() <= 0 {
		return fmt.Errorf("quote: 지원하지 않는 timeframe %q", t)
	}
	return nil
}

// Bar 는 OHLC + bid/ask spread 정보를 가진 봉 1건.
//
// 봉의 lifecycle:
//
//	NewBar(first tick)   →   Update(more ticks)   →   Close()
//	     ↑ OpenedAt 확정     ↑ High/Low/Close 갱신    ↑ ClosedAt 확정
//
// docs/chart-schema.md 의 quote_bars 테이블과 컬럼이 1:1 대응.
type Bar struct {
	Pair     session.Pair
	TF       Timeframe
	OpenedAt time.Time // 봉 시작 (포함). Timeframe.BucketStart 결과.
	ClosedAt time.Time // 봉 종료 (불포함). Close() 호출 시 OpenedAt+Duration.

	OpenBid  float64
	OpenAsk  float64
	HighBid  float64
	HighAsk  float64
	LowBid   float64
	LowAsk   float64
	CloseBid float64
	CloseAsk float64

	TickCount int // 봉에 포함된 tick 수 (volume 대용)
}

// NewBar 는 첫 tick 으로 새 봉을 연다. OpenedAt 는 first.TS 가 속한 bucket
// 시작 시각으로 자동 정렬된다.
func NewBar(tf Timeframe, first Quote) *Bar {
	return &Bar{
		Pair:      first.Pair,
		TF:        tf,
		OpenedAt:  tf.BucketStart(first.TS),
		OpenBid:   first.Bid,
		OpenAsk:   first.Ask,
		HighBid:   first.Bid,
		HighAsk:   first.Ask,
		LowBid:    first.Bid,
		LowAsk:    first.Ask,
		CloseBid:  first.Bid,
		CloseAsk:  first.Ask,
		TickCount: 1,
	}
}

// Update 는 새 tick 을 봉에 흡수한다 (High/Low/Close 갱신).
// 호출자는 q.TS 가 현재 봉의 bucket 범위 안에 있음을 보장해야 한다
// (Bar.Contains 으로 확인 가능).
func (b *Bar) Update(q Quote) {
	if q.Bid > b.HighBid {
		b.HighBid = q.Bid
	}
	if q.Bid < b.LowBid {
		b.LowBid = q.Bid
	}
	if q.Ask > b.HighAsk {
		b.HighAsk = q.Ask
	}
	if q.Ask < b.LowAsk {
		b.LowAsk = q.Ask
	}
	b.CloseBid = q.Bid
	b.CloseAsk = q.Ask
	b.TickCount++
}

// Close 는 봉의 ClosedAt 을 OpenedAt + TF.Duration() 로 설정한다.
// 한 봉당 한 번만 호출해야 한다.
func (b *Bar) Close() {
	b.ClosedAt = b.OpenedAt.Add(b.TF.Duration())
}

// Contains 는 ts 가 이 봉의 bucket 범위에 포함되는지 반환한다.
// 범위는 [OpenedAt, OpenedAt + Duration).
func (b *Bar) Contains(ts time.Time) bool {
	end := b.OpenedAt.Add(b.TF.Duration())
	utc := ts.UTC()
	return !utc.Before(b.OpenedAt) && utc.Before(end)
}

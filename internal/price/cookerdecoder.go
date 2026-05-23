package price

import "github.com/winwaysystems/wtg/pkg/quote"

// JSONCookerDecoder 는 v1 평면 JSON envelope (docs/cooker-quote-schema.md) 을
// 파싱해서 Aggregator 의 CookerBodyDecoder 시그니처로 어댑트한다.
//
// 사용:
//
//	agg := price.NewAggregator(symbols, price.JSONCookerDecoder(), onClose)
//
// 검증은 quote.DecodeJSONEnvelope 가 수행하며, 실패하면 ok=false 로 drop.
// (bid<=0, ask<bid, sym 누락, JSON 파싱 실패, 빈 body 모두 drop)
func JSONCookerDecoder() CookerBodyDecoder {
	return func(body []byte) (bid, ask float64, ok bool) {
		env, err := quote.DecodeJSONEnvelope(body)
		if err != nil {
			return 0, 0, false
		}
		return env.Bid, env.Ask, true
	}
}

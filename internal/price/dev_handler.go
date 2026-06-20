package price

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// dev_handler.go — DevMode 전용 우회 tick 주입 endpoint.
//
// 운영 path: forwarder/cooker → broker broadcast → mci-price (handleUnsolicited).
// dev 우회: 외부 스크립트 / forwarder 가 직접 HTTP POST → 동일 consumers chain.
//
// 사용 시점: broker handshake / representative receiver 문제로 mci-price 가
// broadcast 를 받지 못하는 dev 환경 검증. Aggregator → BarCloseHandler →
// PricingConsumer → gRPC SubscribeBar 까지의 path 는 정상 검증된다.
//
// 활성화 조건: cfg.DevMode == true. 운영 빌드에선 라우트 등록 자체가 없음
// (server.go 의 mux 등록 분기).

// devTickRequest — POST /v1/dev/tick body.
type devTickRequest struct {
	Symbol string  `json:"symbol"`       // 외부 심볼 (예: "USDKRW") — etcd SymbolMap lookup 대상
	Bid    float64 `json:"bid"`          // 매도 호가
	Ask    float64 `json:"ask"`          // 매수 호가
	TS     string  `json:"ts,omitempty"` // RFC3339 (선택 — 비면 now)
}

// DevTickHandler — POST /v1/dev/tick.
//
// 정상 path 의 handleUnsolicited 와 동일하게 Tick → conflation → consumers 호출.
// Tick.Body 에 v1 평면 envelope JSON 을 박아서 Aggregator 의 JSONCookerDecoder
// 가 자연스럽게 bid/ask 추출.
func (s *Server) DevTickHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req devTickRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad_json: "+err.Error(), http.StatusBadRequest)
			return
		}
		if req.Symbol == "" || req.Bid <= 0 || req.Ask < req.Bid {
			http.Error(w, "validation: symbol 필수, bid>0, ask>=bid", http.StatusBadRequest)
			return
		}

		// v1 평면 envelope (docs/cooker-quote-schema.md) — JSONCookerDecoder 가 파싱.
		// BestConsumer 가 Tick.Source 와 envelope.src 모두 요구 — DEV 라벨 부여.
		body := []byte(fmt.Sprintf(`{"sym":"%s","bid":%g,"ask":%g,"src":"DEV"}`, req.Symbol, req.Bid, req.Ask))

		tick := &Tick{
			Symbol:   req.Symbol,
			Source:   "DEV",
			Body:     body,
			Received: time.Now(),
		}
		// 정상 path 와 동일한 카운터 / consumer 호출.
		s.totalRecv.Add(1)
		s.totalMatch.Add(1)
		s.conflation.Update(tick)
		for _, c := range s.consumers {
			c.OnTick(tick)
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

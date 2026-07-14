package pricing

import (
	"encoding/json"
	"fmt"

	"github.com/winwaysystems/wtg/pkg/session"
)

// SwapUpdate 는 스왑포인트 갱신 1건 (mds W9504A01 / admin REST 공용 단위).
type SwapUpdate struct {
	Pair  string  `json:"pair"`
	Tenor string  `json:"tenor"`
	Bid   float64 `json:"bid"`
	Ask   float64 `json:"ask"`
}

// ApplySwapToDoc 은 etcd 의 PricingTableDoc JSON 에 스왑포인트 갱신을 적용한
// 새 JSON 을 돌려준다 (순수 함수 — etcd get/put 루프는 호출부가 담당).
//
//   - clear=true: 해당 pair 의 SwapPoint 항목 전체 삭제 (mds regTp=2 동등)
//   - clear=false: (pair, tenor) upsert — Bid/Ask 만 교체, Skew/Spread 는 보존
//   - docJSON 이 빈값이면 새 문서에서 시작
//
// Version 은 +1 (watch 측 갱신 감지 컨벤션).
func ApplySwapToDoc(docJSON []byte, pair string, ups []SwapUpdate, clear bool) ([]byte, error) {
	var doc PricingTableDoc
	if len(docJSON) > 0 {
		if err := json.Unmarshal(docJSON, &doc); err != nil {
			return nil, fmt.Errorf("pricing: doc 파싱 실패: %w", err)
		}
	}

	p := session.Pair(pair)
	if clear {
		kept := doc.SwapPoint[:0]
		for _, e := range doc.SwapPoint {
			if e.Pair != p {
				kept = append(kept, e)
			}
		}
		doc.SwapPoint = kept
	} else {
		for _, up := range ups {
			t := Tenor(up.Tenor)
			found := false
			for i := range doc.SwapPoint {
				if doc.SwapPoint[i].Pair == p && doc.SwapPoint[i].Tenor == t {
					doc.SwapPoint[i].BidAmount = up.Bid
					doc.SwapPoint[i].AskAmount = up.Ask
					found = true
					break
				}
			}
			if !found {
				doc.SwapPoint = append(doc.SwapPoint, SwapEntryDoc{
					Pair: p, Tenor: t, BidAmount: up.Bid, AskAmount: up.Ask,
				})
			}
		}
	}

	doc.Version++
	return json.Marshal(&doc)
}

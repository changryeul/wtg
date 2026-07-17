package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// scenario.go — mock LP 시나리오 모델 + FIX 35=W 빌더 (순수 로직, TDD 대상).
//
// mock-lp 는 LP(원천: SMB/KMB/EBS/CMB…)별 결정적 호가/체결을 FIX 4.4 35=W 로
// UDP 송신해, quote-forwarder → BestConsumer → CrossRateConsumer → AlgoStream
// 경로를 결정적으로 e2e 검증한다. load-gen(랜덤 부하) 과 달리 값을 지정한다.

// Quote — 한 LP 의 한 통화쌍 호가(+선택적 체결).
type Quote struct {
	LP      string  `json:"lp"`   // 원천 (FIX 49=SenderCompID = excode). 예: "SMB"
	Pair    string  `json:"pair"` // 통화쌍 (FIX 55). 예: "USDKRW"
	Bid     float64 `json:"bid"`
	Ask     float64 `json:"ask"`
	Last    float64 `json:"last,omitempty"`     // 시장 체결가 (269=2). 0 이면 미포함.
	LastQty float64 `json:"last_qty,omitempty"` // 체결 수량 (271).
}

// Scenario — 시나리오 파일 (quotes 목록).
type Scenario struct {
	Quotes []Quote `json:"quotes"`
}

// parseScenario — JSON 시나리오 파싱.
func parseScenario(b []byte) (Scenario, error) {
	var sc Scenario
	if err := json.Unmarshal(b, &sc); err != nil {
		return Scenario{}, fmt.Errorf("scenario JSON 파싱: %w", err)
	}
	return sc, nil
}

// parseFeeds — "SMB:host:port,KMB:host:port" → map[LP]="host:port".
// LP 이름에 콜론이 없다고 가정 (첫 콜론까지가 LP, 나머지가 dest).
func parseFeeds(spec string) (map[string]string, error) {
	out := map[string]string{}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		colon := strings.IndexByte(part, ':')
		if colon < 1 || colon == len(part)-1 {
			return nil, fmt.Errorf("feed spec %q: LP:host:port 형식 필요", part)
		}
		lp := part[:colon]
		dest := part[colon+1:]
		if !strings.Contains(dest, ":") {
			return nil, fmt.Errorf("feed spec %q: dest 는 host:port 형식 필요", part)
		}
		out[lp] = dest
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("feed spec 비어있음")
	}
	return out, nil
}

// destFor — quote 의 LP 를 feeds 맵으로 dest 해소.
func destFor(feeds map[string]string, q Quote) (string, bool) {
	d, ok := feeds[q.LP]
	return d, ok
}

const fixSOH = "\x01"

// buildSnapshot — Quote 를 FIX 4.4 35=W (MarketDataSnapshotFullRefresh) 로.
//   - 49=<LP>(SenderCompID=excode), 55=<pair>
//   - 269=0/270=bid, 269=1/270=ask (각 271=수량)
//   - Last>0 이면 269=2/270=last/271=last_qty (Trade — mds fillprc 대응)
//
// 가격은 shortest round-trip(-1) 로 원 값 보존. quote-forwarder fastExtractV1
// 이 그대로 파싱한다.
func buildSnapshot(q Quote) []byte {
	entries := 2
	if q.Last > 0 {
		entries = 3
	}
	fields := []string{
		"8=FIX.4.4", "9=0", "35=W", "49=" + q.LP, "56=WTG", "55=" + q.Pair,
		"268=" + strconv.Itoa(entries),
		"269=0", "270=" + f(q.Bid), "271=1000000",
		"269=1", "270=" + f(q.Ask), "271=1000000",
	}
	if q.Last > 0 {
		fields = append(fields,
			"269=2", "270="+f(q.Last), "271="+f(q.LastQty), "278=MOCK")
	}
	fields = append(fields, "10=000", "")
	return []byte(strings.Join(fields, fixSOH))
}

func f(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

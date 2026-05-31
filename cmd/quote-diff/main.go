// Command quote-diff — 두 ws source 의 envelope 일치 자동 비교 도구.
//
// 용도:
//   - cs P4-1 단계 (broker subscribe + ws subscribe dual run) 의 confidence 확보
//   - legacy/best envelope 변환 정확도 검증 (WTG 측 단독)
//
// 흐름:
//   1. --source-a / --source-b 두 ws endpoint connect + subscribe
//   2. envelope 받으면 평면 {symbol, seq, bid, ask, ts} 로 normalize
//      (legacy: entries 에서 bid/ask 추출 / best: data.bid/data.ask 직접)
//   3. (symbol, seq) 키로 match — 둘 다 도착하면 비교
//   4. match/mismatch/orphan 카운터 + bid/ask 차이 (Δ) 통계
//   5. 종료 (--duration 만료 또는 SIGINT) 시 summary 출력
//
// 예시:
//
//	quote-diff \
//	    --source-a ws://localhost:8083/v1/subscribe \
//	    --source-b ws://localhost:8089/v1/subscribe \
//	    --pairs USD/KRW,EUR/KRW \
//	    --duration 30s
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

// normalizedTick — envelope 양식 무관 평면 형식.
type normalizedTick struct {
	Symbol string
	Seq    uint32
	Bid    float64
	Ask    float64
	TS     string
	Source string // "a" 또는 "b"
	RecvAt time.Time
}

// key — (symbol, seq, ts) 매칭 키. seq 만으론 부족 — 다중 feed 환경에서 같은
// pair 의 seq 가 feed 별 독립 카운터일 수 있어 ts 까지 키에 포함해야 동일 raw
// envelope 의 두 source 표현이 매칭됨.
type key struct {
	sym string
	seq uint32
	ts  string
}

// matcher — 양쪽 source 에서 도착한 tick 을 키로 매칭, 비교, 카운트.
type matcher struct {
	mu      sync.Mutex
	pending map[key]normalizedTick // 한쪽만 도착한 상태

	matched    atomic.Uint64 // 양쪽 도착 + 같음
	mismatched atomic.Uint64 // 양쪽 도착 + 다름
	totalA     atomic.Uint64
	totalB     atomic.Uint64
	maxBidDiff atomic.Uint64 // 큰 cumulative — IEEE bit
	maxAskDiff atomic.Uint64
	expireAge  time.Duration

	logger *slog.Logger
}

func newMatcher(logger *slog.Logger, expire time.Duration) *matcher {
	return &matcher{
		pending:   make(map[key]normalizedTick, 1024),
		expireAge: expire,
		logger:    logger,
	}
}

// Submit — 한 source 의 tick 등록 + 반대편 도착했으면 비교.
func (m *matcher) Submit(t normalizedTick) {
	if t.Source == "a" {
		m.totalA.Add(1)
	} else {
		m.totalB.Add(1)
	}
	k := key{t.Symbol, t.Seq, t.TS}

	m.mu.Lock()
	other, ok := m.pending[k]
	if ok {
		delete(m.pending, k)
	} else {
		m.pending[k] = t
	}
	m.mu.Unlock()

	if !ok {
		return
	}
	// 두 source 의 tick 비교.
	bidDiff := math.Abs(t.Bid - other.Bid)
	askDiff := math.Abs(t.Ask - other.Ask)
	const epsilon = 1e-9
	if bidDiff < epsilon && askDiff < epsilon {
		m.matched.Add(1)
	} else {
		m.mismatched.Add(1)
		m.logger.Warn("mismatch",
			slog.String("sym", t.Symbol),
			slog.Uint64("seq", uint64(t.Seq)),
			slog.Float64("a_bid", t.Bid), slog.Float64("a_ask", t.Ask),
			slog.Float64("b_bid", other.Bid), slog.Float64("b_ask", other.Ask),
			slog.Float64("bid_diff", bidDiff),
			slog.Float64("ask_diff", askDiff),
		)
		// max diff 갱신 (atomic — float64 를 bit 패턴으로).
		bitDiff := math.Float64bits(bidDiff)
		for {
			cur := m.maxBidDiff.Load()
			if bitDiff <= cur {
				break
			}
			if m.maxBidDiff.CompareAndSwap(cur, bitDiff) {
				break
			}
		}
		bitDiff = math.Float64bits(askDiff)
		for {
			cur := m.maxAskDiff.Load()
			if bitDiff <= cur {
				break
			}
			if m.maxAskDiff.CompareAndSwap(cur, bitDiff) {
				break
			}
		}
	}
}

// Expire — 한쪽만 도착한 tick 들 중 너무 오래된 것 제거 (drop 으로 카운트).
func (m *matcher) Expire() (expired int) {
	cutoff := time.Now().Add(-m.expireAge)
	m.mu.Lock()
	for k, t := range m.pending {
		if t.RecvAt.Before(cutoff) {
			delete(m.pending, k)
			expired++
		}
	}
	m.mu.Unlock()
	return
}

// Summary — 종료 시 통계 출력.
func (m *matcher) Summary() {
	matched := m.matched.Load()
	mismatched := m.mismatched.Load()
	totalA := m.totalA.Load()
	totalB := m.totalB.Load()
	m.mu.Lock()
	orphan := len(m.pending)
	m.mu.Unlock()
	maxBid := math.Float64frombits(m.maxBidDiff.Load())
	maxAsk := math.Float64frombits(m.maxAskDiff.Load())

	total := matched + mismatched
	matchRate := 100.0
	if total > 0 {
		matchRate = float64(matched) / float64(total) * 100
	}

	fmt.Println("\n══════ quote-diff Summary ══════")
	fmt.Printf("  source A: %d ticks received\n", totalA)
	fmt.Printf("  source B: %d ticks received\n", totalB)
	fmt.Printf("  matched     : %d (%.2f%% of paired)\n", matched, matchRate)
	fmt.Printf("  mismatched  : %d\n", mismatched)
	fmt.Printf("  orphan      : %d (한쪽만 도착, expire 안 됨)\n", orphan)
	if mismatched > 0 {
		fmt.Printf("  max bid diff: %.10f\n", maxBid)
		fmt.Printf("  max ask diff: %.10f\n", maxAsk)
	}
	fmt.Println("════════════════════════════════")
}

// parseEnvelope — best (data.bid/ask) 또는 legacy (entries[]) 형식 → normalizedTick.
func parseEnvelope(raw []byte, source string) (normalizedTick, bool) {
	// 두 schema 의 공통 outer.
	var outer struct {
		// best
		Symbol string `json:"symbol"`
		SeqNum uint32 `json:"seq_num"`
		Data   struct {
			Sym string  `json:"sym"`
			Bid float64 `json:"bid"`
			Ask float64 `json:"ask"`
			Seq uint32  `json:"seq"`
			TS  string  `json:"ts"`
		} `json:"data"`
		// legacy
		Seq     uint32 `json:"seq"`
		TS      string `json:"ts"`
		Feed    string `json:"feed"`
		MsgType string `json:"msgtype"`
		Entries []struct {
			Type string  `json:"type"`
			Px   float64 `json:"px"`
		} `json:"entries"`
		// control
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &outer); err != nil {
		return normalizedTick{}, false
	}
	// control message (subscribed / unsubscribed / error) — skip.
	if outer.Type != "" && outer.SeqNum == 0 && outer.Data.Bid == 0 && outer.Data.Ask == 0 && len(outer.Entries) == 0 {
		return normalizedTick{}, false
	}
	out := normalizedTick{Source: source, RecvAt: time.Now()}

	// best 형식 — data 에 채워짐.
	if outer.Data.Sym != "" {
		out.Symbol = outer.Data.Sym
		out.Seq = outer.Data.Seq
		out.Bid = outer.Data.Bid
		out.Ask = outer.Data.Ask
		out.TS = outer.Data.TS
		return out, true
	}
	// legacy 형식 — entries 에서 추출.
	if len(outer.Entries) > 0 && outer.Symbol != "" {
		out.Symbol = outer.Symbol
		out.Seq = outer.Seq
		out.TS = outer.TS
		for _, e := range outer.Entries {
			switch e.Type {
			case "bid":
				out.Bid = e.Px
			case "ask":
				out.Ask = e.Px
			}
		}
		return out, true
	}
	return normalizedTick{}, false
}

// streamSource — 단일 ws connect + subscribe + envelope 수신 → matcher.Submit.
func streamSource(ctx context.Context, logger *slog.Logger, url, label string, pairs []string, m *matcher) error {
	h := http.Header{}
	h.Set("Origin", "http://quote-diff")
	logger.Info("ws connect", slog.String("source", label), slog.String("url", url))
	c, _, err := websocket.DefaultDialer.DialContext(ctx, url, h)
	if err != nil {
		return fmt.Errorf("ws dial %s: %w", label, err)
	}
	defer c.Close()

	subMsg := map[string]any{"type": "subscribe", "pairs": pairs}
	subBytes, _ := json.Marshal(subMsg)
	if err := c.WriteMessage(websocket.TextMessage, subBytes); err != nil {
		return fmt.Errorf("ws subscribe %s: %w", label, err)
	}
	logger.Info("ws subscribed", slog.String("source", label), slog.Any("pairs", pairs))

	// ctx 종료 시 ws close.
	go func() {
		<-ctx.Done()
		_ = c.Close()
	}()

	for {
		_, data, err := c.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("ws read %s: %w", label, err)
		}
		t, ok := parseEnvelope(data, label)
		if !ok {
			continue
		}
		m.Submit(t)
	}
}

func main() {
	var (
		sourceA  = flag.String("source-a", "ws://localhost:8083/v1/subscribe", "ws URL A (best format)")
		sourceB  = flag.String("source-b", "ws://localhost:8089/v1/subscribe", "ws URL B (legacy format)")
		pairsStr = flag.String("pairs", "USD/KRW,EUR/KRW,JPY/KRW", "subscribe pair 리스트")
		duration = flag.Duration("duration", 30*time.Second, "총 실행 시간")
		expire   = flag.Duration("expire", 5*time.Second, "orphan tick 의 max age (이상 시 매칭 포기)")
		usid     = flag.String("user", "diff", "DevMode 인증용 usid (?x_wtg_user=)")
	)
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	pairs := []string{}
	for _, p := range strings.Split(*pairsStr, ",") {
		if p = strings.TrimSpace(p); p != "" {
			pairs = append(pairs, p)
		}
	}
	if len(pairs) == 0 {
		logger.Error("pair 가 비어있음")
		os.Exit(2)
	}

	// 인증 query 자동 첨부 (DevMode).
	addAuth := func(u string) string {
		sep := "?"
		if strings.Contains(u, "?") {
			sep = "&"
		}
		return u + sep + "x_wtg_user=" + *usid
	}
	urlA := addAuth(*sourceA)
	urlB := addAuth(*sourceB)

	ctx, cancel := context.WithTimeout(context.Background(), *duration)
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	m := newMatcher(logger, *expire)

	// expire 주기 — orphan tick 청소.
	go func() {
		t := time.NewTicker(*expire)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.Expire()
			}
		}
	}()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := streamSource(ctx, logger, urlA, "a", pairs, m); err != nil {
			logger.Warn("source a 종료", slog.Any("err", err))
		}
	}()
	go func() {
		defer wg.Done()
		if err := streamSource(ctx, logger, urlB, "b", pairs, m); err != nil {
			logger.Warn("source b 종료", slog.Any("err", err))
		}
	}()
	wg.Wait()

	m.Summary()
	if m.mismatched.Load() > 0 {
		os.Exit(1)
	}
}

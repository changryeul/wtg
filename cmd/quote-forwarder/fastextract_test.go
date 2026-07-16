package main

import (
	"strconv"
	"strings"
	"testing"
)

// build35W — load-gen / wtgctl burst 와 동일한 35=W snapshot wire.
func build35W(feed, sym string, bid, ask float64) []byte {
	parts := []string{
		"8=FIX.4.4", "9=80", "35=W",
		"49=" + feed, "56=SUB",
		"55=" + sym,
		"268=2",
		"269=0", "270=" + strconv.FormatFloat(bid, 'f', 4, 64), "271=1000000",
		"269=1", "270=" + strconv.FormatFloat(ask, 'f', 4, 64), "271=1500000",
		"10=000", "",
	}
	return []byte(strings.Join(parts, "\x01"))
}

func TestFastExtractV1_Snapshot(t *testing.T) {
	buf := build35W("SMB", "USDKRW", 1380.4500, 1380.5500)
	sym, bid, ask, _, _, ok := fastExtractV1(buf)
	if !ok || sym != "USDKRW" || bid != 1380.45 || ask != 1380.55 {
		t.Errorf("got sym=%q bid=%v ask=%v ok=%v", sym, bid, ask, ok)
	}
}

// build35WithTrade — bid/ask + 체결(269=2) 이 함께 온 35=W snapshot.
func build35WithTrade(sym string, bid, ask, last, lastQty float64) []byte {
	parts := []string{
		"8=FIX.4.4", "9=99", "35=W", "49=SMB", "56=SUB",
		"55=" + sym, "268=3",
		"269=0", "270=" + strconv.FormatFloat(bid, 'f', 4, 64), "271=1000000",
		"269=1", "270=" + strconv.FormatFloat(ask, 'f', 4, 64), "271=1500000",
		"269=2", "270=" + strconv.FormatFloat(last, 'f', 4, 64),
		"271=" + strconv.FormatFloat(lastQty, 'f', 0, 64),
		"10=000", "",
	}
	return []byte(strings.Join(parts, "\x01"))
}

// 체결가(269=2) 가 bid/ask 와 함께 오면 last/lastQty 로 추출된다 (mds fillprc 대응).
func TestFastExtractV1_Trade(t *testing.T) {
	buf := build35WithTrade("USDKRW", 1380.45, 1380.55, 1380.50, 500000)
	sym, bid, ask, last, lastQty, ok := fastExtractV1(buf)
	if !ok || sym != "USDKRW" || bid != 1380.45 || ask != 1380.55 {
		t.Fatalf("bid/ask 추출 실패: sym=%q bid=%v ask=%v ok=%v", sym, bid, ask, ok)
	}
	if last != 1380.50 {
		t.Errorf("last=%v, want 1380.50", last)
	}
	if lastQty != 500000 {
		t.Errorf("lastQty=%v, want 500000", lastQty)
	}
}

// 체결가 없는 일반 snapshot 은 last=0 (trade entry 부재).
func TestFastExtractV1_NoTrade(t *testing.T) {
	buf := build35W("SMB", "USDKRW", 1380.45, 1380.55)
	_, _, _, last, lastQty, ok := fastExtractV1(buf)
	if !ok || last != 0 || lastQty != 0 {
		t.Errorf("trade 없는데 last=%v lastQty=%v ok=%v", last, lastQty, ok)
	}
}

func TestFastExtractV1_MatchesParseQuoteExtractV1(t *testing.T) {
	cases := []struct {
		feed, sym string
		bid, ask  float64
	}{
		{"SMB", "USDKRW", 1380.45, 1380.55},
		{"KMB", "EURUSD", 1.0850, 1.0852},
		{"EBS", "USDJPY", 156.20, 156.22},
		{"REUT", "GBPUSD", 1.2740, 1.2742},
	}
	for _, c := range cases {
		buf := build35W(c.feed, c.sym, c.bid, c.ask)

		// 기존 path
		richEnv := parseQuote(buf)
		oldSym, oldBid, oldAsk, oldOk := extractV1(richEnv)

		// fast path
		newSym, newBid, newAsk, _, _, newOk := fastExtractV1(buf)

		if oldOk != newOk || oldSym != newSym || oldBid != newBid || oldAsk != newAsk {
			t.Errorf("불일치 (%s/%s): old=(%q,%v,%v,%v) new=(%q,%v,%v,%v)",
				c.feed, c.sym, oldSym, oldBid, oldAsk, oldOk, newSym, newBid, newAsk, newOk)
		}
	}
}

func TestFastExtractV1_OneSideOnly(t *testing.T) {
	// bid 만 있는 incremental — ok=false
	buf := []byte(strings.Join([]string{
		"8=FIX.4.4", "35=X",
		"55=USDKRW",
		"268=1", "279=0", "269=0", "270=1380.45", "271=1000000",
		"10=000", "",
	}, "\x01"))
	if _, _, _, _, _, ok := fastExtractV1(buf); ok {
		t.Errorf("bid 만인데 ok=true")
	}
}

func TestFastExtractV1_MissingSym(t *testing.T) {
	buf := []byte(strings.Join([]string{
		"8=FIX.4.4", "35=W",
		"268=2",
		"269=0", "270=1.0", "271=100",
		"269=1", "270=1.1", "271=200",
		"10=000", "",
	}, "\x01"))
	if _, _, _, _, _, ok := fastExtractV1(buf); ok {
		t.Errorf("sym 없는데 ok=true")
	}
}

func TestFastExtractV1_CrossedAskLessThanBid(t *testing.T) {
	buf := build35W("SMB", "USDKRW", 1380.55, 1380.45) // crossed
	if _, _, _, _, _, ok := fastExtractV1(buf); ok {
		t.Errorf("ask<bid 인데 ok=true")
	}
}

func TestFastExtractV1_Garbage(t *testing.T) {
	if _, _, _, _, _, ok := fastExtractV1(nil); ok {
		t.Errorf("nil 에 ok=true")
	}
	if _, _, _, _, _, ok := fastExtractV1([]byte("garbage")); ok {
		t.Errorf("garbage 에 ok=true")
	}
}

// ── Benchmark ──

var benchBuf = build35W("SMB", "USDKRW", 1380.45, 1380.55)

func BenchmarkParseQuoteExtractV1(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		env := parseQuote(benchBuf)
		_, _, _, _ = extractV1(env)
	}
}

func BenchmarkFastExtractV1(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _, _, _, _, _ = fastExtractV1(benchBuf)
	}
}

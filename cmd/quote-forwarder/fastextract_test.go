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
	sym, bid, ask, ok := fastExtractV1(buf)
	if !ok || sym != "USDKRW" || bid != 1380.45 || ask != 1380.55 {
		t.Errorf("got sym=%q bid=%v ask=%v ok=%v", sym, bid, ask, ok)
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
		newSym, newBid, newAsk, newOk := fastExtractV1(buf)

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
	if _, _, _, ok := fastExtractV1(buf); ok {
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
	if _, _, _, ok := fastExtractV1(buf); ok {
		t.Errorf("sym 없는데 ok=true")
	}
}

func TestFastExtractV1_CrossedAskLessThanBid(t *testing.T) {
	buf := build35W("SMB", "USDKRW", 1380.55, 1380.45) // crossed
	if _, _, _, ok := fastExtractV1(buf); ok {
		t.Errorf("ask<bid 인데 ok=true")
	}
}

func TestFastExtractV1_Garbage(t *testing.T) {
	if _, _, _, ok := fastExtractV1(nil); ok {
		t.Errorf("nil 에 ok=true")
	}
	if _, _, _, ok := fastExtractV1([]byte("garbage")); ok {
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
		_, _, _, _ = fastExtractV1(benchBuf)
	}
}

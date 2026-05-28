package main

import "testing"

func TestExtractV1Snapshot(t *testing.T) {
	// 35=W snapshot — env.Symbol 최상위 + bid/ask entries
	env := quoteEnvelope{
		MsgType: "snapshot",
		Symbol:  "USDKRW",
		Entries: []mdEntry{
			{Type: "bid", Px: 1380.45, Qty: 1000000},
			{Type: "ask", Px: 1380.55, Qty: 1500000},
		},
	}
	sym, bid, ask, ok := extractV1(env)
	if !ok || sym != "USDKRW" || bid != 1380.45 || ask != 1380.55 {
		t.Errorf("ok=%v sym=%q bid=%v ask=%v", ok, sym, bid, ask)
	}
}

func TestExtractV1Incremental(t *testing.T) {
	// 35=X incremental — entry 별 Symbol
	env := quoteEnvelope{
		MsgType: "incremental",
		Entries: []mdEntry{
			{Type: "bid", Symbol: "EURUSD", Px: 1.0850},
			{Type: "ask", Symbol: "EURUSD", Px: 1.0852},
		},
	}
	sym, bid, ask, ok := extractV1(env)
	if !ok || sym != "EURUSD" || bid != 1.0850 || ask != 1.0852 {
		t.Errorf("ok=%v sym=%q bid=%v ask=%v", ok, sym, bid, ask)
	}
}

func TestExtractV1OneSideOnly(t *testing.T) {
	// bid 만 — ok=false (cache 결합은 forwarder scope 밖)
	env := quoteEnvelope{
		MsgType: "incremental",
		Entries: []mdEntry{{Type: "bid", Symbol: "USDJPY", Px: 156.20}},
	}
	if _, _, _, ok := extractV1(env); ok {
		t.Errorf("bid 만인데 ok=true — single-side 발행은 v1 envelope 검증을 통과하면 안 됨")
	}
}

func TestExtractV1MissingSymbol(t *testing.T) {
	env := quoteEnvelope{
		Entries: []mdEntry{
			{Type: "bid", Px: 1.0},
			{Type: "ask", Px: 1.1},
		},
	}
	if _, _, _, ok := extractV1(env); ok {
		t.Errorf("sym 없는데 ok=true")
	}
}

func TestExtractV1AskLessThanBid(t *testing.T) {
	env := quoteEnvelope{
		Symbol: "USDKRW",
		Entries: []mdEntry{
			{Type: "bid", Px: 1400.0},
			{Type: "ask", Px: 1395.0}, // crossed
		},
	}
	if _, _, _, ok := extractV1(env); ok {
		t.Errorf("ask<bid 인데 ok=true — quote.DecodeJSONEnvelope 가 reject 할 값")
	}
}

func TestExtractV1IgnoresTradeEntries(t *testing.T) {
	env := quoteEnvelope{
		Symbol: "USDKRW",
		Entries: []mdEntry{
			{Type: "trade", Px: 1380.50, Qty: 500000}, // 무시되어야
			{Type: "bid", Px: 1380.45},
			{Type: "ask", Px: 1380.55},
		},
	}
	sym, bid, ask, ok := extractV1(env)
	if !ok || sym != "USDKRW" || bid != 1380.45 || ask != 1380.55 {
		t.Errorf("trade entry 가 bid/ask 추출을 방해함: ok=%v sym=%q bid=%v ask=%v", ok, sym, bid, ask)
	}
}

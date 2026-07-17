package main

import (
	"bytes"
	"strconv"
	"testing"
)

// extractQuote — buildSnapshot 출력이 quote-forwarder fastExtractV1 과 동일
// 알고리즘(269→270/271)으로 파싱되는지 검증하는 테스트용 추출기.
func extractQuote(buf []byte) (lp, pair string, bid, ask, last, lastQty float64, entries int) {
	var et byte
	for _, f := range bytes.Split(buf, []byte{0x01}) {
		eq := bytes.IndexByte(f, '=')
		if eq <= 0 {
			continue
		}
		tag := string(f[:eq])
		val := string(f[eq+1:])
		switch tag {
		case "49":
			lp = val
		case "55":
			pair = val
		case "269":
			et = val[0]
			entries++
		case "270":
			switch et {
			case '0':
				bid = mustFloat(val)
			case '1':
				ask = mustFloat(val)
			case '2':
				last = mustFloat(val)
			}
		case "271":
			if et == '2' {
				lastQty = mustFloat(val)
			}
		}
	}
	return
}

func mustFloat(s string) float64 {
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

func TestBuildSnapshot_BidAsk(t *testing.T) {
	q := Quote{LP: "SMB", Pair: "USDKRW", Bid: 1380.10, Ask: 1380.20}
	buf := buildSnapshot(q)
	lp, pair, bid, ask, last, _, entries := extractQuote(buf)
	if lp != "SMB" || pair != "USDKRW" {
		t.Fatalf("lp=%q pair=%q", lp, pair)
	}
	if bid != 1380.10 || ask != 1380.20 {
		t.Errorf("bid=%v ask=%v, want 1380.10/1380.20", bid, ask)
	}
	if last != 0 {
		t.Errorf("last=%v, want 0 (체결 미지정)", last)
	}
	if entries != 2 {
		t.Errorf("entries=%d, want 2 (bid/ask)", entries)
	}
}

func TestBuildSnapshot_WithTrade(t *testing.T) {
	q := Quote{LP: "KMB", Pair: "USDKRW", Bid: 1380.05, Ask: 1380.30, Last: 1380.15, LastQty: 500000}
	buf := buildSnapshot(q)
	_, _, bid, ask, last, lastQty, entries := extractQuote(buf)
	if bid != 1380.05 || ask != 1380.30 {
		t.Errorf("bid=%v ask=%v", bid, ask)
	}
	if last != 1380.15 || lastQty != 500000 {
		t.Errorf("last=%v qty=%v, want 1380.15/500000", last, lastQty)
	}
	if entries != 3 {
		t.Errorf("entries=%d, want 3 (bid/ask/trade)", entries)
	}
	// 35=W snapshot 인지.
	if !bytes.Contains(buf, []byte("35=W")) {
		t.Errorf("35=W 아님: %s", bytes.ReplaceAll(buf, []byte{0x01}, []byte("|")))
	}
}

func TestParseScenario(t *testing.T) {
	js := []byte(`{"quotes":[
		{"lp":"SMB","pair":"USDKRW","bid":1380.10,"ask":1380.20,"last":1380.15},
		{"lp":"KMB","pair":"USDKRW","bid":1380.05,"ask":1380.30},
		{"lp":"SMB","pair":"USDCNH","bid":7.10,"ask":7.11}
	]}`)
	sc, err := parseScenario(js)
	if err != nil {
		t.Fatal(err)
	}
	if len(sc.Quotes) != 3 {
		t.Fatalf("quotes %d, want 3", len(sc.Quotes))
	}
	if sc.Quotes[0].LP != "SMB" || sc.Quotes[0].Last != 1380.15 {
		t.Errorf("quote[0]=%+v", sc.Quotes[0])
	}
	if sc.Quotes[2].Pair != "USDCNH" {
		t.Errorf("quote[2]=%+v", sc.Quotes[2])
	}
}

func TestParseFeeds(t *testing.T) {
	m, err := parseFeeds("SMB:127.0.0.1:30044,KMB:127.0.0.1:30045")
	if err != nil {
		t.Fatal(err)
	}
	if m["SMB"] != "127.0.0.1:30044" || m["KMB"] != "127.0.0.1:30045" {
		t.Errorf("feeds=%v", m)
	}
	// 잘못된 형식.
	if _, err := parseFeeds("SMB"); err == nil {
		t.Error("형식 오류인데 err=nil")
	}
}

// destFor — quote 의 LP 를 feeds 맵으로 dest 해소.
func TestDestFor(t *testing.T) {
	feeds := map[string]string{"SMB": "127.0.0.1:30044", "KMB": "127.0.0.1:30045"}
	if d, ok := destFor(feeds, Quote{LP: "SMB"}); !ok || d != "127.0.0.1:30044" {
		t.Errorf("SMB dest=%q ok=%v", d, ok)
	}
	if _, ok := destFor(feeds, Quote{LP: "EBS"}); ok {
		t.Error("미등록 LP 인데 ok=true")
	}
}

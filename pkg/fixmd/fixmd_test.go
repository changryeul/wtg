package fixmd

import (
	"strconv"
	"strings"
	"testing"
)

func build35W(sym string, bid, ask, bidSz, askSz float64) []byte {
	p := []string{
		"8=FIX.4.4", "35=W", "55=" + sym, "268=2",
		"269=0", "270=" + strconv.FormatFloat(bid, 'f', 4, 64), "271=" + strconv.FormatFloat(bidSz, 'f', 0, 64),
		"269=1", "270=" + strconv.FormatFloat(ask, 'f', 4, 64), "271=" + strconv.FormatFloat(askSz, 'f', 0, 64),
		"10=000", "",
	}
	return []byte(strings.Join(p, "\x01"))
}

func TestParseSnapshot_TopOfBookWithAvail(t *testing.T) {
	s, ok := ParseSnapshot(build35W("USDKRW", 1380.45, 1380.55, 1_000_000, 1_500_000))
	if !ok {
		t.Fatal("ok=false")
	}
	if s.Sym != "USDKRW" || s.Bid != 1380.45 || s.Ask != 1380.55 {
		t.Errorf("top-of-book 오류: %+v", s)
	}
	if s.BidSize != 1_000_000 || s.AskSize != 1_500_000 {
		t.Errorf("avail 오류: bidSize=%v askSize=%v", s.BidSize, s.AskSize)
	}
}

func TestParseSnapshot_Reject(t *testing.T) {
	// crossed (ask<bid)
	if _, ok := ParseSnapshot(build35W("USDKRW", 1381, 1380, 0, 0)); ok {
		t.Error("crossed 인데 ok=true")
	}
	if _, ok := ParseSnapshot([]byte("garbage")); ok {
		t.Error("garbage ok=true")
	}
	if _, ok := ParseSnapshot(nil); ok {
		t.Error("nil ok=true")
	}
}

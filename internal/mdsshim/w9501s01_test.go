package mdsshim

import (
	"strings"
	"testing"
)

func TestParseW9501S01(t *testing.T) {
	in := fixedField("SPT", 4) + fixedField("0", 4) + fixedField("USDKRW", 16) + fixedField("", 16)
	req, err := ParseW9501S01([]byte(in))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.Pdcd != "SPT" || req.Pair != "USD/KRW" {
		t.Fatalf("불일치: %+v", req)
	}
	// FWD 는 미지원 (forward-snapshot 은 phase 2)
	if _, err := ParseW9501S01([]byte(strings.Replace(in, "SPT", "FWD", 1))); err == nil {
		t.Fatal("FWD 는 에러여야 함")
	}
	if _, err := ParseW9501S01([]byte("short")); err == nil {
		t.Fatal("길이 미달은 에러여야 함")
	}
}

func TestBuildW9501S01Reply(t *testing.T) {
	bars := []ChartBar{{
		Kymd: "20260713", Khms: "090000",
		BidO: 1385.5, BidH: 1386, BidL: 1385, BidC: 1385.8,
		AskO: 1385.7, AskH: 1386.2, AskL: 1385.2, AskC: 1386,
	}}
	out := BuildW9501S01Reply("SPT", "USDKRW", bars)
	if len(out) != 40+208 {
		t.Fatalf("len=%d, want 248", len(out))
	}
	if got := strings.TrimSpace(string(out[36:40])); got != "1" {
		t.Fatalf("nrec=%q", got)
	}
	dat := out[40:]
	if got := strings.TrimRight(string(dat[32:48]), " \x00"); got != "20260713" {
		t.Fatalf("kymd=%q", got)
	}
	if got := strings.TrimRight(string(dat[64:80]), " \x00"); got != "1385.50000" {
		t.Fatalf("bid_open=%q", got)
	}
}

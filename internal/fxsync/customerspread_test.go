package fxsync

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/winwaysystems/wtg/pkg/session"
)

// customerSpreadsToEntries — CustomerSpread → pricing.CustomerEntryDoc(override).

func TestCustomerSpreadsToEntries(t *testing.T) {
	cs := CustomerSpreads{
		{Usid: "alice01", Pair: "USD/KRW", BidDelta: 0.15, AskDelta: 0.20, Active: true},
		{Usid: "bob02", Pair: "EUR/KRW", BidDelta: 0.30, AskDelta: 0.30, Active: true},
		{Usid: "gone03", Pair: "USD/KRW", BidDelta: 0.10, AskDelta: 0.10, Active: false}, // skip
	}
	ents := customerSpreadsToEntries(cs)
	if len(ents) != 2 {
		t.Fatalf("len = %d, want 2 (inactive 제외)", len(ents))
	}
	// 전부 override 모드 (tier/HQ 무시).
	for _, e := range ents {
		if e.Mode != "override" {
			t.Errorf("%s mode = %q, want override", e.CustomerID, e.Mode)
		}
	}
	if ents[0].CustomerID != "alice01" || ents[0].Pair != session.Pair("USD/KRW") ||
		ents[0].BidDelta != 0.15 || ents[0].AskDelta != 0.20 {
		t.Errorf("alice01 매핑: %+v", ents[0])
	}
}

func TestFileBackend_LoadCustomerSpreads(t *testing.T) {
	dir := t.TempDir()
	js := `[
	  {"usid":"alice01","pair":"USD/KRW","bid_delta":0.15,"ask_delta":0.20,"active":true},
	  {"usid":"bob02","pair":"EUR/KRW","bid_delta":0.30,"ask_delta":0.30,"active":true}
	]`
	if err := os.WriteFile(filepath.Join(dir, "customer_margin.json"), []byte(js), 0o644); err != nil {
		t.Fatal(err)
	}
	b := NewFileBackend(dir)
	cs, err := b.LoadCustomerSpreads(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 {
		t.Fatalf("len = %d, want 2", len(cs))
	}
	if cs[0].Usid != "alice01" || cs[0].BidDelta != 0.15 || cs[0].Pair != "USD/KRW" {
		t.Errorf("alice01: %+v", cs[0])
	}
}

func TestFileBackend_LoadCustomerSpreads_Missing(t *testing.T) {
	b := NewFileBackend(t.TempDir())
	cs, err := b.LoadCustomerSpreads(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 0 {
		t.Errorf("누락 파일인데 len = %d", len(cs))
	}
}

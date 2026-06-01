package fxsync

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestFileBackend_LoadCurrencies_Basic(t *testing.T) {
	dir := t.TempDir()
	body := `[
	  {"code":"USD","name":"미국 달러","decimal_places":4,"active":true},
	  {"code":"KRW","name":"한국 원","decimal_places":2,"active":true},
	  {"code":"HKD","name":"홍콩 달러","decimal_places":4,"active":false}
	]`
	if err := os.WriteFile(filepath.Join(dir, "currency.json"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	b := NewFileBackend(dir)
	got, err := b.LoadCurrencies(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	if got[0].Code != "USD" || got[0].DecimalPlaces != 4 {
		t.Errorf("first = %+v", got[0])
	}
	// Active=false 도 backend 단계에선 그대로 반환 (Syncer 가 필터).
	if got[2].Code != "HKD" || got[2].Active {
		t.Errorf("HKD entry: %+v", got[2])
	}
}

func TestFileBackend_LoadCurrencies_MissingFile(t *testing.T) {
	b := NewFileBackend(t.TempDir()) // empty dir
	got, err := b.LoadCurrencies(context.Background())
	if err != nil {
		t.Errorf("missing file should not error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("missing file should return empty: %d", len(got))
	}
}

func TestFileBackend_LoadCurrencies_Malformed(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "currency.json"), []byte("not json"), 0644)
	b := NewFileBackend(dir)
	_, err := b.LoadCurrencies(context.Background())
	if err == nil {
		t.Error("malformed JSON should error")
	}
}

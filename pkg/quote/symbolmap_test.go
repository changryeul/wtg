package quote

import (
	"sync"
	"testing"

	"github.com/winwaysystems/wtg/pkg/session"
)

func TestSymbolMap_LookupBasic(t *testing.T) {
	m := NewSymbolMap()
	m.Replace([]SymbolEntry{
		{Symbol: "USDKRW", Pair: "USD/KRW", Active: true},
		{Symbol: "EURKRW", Pair: "EUR/KRW", Active: true},
		{Symbol: "JPYKRW", Pair: "JPY/KRW", Active: false}, // 정지
	})

	if p, active, found := m.Lookup("USDKRW"); !found || !active || p != "USD/KRW" {
		t.Errorf("USDKRW lookup: pair=%q active=%v found=%v", p, active, found)
	}
	if p, active, found := m.Lookup("JPYKRW"); !found || active || p != "JPY/KRW" {
		t.Errorf("JPYKRW (inactive): pair=%q active=%v found=%v", p, active, found)
	}
	if _, _, found := m.Lookup("XAUUSD"); found {
		t.Error("미등록 심볼이 found=true")
	}
}

func TestSymbolMap_Reverse(t *testing.T) {
	m := NewSymbolMap()
	m.Replace([]SymbolEntry{
		{Symbol: "USDKRW", Pair: "USD/KRW", Active: true},
	})
	if s, ok := m.Reverse("USD/KRW"); !ok || s != "USDKRW" {
		t.Errorf("reverse: symbol=%q ok=%v", s, ok)
	}
	if _, ok := m.Reverse("EUR/KRW"); ok {
		t.Error("미등록 pair 가 reverse 에서 found=true")
	}
}

func TestSymbolMap_ReplaceIsAtomic(t *testing.T) {
	m := NewSymbolMap()

	// v1
	m.Replace([]SymbolEntry{{Symbol: "A", Pair: "P/A", Active: true}})
	if m.Size() != 1 {
		t.Errorf("v1 size = %d, want 1", m.Size())
	}

	// v2: 통째 교체 (A 사라지고 B 추가)
	m.Replace([]SymbolEntry{{Symbol: "B", Pair: "P/B", Active: true}})
	if _, _, found := m.Lookup("A"); found {
		t.Error("Replace 후 이전 entry 가 남아있음")
	}
	if _, active, found := m.Lookup("B"); !found || !active {
		t.Error("Replace 후 새 entry 가 없음")
	}
	if m.Size() != 1 {
		t.Errorf("v2 size = %d, want 1", m.Size())
	}
}

func TestSymbolMap_NilSafe(t *testing.T) {
	// p 가 빈 상태라도 panic 안 나야 함 (zero value)
	var m SymbolMap
	if _, _, found := m.Lookup("X"); found {
		t.Error("zero value Lookup 이 found=true")
	}
	if _, ok := m.Reverse("P/X"); ok {
		t.Error("zero value Reverse 가 ok=true")
	}
	if m.Size() != 0 {
		t.Errorf("zero value Size = %d", m.Size())
	}
	if m.All() != nil {
		t.Errorf("zero value All = %v", m.All())
	}
}

func TestSymbolMap_All(t *testing.T) {
	m := NewSymbolMap()
	want := []SymbolEntry{
		{Symbol: "USDKRW", Pair: "USD/KRW", Active: true},
		{Symbol: "EURKRW", Pair: "EUR/KRW", Active: false},
	}
	m.Replace(want)
	got := m.All()
	if len(got) != len(want) {
		t.Fatalf("All len = %d, want %d", len(got), len(want))
	}
	// 순서 보장 X — set 비교
	gotMap := map[string]SymbolEntry{}
	for _, e := range got {
		gotMap[e.Symbol] = e
	}
	for _, e := range want {
		if got, ok := gotMap[e.Symbol]; !ok || got != e {
			t.Errorf("missing/mismatch entry %+v", e)
		}
	}
}

// 동시 Replace 와 Lookup 이 race 없이 동작 (go test -race).
func TestSymbolMap_ConcurrentReplaceAndLookup(t *testing.T) {
	m := NewSymbolMap()
	entries := []SymbolEntry{
		{Symbol: "USDKRW", Pair: "USD/KRW", Active: true},
		{Symbol: "EURKRW", Pair: "EUR/KRW", Active: true},
	}
	m.Replace(entries)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// readers
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_, _, _ = m.Lookup("USDKRW")
					_, _ = m.Reverse("EUR/KRW")
				}
			}
		}()
	}

	// writer
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			active := i%2 == 0
			m.Replace([]SymbolEntry{
				{Symbol: "USDKRW", Pair: "USD/KRW", Active: active},
				{Symbol: "EURKRW", Pair: "EUR/KRW", Active: !active},
			})
		}
		close(stop)
	}()

	// optional safety: 이 채널을 wg 가 처리 못하면 timeout 으로 보호하고 싶지만,
	// race detector 가 race 를 잡는 것이 목적이므로 그냥 끝까지 기다린다.
	_ = session.Pair("")  // session import 사용 표시
	wg.Wait()
}

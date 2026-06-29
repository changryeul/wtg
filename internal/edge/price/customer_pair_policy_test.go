package price

import (
	"reflect"
	"testing"
)

func TestMemoryCustomerPairPolicy_Empty(t *testing.T) {
	p := NewMemoryCustomerPairPolicy()
	allowed, ok := p.AllowedFor("alice")
	if ok {
		t.Errorf("등록 안 된 customer 가 ok=true — backward compat 깨짐 (got %v)", allowed)
	}
	if allowed != nil {
		t.Errorf("등록 안 된 customer 의 allowed != nil (got %v)", allowed)
	}
}

func TestMemoryCustomerPairPolicy_SetThenAllowedFor(t *testing.T) {
	p := NewMemoryCustomerPairPolicy()
	p.Set("alice", []string{"USD/KRW", "EUR/USD"})
	allowed, ok := p.AllowedFor("alice")
	if !ok {
		t.Fatalf("alice 등록 후 ok=false")
	}
	want := []string{"EUR/USD", "USD/KRW"} // sorted
	if !reflect.DeepEqual(allowed, want) {
		t.Errorf("allowed = %v, want %v (sorted 보장)", allowed, want)
	}
}

func TestMemoryCustomerPairPolicy_SetDeduplicates(t *testing.T) {
	p := NewMemoryCustomerPairPolicy()
	p.Set("alice", []string{"USD/KRW", "USD/KRW", "  ", "EUR/USD", "EUR/USD"})
	allowed, _ := p.AllowedFor("alice")
	want := []string{"EUR/USD", "USD/KRW"}
	if !reflect.DeepEqual(allowed, want) {
		t.Errorf("dedup/trim 후 allowed = %v, want %v", allowed, want)
	}
}

func TestMemoryCustomerPairPolicy_EmptyList_IsTotalBlock(t *testing.T) {
	p := NewMemoryCustomerPairPolicy()
	p.Set("bob", []string{})
	allowed, ok := p.AllowedFor("bob")
	if !ok {
		t.Errorf("빈 list 등록 후 ok=false — 등록은 됐어야")
	}
	if len(allowed) != 0 {
		t.Errorf("빈 list 등록 후 allowed = %v, want []", allowed)
	}
}

func TestMemoryCustomerPairPolicy_Delete(t *testing.T) {
	p := NewMemoryCustomerPairPolicy()
	p.Set("alice", []string{"USD/KRW"})
	p.Set("bob", []string{"EUR/USD"})
	p.Delete("alice")
	if _, ok := p.AllowedFor("alice"); ok {
		t.Errorf("Delete 후 alice 가 여전히 등록됨")
	}
	if _, ok := p.AllowedFor("bob"); !ok {
		t.Errorf("Delete 가 다른 customer 에 영향")
	}
}

func TestMemoryCustomerPairPolicy_Replace(t *testing.T) {
	p := NewMemoryCustomerPairPolicy()
	p.Set("alice", []string{"USD/KRW"})
	p.Replace(map[string][]string{
		"bob":     {"EUR/USD"},
		"charlie": {"JPY/KRW", "GBP/USD"},
	})
	if _, ok := p.AllowedFor("alice"); ok {
		t.Errorf("Replace 후 alice 가 남아있음 — 전체 교체 깨짐")
	}
	if got, ok := p.AllowedFor("charlie"); !ok || !reflect.DeepEqual(got, []string{"GBP/USD", "JPY/KRW"}) {
		t.Errorf("charlie = %v ok=%v, want sorted [GBP/USD JPY/KRW]", got, ok)
	}
}

func TestMemoryCustomerPairPolicy_Snapshot(t *testing.T) {
	p := NewMemoryCustomerPairPolicy()
	p.Set("alice", []string{"USD/KRW"})
	p.Set("bob", []string{"EUR/USD"})
	snap := p.Snapshot()
	if len(snap) != 2 {
		t.Errorf("snapshot size = %d, want 2", len(snap))
	}
	// snapshot 수정이 원본에 영향 안 줘야 (deep copy).
	snap["alice"][0] = "MUTATED"
	got, _ := p.AllowedFor("alice")
	if got[0] != "USD/KRW" {
		t.Errorf("snapshot 수정이 원본에 영향: got %v", got)
	}
}

// TestGateSubscribe_WithCustomerPolicy — gateSubscribe 가 customer policy 와
// 글로벌 정책을 올바르게 결합하는지. 4 케이스:
//
//  1. customer 미등록 → 글로벌만 (unrestricted intersection 효과)
//  2. customer 등록 + 글로벌 통과 + customer 통과 → accept
//  3. customer 등록 + 글로벌 통과 + customer 차단 → reject
//  4. 글로벌 차단 → customer 무관 reject (글로벌 우선)
func TestGateSubscribe_WithCustomerPolicy(t *testing.T) {
	gv := NewMemoryPairValidator()
	gv.Add("USD/KRW", "EUR/USD", "JPY/KRW") // 글로벌 허용
	cp := NewMemoryCustomerPairPolicy()
	cp.Set("alice", []string{"USD/KRW"}) // alice 는 USD/KRW 만

	s := &Server{
		pairValidator: gv,
		customerPairs: cp,
	}

	// case 1: customer 미등록 → 글로벌만 → 글로벌 허용 3개 모두 통과.
	subBob := &Subscriber{customerID: "bob"} // 미등록
	a, r := s.gateSubscribe(subBob, []string{"USD/KRW", "EUR/USD", "JPY/KRW"})
	if len(a) != 3 || len(r) != 0 {
		t.Errorf("미등록 bob: accept=%v reject=%v, want 모두 accept", a, r)
	}

	// case 2/3: alice — USD/KRW 만 통과, EUR/USD 는 customer 차단.
	subAlice := &Subscriber{customerID: "alice"}
	a, r = s.gateSubscribe(subAlice, []string{"USD/KRW", "EUR/USD"})
	if len(a) != 1 || a[0] != "USD/KRW" {
		t.Errorf("alice accept=%v, want [USD/KRW]", a)
	}
	if len(r) != 1 || r[0] != "EUR/USD" {
		t.Errorf("alice reject=%v, want [EUR/USD]", r)
	}

	// case 4: 글로벌 차단 pair (GBP/USD) → customer 무관 reject.
	a, r = s.gateSubscribe(subAlice, []string{"GBP/USD"})
	if len(a) != 0 || len(r) != 1 || r[0] != "GBP/USD" {
		t.Errorf("글로벌 차단: accept=%v reject=%v, want [] [GBP/USD]", a, r)
	}
}

// 빈 list 등록 = 전체 차단 의도.
func TestGateSubscribe_CustomerEmptyList_BlocksAll(t *testing.T) {
	gv := NewMemoryPairValidator()
	gv.Add("USD/KRW", "EUR/USD")
	cp := NewMemoryCustomerPairPolicy()
	cp.Set("blocked", []string{}) // 빈 list = 전체 차단

	s := &Server{pairValidator: gv, customerPairs: cp}
	subBlocked := &Subscriber{customerID: "blocked"}
	a, r := s.gateSubscribe(subBlocked, []string{"USD/KRW", "EUR/USD"})
	if len(a) != 0 {
		t.Errorf("빈 list 등록 customer accept=%v, want 모두 reject", a)
	}
	if len(r) != 2 {
		t.Errorf("reject=%v, want 2", r)
	}
}

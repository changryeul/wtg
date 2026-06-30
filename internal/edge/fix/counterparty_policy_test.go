package fix

import (
	"reflect"
	"testing"
)

func TestMemoryCounterpartyPolicy_Empty(t *testing.T) {
	p := NewMemoryCounterpartyPolicy()
	if _, ok := p.Lookup("alice"); ok {
		t.Errorf("미등록 lookup 이 ok=true")
	}
	if snap := p.Snapshot(); len(snap) != 0 {
		t.Errorf("Snapshot len=%d, want 0", len(snap))
	}
}

func TestMemoryCounterpartyPolicy_SetLookup(t *testing.T) {
	p := NewMemoryCounterpartyPolicy()
	cp := Counterparty{Password: "pw1", Channel: "FIX", Site: "HQ", Tier: "VIP", Usid: "ECN_A"}
	p.Set("CP_A", cp)
	got, ok := p.Lookup("CP_A")
	if !ok {
		t.Fatalf("Set 후 Lookup ok=false")
	}
	if !reflect.DeepEqual(got, cp) {
		t.Errorf("Lookup = %+v, want %+v", got, cp)
	}
}

func TestMemoryCounterpartyPolicy_Replace(t *testing.T) {
	p := NewMemoryCounterpartyPolicy()
	p.Set("OLD", Counterparty{Password: "x"})
	p.Replace(map[string]Counterparty{
		"NEW_A": {Password: "a"},
		"NEW_B": {Password: "b"},
	})
	if _, ok := p.Lookup("OLD"); ok {
		t.Errorf("Replace 후 OLD 가 남아있음")
	}
	if _, ok := p.Lookup("NEW_A"); !ok {
		t.Errorf("Replace 후 NEW_A 없음")
	}
}

func TestMemoryCounterpartyPolicy_Delete(t *testing.T) {
	p := NewMemoryCounterpartyPolicy()
	p.Set("A", Counterparty{Password: "a"})
	p.Set("B", Counterparty{Password: "b"})
	p.Delete("A")
	if _, ok := p.Lookup("A"); ok {
		t.Errorf("Delete 후 A lookup 가 ok=true")
	}
	if _, ok := p.Lookup("B"); !ok {
		t.Errorf("Delete 가 다른 entry 에 영향")
	}
}

func TestMemoryCounterpartyPolicy_SnapshotDeepCopy(t *testing.T) {
	p := NewMemoryCounterpartyPolicy()
	p.Set("CP", Counterparty{Password: "secret"})
	snap := p.Snapshot()
	// snapshot 의 entry 변경이 원본에 영향 안 줘야.
	if cp, ok := snap["CP"]; ok {
		cp.Password = "MUTATED"
		snap["CP"] = cp
	}
	got, _ := p.Lookup("CP")
	if got.Password != "secret" {
		t.Errorf("snapshot 수정이 원본에 영향: %v", got.Password)
	}
}

func TestStaticPolicy_Lookup(t *testing.T) {
	sp := &staticPolicy{m: map[string]Counterparty{
		"X": {Password: "p"},
	}}
	if got, ok := sp.Lookup("X"); !ok || got.Password != "p" {
		t.Errorf("staticPolicy Lookup 실패: %v / %v", got, ok)
	}
	if _, ok := sp.Lookup("Y"); ok {
		t.Errorf("staticPolicy 미등록 lookup ok=true")
	}
}

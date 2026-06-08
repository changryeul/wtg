package push

import (
	"fmt"
	"testing"
)

// TestHashRing_Sticky — 같은 user 반복 lookup 시 같은 인스턴스 반환.
func TestHashRing_Sticky(t *testing.T) {
	endpoints := []string{"http://push-1", "http://push-2", "http://push-3"}
	r := NewRing(endpoints, 100)
	if r == nil {
		t.Fatal("NewRing 결과 nil")
	}
	for _, u := range []string{"dealer01", "dealer02", "dealer03", "트레이더01"} {
		first := r.Lookup(u)
		for i := 0; i < 100; i++ {
			if got := r.Lookup(u); got != first {
				t.Errorf("user %q lookup 결정성 깨짐: first=%d, got[%d]=%d", u, first, i, got)
			}
		}
	}
}

// TestHashRing_Distribution — V=100 일 때 분배가 어느 정도 균등.
// 4 인스턴스 × 1000 user → 각 인스턴스 100~400 범위.
func TestHashRing_Distribution(t *testing.T) {
	endpoints := []string{"http://push-1", "http://push-2", "http://push-3", "http://push-4"}
	r := NewRing(endpoints, 100)
	counts := make([]int, 4)
	for i := 0; i < 1000; i++ {
		counts[r.Lookup(fmt.Sprintf("user-%d", i))]++
	}
	for i, c := range counts {
		if c < 100 || c > 400 {
			t.Errorf("인스턴스 %d 분배 count=%d (100~400 기대)", i, c)
		}
	}
}

// TestHashRing_StickyOnGrowth — 인스턴스 추가 (N → N+1) 시 sticky 유지율 측정.
// hash mod N 은 ~1/N 만 유지, ring 은 ~N/(N+1) (1000 user 중 ~800+) 유지 기대.
func TestHashRing_StickyOnGrowth(t *testing.T) {
	before := []string{"http://push-1", "http://push-2", "http://push-3", "http://push-4"}
	after := append([]string{}, before...)
	after = append(after, "http://push-5") // 1 인스턴스 추가.

	rBefore := NewRing(before, 100)
	rAfter := NewRing(after, 100)

	const total = 1000
	mappingMatch := 0
	modMatch := 0
	for i := 0; i < total; i++ {
		u := fmt.Sprintf("user-%d", i)
		if rBefore.Lookup(u) == rAfter.Lookup(u) {
			mappingMatch++
		}
		if userIndex(u, len(before)) == userIndex(u, len(after)) {
			modMatch++
		}
	}
	t.Logf("ring sticky on N=4→5: %d/%d (%.1f%%)", mappingMatch, total, 100*float64(mappingMatch)/total)
	t.Logf("mod  sticky on N=4→5: %d/%d (%.1f%%)", modMatch, total, 100*float64(modMatch)/total)

	// ring 은 이론상 N/(N+1) = 80% sticky 유지 기대 (실제는 75~85%).
	// 분배 변동성 고려해 70% 임계로 검증.
	if mappingMatch < 700 {
		t.Errorf("ring sticky 유지율 부족: %d/%d (기대 ≥70%%)", mappingMatch, total)
	}
	// ring 은 mod 보다 sticky 유지율이 항상 높아야.
	if mappingMatch <= modMatch {
		t.Errorf("ring sticky(%d) ≤ mod sticky(%d) — ring 이점 무의미", mappingMatch, modMatch)
	}
}

// TestHashRing_StickyOnRemoval — 인스턴스 제거 (N → N-1) 시 sticky 유지율.
// 제거된 인스턴스에 매핑되었던 사용자만 재배치 — 약 (N-1)/N 유지.
func TestHashRing_StickyOnRemoval(t *testing.T) {
	before := []string{"http://push-1", "http://push-2", "http://push-3", "http://push-4"}
	after := []string{"http://push-1", "http://push-2", "http://push-3"} // push-4 제거.

	rBefore := NewRing(before, 100)
	rAfter := NewRing(after, 100)

	const total = 1000
	match := 0
	for i := 0; i < total; i++ {
		u := fmt.Sprintf("user-%d", i)
		// before 의 인스턴스 idx → URL 매핑 후, after 에서 같은 URL 매핑 비교.
		// (단순 idx 비교는 idx 시프트로 부정확)
		beforeURL := before[rBefore.Lookup(u)]
		afterURL := after[rAfter.Lookup(u)]
		if beforeURL == afterURL {
			match++
		}
	}
	t.Logf("ring sticky on N=4→3 (push-4 removed): %d/%d (%.1f%%)", match, total, 100*float64(match)/total)
	if match < 650 {
		t.Errorf("ring sticky 유지율 부족: %d/%d (기대 ≥65%%)", match, total)
	}
}

// TestHashRing_VNodeImpact — V (vnode 수) 가 분배 균등성에 미치는 영향.
// V 가 클수록 분배가 더 균등.
func TestHashRing_VNodeImpact(t *testing.T) {
	endpoints := []string{"http://push-1", "http://push-2", "http://push-3", "http://push-4"}
	for _, v := range []int{10, 50, 100, 200} {
		r := NewRing(endpoints, v)
		counts := make([]int, 4)
		for i := 0; i < 1000; i++ {
			counts[r.Lookup(fmt.Sprintf("user-%d", i))]++
		}
		// 표준편차 측정 — V 가 클수록 줄어야 정상 (단위 test 에선 hard assert X, log only).
		min, max := counts[0], counts[0]
		for _, c := range counts[1:] {
			if c < min {
				min = c
			}
			if c > max {
				max = c
			}
		}
		t.Logf("V=%d 분배: %v (min=%d, max=%d, spread=%d)", v, counts, min, max, max-min)
	}
}

// TestHashRing_Empty — endpoints 빈 시 nil 반환.
func TestHashRing_Empty(t *testing.T) {
	if r := NewRing(nil, 100); r != nil {
		t.Errorf("endpoints nil 시 NewRing → nil 기대, got %v", r)
	}
	if r := NewRing([]string{}, 100); r != nil {
		t.Errorf("endpoints 빈 시 NewRing → nil 기대, got %v", r)
	}
}

// TestHashRing_NilSafe — nil ring 의 Lookup 도 panic X.
func TestHashRing_NilSafe(t *testing.T) {
	var r *HashRing
	if got := r.Lookup("any"); got != 0 {
		t.Errorf("nil ring Lookup → 0 기대, got %d", got)
	}
	if got := r.Endpoints(); got != nil {
		t.Errorf("nil ring Endpoints → nil 기대, got %v", got)
	}
	if got := r.VNodeCount(); got != 0 {
		t.Errorf("nil ring VNodeCount → 0 기대, got %d", got)
	}
}

// TestHashRing_DefaultVNodes — V<=0 입력 시 default 100.
func TestHashRing_DefaultVNodes(t *testing.T) {
	r := NewRing([]string{"http://push-1", "http://push-2"}, 0)
	if r.VNodeCount() != 200 {
		t.Errorf("V=0 → default 100 × 2 endpoint = 200 v-node 기대, got %d", r.VNodeCount())
	}
	r = NewRing([]string{"http://push-1", "http://push-2"}, -5)
	if r.VNodeCount() != 200 {
		t.Errorf("V=-5 → default 100 × 2 = 200 v-node 기대, got %d", r.VNodeCount())
	}
}

package main

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// classifyInvalid 의 5 reason 분류. 호출 후 반환 string + atomic 카운터 둘 다
// 검증. 카운터 reset 을 위해 sub-test 순서대로.
func TestClassifyInvalid_Reasons(t *testing.T) {
	cases := []struct {
		name      string
		sym       string
		bid, ask  float64
		wantReason string
	}{
		{"missing_symbol", "", 156.4, 156.5, "missing_symbol"},
		{"not_a_quote (trade-like)", "USDJPY", 0, 0, "not_a_quote"},
		{"non_positive_bid", "USDJPY", -4.66, 156.5, "non_positive_price"},
		{"non_positive_ask_only", "USDJPY", 156.4, 0, "non_positive_price"}, // ask==0 만 — not_a_quote(둘 다 0) 안 잡히고 bid<=0||ask<=0 분기
		{"crossed_spread", "USDJPY", 156.5, 156.0, "crossed_spread"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyInvalid(tc.sym, tc.bid, tc.ask)
			if got != tc.wantReason {
				t.Errorf("classifyInvalid(%q, %v, %v) = %q, want %q",
					tc.sym, tc.bid, tc.ask, got, tc.wantReason)
			}
		})
	}
}

// classifyInvalid 가 분기 선후 따라 정확히 한 카운터만 증가하는지.
// (테스트가 다른 test 와 같은 atomic 공유 — 직전 누계를 baseline 으로 비교.)
func TestClassifyInvalid_CounterDelta(t *testing.T) {
	t.Run("non_positive_price 만 1 증가", func(t *testing.T) {
		before := totalInvalidNonPositive.Load()
		beforeNAQ := totalInvalidNotAQuote.Load()
		_ = classifyInvalid("USDJPY", -1.0, 1.0)
		if got := totalInvalidNonPositive.Load() - before; got != 1 {
			t.Errorf("non_positive delta = %d, want 1", got)
		}
		if got := totalInvalidNotAQuote.Load() - beforeNAQ; got != 0 {
			t.Errorf("not_a_quote 카운터가 잘못 증가 = %d", got)
		}
	})
}

// shouldLogRejectSample 의 rate-limit + 동시성.
func TestShouldLogRejectSample_RateLimit(t *testing.T) {
	// key 충돌 회피 — 테스트 격리.
	key := "TEST_RATE:non_positive_price"
	now := time.Now()
	// 1) 첫 호출은 true (즉시 로그).
	if !shouldLogRejectSample(key, now) {
		t.Fatal("첫 호출은 true 여야 — 첫 등장")
	}
	// 2) 같은 시각 직후는 false (60초 rate limit).
	if shouldLogRejectSample(key, now) {
		t.Error("같은 시각 두 번째 호출은 false 여야 — rate limit")
	}
	// 3) 50초 지난 시점도 false (60초 이내).
	if shouldLogRejectSample(key, now.Add(50*time.Second)) {
		t.Error("50초 후 호출도 false 여야 — rate limit 미만")
	}
	// 4) 61초 후는 true (interval 초과).
	if !shouldLogRejectSample(key, now.Add(61*time.Second)) {
		t.Error("61초 후 호출은 true 여야 — interval 초과")
	}
}

// 서로 다른 key 는 독립 — 한 쪽이 로그됐다고 다른 쪽 rate limit 영향 X.
func TestShouldLogRejectSample_IsolatedKeys(t *testing.T) {
	now := time.Now()
	a := "TEST_ISO:reason_a"
	b := "TEST_ISO:reason_b"
	_ = shouldLogRejectSample(a, now)
	if !shouldLogRejectSample(b, now) {
		t.Error("다른 key 는 즉시 로그돼야 함 — key isolation 실패")
	}
}

// 동시성 — 같은 key 에 동시에 다수 goroutine 진입해도 첫 호출 1개만 true.
func TestShouldLogRejectSample_ConcurrentCAS(t *testing.T) {
	key := "TEST_CONC:" + strings.Repeat("x", 8)
	now := time.Now().Add(120 * time.Second) // 다른 테스트 잔여 영향 회피
	const goroutines = 50
	var wg sync.WaitGroup
	var trueCount int64
	var mu sync.Mutex
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			if shouldLogRejectSample(key, now) {
				mu.Lock()
				trueCount++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if trueCount != 1 {
		t.Errorf("동시 %d goroutine 중 true 반환 = %d, want 1 (CAS 보호 실패)", goroutines, trueCount)
	}
}

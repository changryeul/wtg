package price

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// 임계 (80%) 미만에서는 WARN 미발생. testLogger 가 backpressure 키워드를
// 잡지 못해야 함.
func TestCheckBackpressure_BelowThreshold(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	// 50% — threshold 80% 미만.
	checkBackpressure(logger, 512, 1024, 0xBP01, "edge-A", "tick")
	if strings.Contains(buf.String(), "backpressure 감지") {
		t.Errorf("80%% 미만에서 WARN 발생: %s", buf.String())
	}
}

// 80% 도달 시 WARN 발생.
func TestCheckBackpressure_AtThreshold(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	// 정확히 80% (819/1024 = 79.98% 는 미만이므로 820/1024 = 80.07% 사용).
	checkBackpressure(logger, 820, 1024, 0xBP02, "edge-B", "quote")
	if !strings.Contains(buf.String(), "backpressure 감지") {
		t.Errorf("80%% 도달인데 WARN 없음: %s", buf.String())
	}
}

// 같은 (sub, kind) 키 즉시 두 번째 호출은 silent — rate limit.
func TestCheckBackpressure_RateLimit(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	checkBackpressure(logger, 900, 1024, 0xBP03, "edge-C", "bar")
	first := strings.Count(buf.String(), "backpressure 감지")
	checkBackpressure(logger, 900, 1024, 0xBP03, "edge-C", "bar")
	checkBackpressure(logger, 900, 1024, 0xBP03, "edge-C", "bar")
	second := strings.Count(buf.String(), "backpressure 감지")
	if first != 1 || second != 1 {
		t.Errorf("rate limit 실패: first=%d second=%d (want 1, 1)", first, second)
	}
}

// 다른 (sub, kind) 키는 독립적으로 WARN.
func TestCheckBackpressure_IsolatedKeys(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	checkBackpressure(logger, 900, 1024, 0xBP04, "edge-D", "tick")
	checkBackpressure(logger, 900, 1024, 0xBP04, "edge-D", "quote") // 다른 kind
	checkBackpressure(logger, 900, 1024, 0xBP05, "edge-E", "tick")  // 다른 sub_id
	if got := strings.Count(buf.String(), "backpressure 감지"); got != 3 {
		t.Errorf("독립 키 3개 모두 WARN 기대 = 3, got = %d", got)
	}
}

// 0 capacity 는 무시 (division by zero 회피).
func TestCheckBackpressure_ZeroCap(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	checkBackpressure(logger, 0, 0, 0xBP06, "edge-F", "ws")
	if strings.Contains(buf.String(), "backpressure 감지") {
		t.Errorf("0 cap 에서 WARN 발생: %s", buf.String())
	}
}

// 동시성 — 같은 키에 다수 goroutine 진입해도 WARN 한 번.
func TestCheckBackpressure_ConcurrentCAS(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	const goroutines = 30
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			checkBackpressure(logger, 900, 1024, 0xBP07, "edge-G", "tick")
		}()
	}
	wg.Wait()
	if got := strings.Count(buf.String(), "backpressure 감지"); got != 1 {
		t.Errorf("동시 %d 호출 중 WARN = %d, want 1 (CAS 보호 실패)", goroutines, got)
	}
}

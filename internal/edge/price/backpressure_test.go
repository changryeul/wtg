package price

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// edge-price 의 checkBackpressure — mci-price 와 같은 패턴이지만 label
// field 가 srv_id 가 아니라 profile.

func TestCheckBackpressure_BelowThreshold(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	checkBackpressure(logger, 128, 256, 0xEBP01, "WEB.BRANCH.VIP", "ws")
	if strings.Contains(buf.String(), "backpressure 감지") {
		t.Errorf("50%% 인데 WARN 발생: %s", buf.String())
	}
}

func TestCheckBackpressure_AtThreshold(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	// 256 의 80% = 204.8 → 205 사용.
	checkBackpressure(logger, 205, 256, 0xEBP02, "WEB.BRANCH.STD", "ws")
	if !strings.Contains(buf.String(), "backpressure 감지") {
		t.Errorf("80%% 도달인데 WARN 없음: %s", buf.String())
	}
	if !strings.Contains(buf.String(), `"profile":"WEB.BRANCH.STD"`) {
		t.Errorf("profile label 누락: %s", buf.String())
	}
}

func TestCheckBackpressure_RateLimit(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	checkBackpressure(logger, 240, 256, 0xEBP03, "MOB.HQ.STD", "ws")
	checkBackpressure(logger, 240, 256, 0xEBP03, "MOB.HQ.STD", "ws")
	if got := strings.Count(buf.String(), "backpressure 감지"); got != 1 {
		t.Errorf("rate limit 실패: WARN = %d, want 1", got)
	}
}

func TestCheckBackpressure_ConcurrentCAS(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	const goroutines = 30
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			checkBackpressure(logger, 220, 256, 0xEBP04, "CS.HQ.VIP", "ws")
		}()
	}
	wg.Wait()
	if got := strings.Count(buf.String(), "backpressure 감지"); got != 1 {
		t.Errorf("동시 %d 호출 중 WARN = %d, want 1", goroutines, got)
	}
}

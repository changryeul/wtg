package routing

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newWrapped(t *testing.T) (Registry, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "routes.json")
	raw := NewInMemoryRegistry(time.Now)
	wrapped := WrapWithFileWriteback(raw, path, quietLogger())
	return wrapped, path
}

func readRoutes(t *testing.T, path string) []Rule {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc struct {
		Routes []Rule `json:"routes"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return doc.Routes
}

func TestWrap_NoopOnEmptyPath(t *testing.T) {
	raw := NewInMemoryRegistry(time.Now)
	wrapped := WrapWithFileWriteback(raw, "", quietLogger())
	if wrapped != raw {
		t.Errorf("path 빈 값이면 same Registry 반환해야 — wrap=%T raw=%T", wrapped, raw)
	}
}

func TestPut_FlushesToFile(t *testing.T) {
	reg, path := newWrapped(t)
	rule := &Rule{Alias: "FOO_BAR", Exchange: "FOO", RoutingKey: "BAR", Active: true}
	if err := reg.Put(rule, "tester"); err != nil {
		t.Fatal(err)
	}
	rules := readRoutes(t, path)
	if len(rules) != 1 {
		t.Fatalf("file rules=%d want 1", len(rules))
	}
	if rules[0].Alias != "FOO_BAR" || rules[0].Exchange != "FOO" {
		t.Errorf("rule[0]=%+v", rules[0])
	}
}

func TestDelete_RemovesFromFile(t *testing.T) {
	reg, path := newWrapped(t)
	_ = reg.Put(&Rule{Alias: "A", Exchange: "X", RoutingKey: "K1", Active: true}, "tester")
	_ = reg.Put(&Rule{Alias: "B", Exchange: "X", RoutingKey: "K2", Active: true}, "tester")
	if err := reg.Delete("A"); err != nil {
		t.Fatal(err)
	}
	rules := readRoutes(t, path)
	if len(rules) != 1 {
		t.Fatalf("file rules=%d want 1 (A 삭제 후)", len(rules))
	}
	if rules[0].Alias != "B" {
		t.Errorf("남은 alias=%s want B", rules[0].Alias)
	}
}

func TestSetActive_PersistsInFile(t *testing.T) {
	reg, path := newWrapped(t)
	_ = reg.Put(&Rule{Alias: "A", Exchange: "X", RoutingKey: "K", Active: true}, "tester")
	if err := reg.SetActive("A", false, "tester"); err != nil {
		t.Fatal(err)
	}
	rules := readRoutes(t, path)
	if rules[0].Active != false {
		t.Errorf("Active=%v want false", rules[0].Active)
	}
}

func TestPut_AtomicWrite_NoCorruptionUnderFailure(t *testing.T) {
	reg, path := newWrapped(t)
	// 5개 alias 순차 추가 — 매번 file 이 valid JSON 인지 확인.
	for _, a := range []string{"A", "B", "C", "D", "E"} {
		_ = reg.Put(&Rule{Alias: a, Exchange: "X", RoutingKey: "K", Active: true}, "tester")
		_ = readRoutes(t, path) // unmarshal 가 실패하면 t.Fatal — atomic 깨졌다는 뜻
	}
	rules := readRoutes(t, path)
	if len(rules) != 5 {
		t.Errorf("file rules=%d want 5", len(rules))
	}
}

func TestConcurrent_PutsSerializedByMutex(t *testing.T) {
	reg, path := newWrapped(t)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			r := &Rule{
				Alias:      "A" + string(rune('0'+n%10)) + string(rune('0'+(n/10)%10)),
				Exchange:   "X",
				RoutingKey: "K",
				Active:     true,
			}
			_ = reg.Put(r, "tester")
		}(i)
	}
	wg.Wait()
	// file 이 정상 JSON 이고 모든 alias 가 들어있어야 (concurrent put 직렬화 OK)
	rules := readRoutes(t, path)
	if len(rules) == 0 {
		t.Fatal("file empty after concurrent puts")
	}
	// in-memory 와 file 의 rule 수가 같아야 (실패 = race condition)
	if len(rules) != len(reg.List()) {
		t.Errorf("file=%d in-memory=%d", len(rules), len(reg.List()))
	}
}

// File 이 dev_seed.LoadRoutesFromFile 로 다시 로드 가능해야 (round-trip).
func TestFlush_RoundTripsThroughLoadRoutesFromFile(t *testing.T) {
	reg, path := newWrapped(t)
	_ = reg.Put(&Rule{Alias: "ROUND", Exchange: "X", RoutingKey: "TRIP", Active: true,
		Comment: "round-trip test"}, "tester")

	loaded, err := LoadRoutesFromFile(path)
	if err != nil {
		t.Fatalf("LoadRoutesFromFile: %v", err)
	}
	if len(loaded) != 1 || loaded[0].Alias != "ROUND" || loaded[0].Comment != "round-trip test" {
		t.Errorf("loaded=%+v", loaded)
	}
}

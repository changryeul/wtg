//go:build integration

package quoteid

import (
	"context"
	"sync"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/winwaysystems/wtg/test/etcdtest"
)

func newEtcdClient(t *testing.T) *clientv3.Client {
	t.Helper()
	srv := etcdtest.Start(t)
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{srv.ClientURL},
		DialTimeout: 3 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

// applySink — onApply callback 결과를 mutex 로 캡처.
type applySink struct {
	mu       sync.Mutex
	snapshot map[string]EngineMeta
	calls    int
}

func (a *applySink) Apply(m map[string]EngineMeta) {
	a.mu.Lock()
	defer a.mu.Unlock()
	cp := make(map[string]EngineMeta, len(m))
	for k, v := range m {
		cp[k] = v
	}
	a.snapshot = cp
	a.calls++
}

func (a *applySink) Snapshot() map[string]EngineMeta {
	a.mu.Lock()
	defer a.mu.Unlock()
	cp := make(map[string]EngineMeta, len(a.snapshot))
	for k, v := range a.snapshot {
		cp[k] = v
	}
	return cp
}

func (a *applySink) Calls() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

// waitForCalls — onApply 가 want 번 이상 호출될 때까지 대기 (timeout).
func waitForCalls(t *testing.T, sink *applySink, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if sink.Calls() >= want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("waitForCalls: got %d, want ≥%d", sink.Calls(), want)
}

func TestEtcdAllowlistWatcher_InitialLoadEmpty(t *testing.T) {
	cli := newEtcdClient(t)
	sink := &applySink{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	w, err := NewEtcdAllowlistWatcher(ctx, EtcdAllowlistWatcherOptions{
		Client:  cli,
		Prefix:  "test/quoteid/engines/",
		OnApply: sink.Apply,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if got := sink.Snapshot(); len(got) != 0 {
		t.Errorf("초기 snapshot 비어야 함, got %v", got)
	}
	if sink.Calls() != 1 {
		t.Errorf("calls=%d, want 1 (initial load)", sink.Calls())
	}
}

func TestEtcdAllowlistWatcher_PutAddsEngine(t *testing.T) {
	cli := newEtcdClient(t)
	sink := &applySink{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	prefix := "test/quoteid/engines/"
	w, err := NewEtcdAllowlistWatcher(ctx, EtcdAllowlistWatcherOptions{
		Client:  cli,
		Prefix:  prefix,
		OnApply: sink.Apply,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	// engine-A 추가.
	if _, err := cli.Put(ctx, prefix+"engine-A", ""); err != nil {
		t.Fatal(err)
	}
	waitForCalls(t, sink, 2, 2*time.Second)
	if _, ok := sink.Snapshot()["engine-A"]; !ok {
		t.Errorf("engine-A 누락: %v", sink.Snapshot())
	}

	// engine-B 추가.
	if _, err := cli.Put(ctx, prefix+"engine-B", ""); err != nil {
		t.Fatal(err)
	}
	waitForCalls(t, sink, 3, 2*time.Second)
	snap := sink.Snapshot()
	if _, ok := snap["engine-A"]; !ok {
		t.Errorf("A 누락: %v", snap)
	}
	if _, ok := snap["engine-B"]; !ok {
		t.Errorf("B 누락: %v", snap)
	}
}

func TestEtcdAllowlistWatcher_DeleteRemovesEngine(t *testing.T) {
	cli := newEtcdClient(t)
	sink := &applySink{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	prefix := "test/quoteid/engines/"
	// 사전 등록.
	if _, err := cli.Put(ctx, prefix+"engine-A", ""); err != nil {
		t.Fatal(err)
	}

	w, err := NewEtcdAllowlistWatcher(ctx, EtcdAllowlistWatcherOptions{
		Client:  cli,
		Prefix:  prefix,
		OnApply: sink.Apply,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, ok := sink.Snapshot()["engine-A"]; !ok {
		t.Errorf("초기 로드 engine-A 누락")
	}

	// 삭제.
	if _, err := cli.Delete(ctx, prefix+"engine-A"); err != nil {
		t.Fatal(err)
	}
	waitForCalls(t, sink, 2, 2*time.Second)
	if _, ok := sink.Snapshot()["engine-A"]; ok {
		t.Errorf("삭제 후에도 engine-A 남음: %v", sink.Snapshot())
	}
}

func TestEtcdAllowlistWatcher_EngineMetaJSON(t *testing.T) {
	cli := newEtcdClient(t)
	sink := &applySink{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	prefix := "test/quoteid/engines/"
	// engine-A — 풀 권한 (빈 value), engine-B — read-only + contact.
	if _, err := cli.Put(ctx, prefix+"engine-A", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := cli.Put(ctx, prefix+"engine-B",
		`{"permissions":["validate"],"contact":"audit@bank.com","expires_at":"2099-01-01T00:00:00Z"}`); err != nil {
		t.Fatal(err)
	}

	w, err := NewEtcdAllowlistWatcher(ctx, EtcdAllowlistWatcherOptions{
		Client:  cli,
		Prefix:  prefix,
		OnApply: sink.Apply,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	snap := sink.Snapshot()
	// engine-A: default meta (empty value).
	a, ok := snap["engine-A"]
	if !ok {
		t.Fatal("engine-A 누락")
	}
	if len(a.Permissions) != 0 {
		t.Errorf("engine-A default 풀 권한 기대, got %v", a.Permissions)
	}

	// engine-B: read-only + meta.
	b, ok := snap["engine-B"]
	if !ok {
		t.Fatal("engine-B 누락")
	}
	if len(b.Permissions) != 1 || b.Permissions[0] != "validate" {
		t.Errorf("engine-B permissions=%v, want [validate]", b.Permissions)
	}
	if b.Contact != "audit@bank.com" {
		t.Errorf("engine-B contact=%q", b.Contact)
	}
	if b.ExpiresAt != "2099-01-01T00:00:00Z" {
		t.Errorf("engine-B expires_at=%q", b.ExpiresAt)
	}
}

func TestEtcdAllowlistWatcher_InvalidJSONFallsBack(t *testing.T) {
	cli := newEtcdClient(t)
	sink := &applySink{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	prefix := "test/quoteid/engines/"
	// 잘못된 JSON 으로 시작 — 파싱 실패해도 engine 자체는 등록 + default meta.
	if _, err := cli.Put(ctx, prefix+"engine-X", `{"permissions": not-quoted}`); err != nil {
		t.Fatal(err)
	}

	w, err := NewEtcdAllowlistWatcher(ctx, EtcdAllowlistWatcherOptions{
		Client:  cli,
		Prefix:  prefix,
		OnApply: sink.Apply,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	snap := sink.Snapshot()
	if _, ok := snap["engine-X"]; !ok {
		t.Errorf("잘못된 JSON 이라도 engine 등록되어야 함 (default meta): %v", snap)
	}
}

func TestEtcdAllowlistWatcher_InitialLoadSeed(t *testing.T) {
	cli := newEtcdClient(t)
	sink := &applySink{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	prefix := "test/quoteid/engines/"
	// 사전 seed 3건.
	for _, eng := range []string{"engine-A", "engine-B", "engine-C"} {
		if _, err := cli.Put(ctx, prefix+eng, ""); err != nil {
			t.Fatal(err)
		}
	}

	w, err := NewEtcdAllowlistWatcher(ctx, EtcdAllowlistWatcherOptions{
		Client:  cli,
		Prefix:  prefix,
		OnApply: sink.Apply,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	snap := sink.Snapshot()
	if len(snap) != 3 {
		t.Errorf("초기 로드 count=%d, want 3: %v", len(snap), snap)
	}
}

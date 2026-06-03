//go:build integration

package ratelimit

import (
	"context"
	"io"
	"log/slog"
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

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// 사전 PUT 된 PolicyDoc 으로 RuleSet 이 빌드되는지.
func TestEtcdWatcher_InitialLoadFromKey(t *testing.T) {
	cli := newEtcdClient(t)
	rs, _ := NewRuleSet(nil, nil)
	defer rs.Stop()

	key := "test/ratelimit/edge-api"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	body := `{
		"version": 7,
		"rules": [
			{"pattern": "POST /v1/tx", "rate": 1, "burst": 1},
			{"pattern": "GET /v1/ping", "rate": 5, "burst": 5}
		]
	}`
	if _, err := cli.Put(ctx, key, body); err != nil {
		t.Fatal(err)
	}

	w, err := NewEtcdWatcher(ctx, EtcdWatcherOptions{
		Client: cli, Key: key, RuleSet: rs, Logger: quietLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	rules := rs.Rules()
	if len(rules) != 2 {
		t.Fatalf("rules len=%d, want 2: %+v", len(rules), rules)
	}
	if rules[0].Pattern != "POST /v1/tx" || rules[0].Burst != 1 {
		t.Errorf("rule[0] = %+v", rules[0])
	}
}

// key 미존재 → defaults 적용.
func TestEtcdWatcher_KeyMissing_UsesDefaults(t *testing.T) {
	cli := newEtcdClient(t)
	rs, _ := NewRuleSet(nil, nil)
	defer rs.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	defaults := []Rule{{Pattern: "POST /v1/tx", Rate: 50, Burst: 100}}
	w, err := NewEtcdWatcher(ctx, EtcdWatcherOptions{
		Client:   cli,
		Key:      "test/ratelimit/missing",
		RuleSet:  rs,
		Defaults: defaults,
		Logger:   quietLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	if got := rs.Rules(); len(got) != 1 || got[0].Pattern != "POST /v1/tx" {
		t.Errorf("defaults 적용 안 됨: %+v", got)
	}
}

// 운영자 PUT → 즉시 hot-swap.
func TestEtcdWatcher_PutTriggersHotSwap(t *testing.T) {
	cli := newEtcdClient(t)
	rs, _ := NewRuleSet(nil, nil)
	defer rs.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	key := "test/ratelimit/edge-api"
	w, err := NewEtcdWatcher(ctx, EtcdWatcherOptions{
		Client: cli, Key: key, RuleSet: rs, Logger: quietLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	// 운영자 PUT.
	body := `{"version":1, "rules":[{"pattern":"POST /v1/login","rate":5,"burst":10}]}`
	if _, err := cli.Put(ctx, key, body); err != nil {
		t.Fatal(err)
	}

	// 적용 대기.
	if err := waitFor(time.Second, func() bool {
		r := rs.Rules()
		return len(r) == 1 && r[0].Pattern == "POST /v1/login"
	}); err != nil {
		t.Fatalf("PUT 반영 안 됨: rules=%+v", rs.Rules())
	}
}

// DELETE → defaults 복원.
func TestEtcdWatcher_DeleteFallsBackToDefaults(t *testing.T) {
	cli := newEtcdClient(t)
	rs, _ := NewRuleSet(nil, nil)
	defer rs.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	key := "test/ratelimit/edge-api"
	if _, err := cli.Put(ctx, key, `{"rules":[{"pattern":"POST /v1/x","rate":1,"burst":1}]}`); err != nil {
		t.Fatal(err)
	}

	defaults := []Rule{{Pattern: "POST /v1/tx", Rate: 50, Burst: 100}}
	w, err := NewEtcdWatcher(ctx, EtcdWatcherOptions{
		Client:   cli,
		Key:      key,
		RuleSet:  rs,
		Defaults: defaults,
		Logger:   quietLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	// 초기엔 PUT 된 룰.
	if got := rs.Rules(); len(got) != 1 || got[0].Pattern != "POST /v1/x" {
		t.Fatalf("초기 PUT 룰 미적용: %+v", got)
	}

	// DELETE.
	if _, err := cli.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(time.Second, func() bool {
		r := rs.Rules()
		return len(r) == 1 && r[0].Pattern == "POST /v1/tx"
	}); err != nil {
		t.Fatalf("DELETE 후 defaults 미복원: rules=%+v", rs.Rules())
	}
}

// 잘못된 JSON / 룰 → 기존 룰 유지 (운영 중단 회피).
func TestEtcdWatcher_BadDocPreservesExisting(t *testing.T) {
	cli := newEtcdClient(t)
	rs, _ := NewRuleSet(nil, nil)
	defer rs.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	key := "test/ratelimit/edge-api"
	// 사전 — 정상 doc PUT.
	if _, err := cli.Put(ctx, key,
		`{"rules":[{"pattern":"POST /v1/tx","rate":50,"burst":100}]}`); err != nil {
		t.Fatal(err)
	}
	w, err := NewEtcdWatcher(ctx, EtcdWatcherOptions{
		Client: cli, Key: key, RuleSet: rs, Logger: quietLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	// 사전 확인 — initialLoad 로 정상 룰 적용.
	if got := rs.Rules(); len(got) != 1 || got[0].Pattern != "POST /v1/tx" {
		t.Fatalf("초기 정상 doc 미반영: %+v", got)
	}

	// 운영자 실수 PUT — 잘못된 JSON.
	if _, err := cli.Put(ctx, key, `garbage{`); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)

	// 기존 룰 유지.
	if got := rs.Rules(); len(got) != 1 || got[0].Pattern != "POST /v1/tx" {
		t.Errorf("잘못된 JSON 후 기존 룰 손상: %+v", got)
	}

	// 잘못된 룰 (음수 burst) PUT — 같은 동작.
	if _, err := cli.Put(ctx, key,
		`{"rules":[{"pattern":"POST /v1/x","rate":-1,"burst":1}]}`); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if got := rs.Rules(); len(got) != 1 || got[0].Pattern != "POST /v1/tx" {
		t.Errorf("음수 룰 후 기존 룰 손상: %+v", got)
	}
}

// waitFor — predicate 가 true 가 될 때까지 polling. timeout 이면 error.
func waitFor(timeout time.Duration, pred func() bool) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pred() {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pred() {
		return nil
	}
	return context.DeadlineExceeded
}

//go:build integration

package routing_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/winwaysystems/wtg/pkg/routing"
	"github.com/winwaysystems/wtg/test/etcdtest"
)

// EtcdRegistry round-trip — Put → 캐시 즉시 + Get/List 동작.
func TestEtcdRegistryPutGet(t *testing.T) {
	srv := etcdtest.Start(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	reg, err := routing.NewEtcdRegistry(ctx, routing.EtcdRegistryOptions{
		Endpoints: []string{srv.ClientURL},
		Prefix:    "wtg/test/routes/",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()

	if err := reg.Put(&routing.Rule{
		Alias: "ORDER_NEW", Exchange: "ORDER", RoutingKey: "NEW", Active: true,
	}, "admin01"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := reg.Get("ORDER_NEW")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Exchange != "ORDER" || got.RoutingKey != "NEW" || got.UpdatedBy != "admin01" {
		t.Errorf("rule: %+v", got)
	}

	if list := reg.List(); len(list) != 1 {
		t.Errorf("List len=%d", len(list))
	}
}

// 두 Registry 인스턴스가 같은 etcd 를 공유 — admin 측 변경이 api 측에 watch 로 전파.
func TestEtcdRegistryWatchPropagates(t *testing.T) {
	srv := etcdtest.Start(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mkReg := func() *routing.EtcdRegistry {
		r, err := routing.NewEtcdRegistry(ctx, routing.EtcdRegistryOptions{
			Endpoints: []string{srv.ClientURL},
			Prefix:    "wtg/test/routes/",
		})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	admin := mkReg(); defer admin.Close()
	api := mkReg();   defer api.Close()

	// admin 이 룰 등록.
	if err := admin.Put(&routing.Rule{
		Alias: "ORDER_NEW", Exchange: "ORDER", RoutingKey: "NEW_V2", Active: true,
	}, "admin"); err != nil {
		t.Fatal(err)
	}

	// api 측에 watch 로 도착할 때까지 대기 (보통 ms 단위).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if r, err := api.Get("ORDER_NEW"); err == nil && r.RoutingKey == "NEW_V2" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("watch 전파 타임아웃")
}

// Delete 도 watch 로 전파.
func TestEtcdRegistryWatchDelete(t *testing.T) {
	srv := etcdtest.Start(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mkReg := func() *routing.EtcdRegistry {
		r, _ := routing.NewEtcdRegistry(ctx, routing.EtcdRegistryOptions{
			Endpoints: []string{srv.ClientURL},
			Prefix:    "wtg/test/routes/",
		})
		return r
	}
	admin := mkReg(); defer admin.Close()
	api := mkReg();   defer api.Close()

	admin.Put(&routing.Rule{Alias: "X", Exchange: "E", RoutingKey: "K", Active: true}, "u")

	// api 측에 도착 확인.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := api.Get("X"); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// 삭제.
	if err := admin.Delete("X"); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := api.Get("X"); errors.Is(err, routing.ErrRouteNotFound) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("watch 삭제 전파 타임아웃")
}

// SetActive 토글.
func TestEtcdRegistrySetActive(t *testing.T) {
	srv := etcdtest.Start(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reg, err := routing.NewEtcdRegistry(ctx, routing.EtcdRegistryOptions{
		Endpoints: []string{srv.ClientURL},
		Prefix:    "wtg/test/routes/",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()

	reg.Put(&routing.Rule{Alias: "A", RoutingKey: "K", Active: true}, "u1")
	if err := reg.SetActive("A", false, "u2"); err != nil {
		t.Fatal(err)
	}
	got, _ := reg.Get("A")
	if got.Active {
		t.Error("SetActive 안됨")
	}
	if got.UpdatedBy != "u2" {
		t.Errorf("UpdatedBy: %s", got.UpdatedBy)
	}
}

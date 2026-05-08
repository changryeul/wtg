//go:build integration

package policy_test

import (
	"context"
	"testing"
	"time"

	"github.com/winwaysystems/wtg/pkg/policy"
	"github.com/winwaysystems/wtg/test/etcdtest"
)

// 두 Engine 인스턴스가 같은 etcd 키를 공유 — 한 쪽 변경이 다른 쪽에 watch 로 전파.
func TestEtcdSyncPropagatesPolicy(t *testing.T) {
	srv := etcdtest.Start(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mkPair := func(label string) (*policy.Engine, *policy.EtcdSync) {
		eng := policy.NewEngine(nil)
		sync, err := policy.StartEtcdSync(ctx, eng, policy.EtcdSyncOptions{
			Endpoints: []string{srv.ClientURL},
			Key:       "wtg/test/policy",
		})
		if err != nil {
			t.Fatalf("%s sync 시작: %v", label, err)
		}
		return eng, sync
	}

	adminEng, adminSync := mkPair("admin"); defer adminSync.Close()
	apiEng,   apiSync   := mkPair("api");   defer apiSync.Close()

	// admin 에서 kill switch 활성.
	adminEng.SetKillSwitch(true, "admin01")

	// api 측에 watch 도착할 때까지 대기.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if apiEng.State().KillSwitch {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !apiEng.State().KillSwitch {
		t.Fatal("kill switch 전파 타임아웃")
	}

	// 차단 심볼 추가도 전파.
	if err := adminEng.AddBlockedSymbol("USDKRW", "admin01"); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		st := apiEng.State()
		if len(st.BlockedSymbols) == 1 && st.BlockedSymbols[0] == "USDKRW" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("BlockedSymbols 전파 타임아웃")
}

// 초기 Get — 이미 etcd 에 정책이 있으면 Engine 이 그걸 적용한 채로 시작.
func TestEtcdSyncInitialLoad(t *testing.T) {
	srv := etcdtest.Start(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 먼저 admin Engine 으로 정책 작성.
	adminEng := policy.NewEngine(nil)
	adminSync, err := policy.StartEtcdSync(ctx, adminEng, policy.EtcdSyncOptions{
		Endpoints: []string{srv.ClientURL},
		Key:       "wtg/test/policy",
	})
	if err != nil {
		t.Fatal(err)
	}
	adminEng.SetKillSwitch(true, "admin")
	_ = adminEng.AddBlockedSymbol("EURUSD", "admin")

	// persist 가 도착할 시간 부여.
	time.Sleep(200 * time.Millisecond)
	adminSync.Close()

	// 새 Engine — etcd 에서 초기 상태 로드해야.
	freshEng := policy.NewEngine(nil)
	freshSync, err := policy.StartEtcdSync(ctx, freshEng, policy.EtcdSyncOptions{
		Endpoints: []string{srv.ClientURL},
		Key:       "wtg/test/policy",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer freshSync.Close()

	st := freshEng.State()
	if !st.KillSwitch {
		t.Error("초기 로드 후 kill switch 미적용")
	}
	if len(st.BlockedSymbols) == 0 || st.BlockedSymbols[0] != "EURUSD" {
		t.Errorf("초기 BlockedSymbols=%v", st.BlockedSymbols)
	}
}

//go:build integration

package price

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/winwaysystems/wtg/pkg/pricing"
	wtgpb "github.com/winwaysystems/wtg/pkg/wtgpb/v1"
	"github.com/winwaysystems/wtg/test/etcdtest"
)

// mock-lp cross e2e — embedded etcd 에 pair(직접 USDKRW/USDCNH + cross CNHKRW)를
// seed 하고, 실 EtcdPairWatcher → CrossRateConsumer 배선을 거쳐 mock-lp 시나리오
// (per-source USDKRW/USDCNH)를 흘려 CNH/KRW 재정환율이 AlgoStream 까지 도달하는지
// 결정적으로 검증한다. cross 산식(worse-side div)이 mds automkm refprctype=4 와
// 동일함을 값으로 확인.
//
// 실행: make test-integration  (또는 go test -tags integration ./internal/price/)
func TestMockLP_CrossE2E(t *testing.T) {
	e := etcdtest.Start(t)
	cli, err := clientv3.New(clientv3.Config{
		Endpoints: []string{e.ClientURL}, DialTimeout: 3 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()

	put := func(p pricing.Pair) {
		body, _ := json.Marshal(p)
		if _, err := cli.Put(context.Background(), "wtg/pair/"+p.ID, string(body)); err != nil {
			t.Fatal(err)
		}
	}
	// 직접 pair 2 + cross pair 1. CNH/KRW = USD/KRW ÷ USD/CNH (worse-side).
	put(pricing.Pair{ID: "USDKRW", Base: "USD", Quote: "KRW", Kind: "direct", Symbol: "USDKRW", Active: true})
	put(pricing.Pair{ID: "USDCNH", Base: "USD", Quote: "CNH", Kind: "direct", Symbol: "USDCNH", Active: true})
	put(pricing.Pair{ID: "CNHKRW", Base: "CNH", Quote: "KRW", Kind: "cross", Symbol: "CNHKRW", Active: true,
		Cross: &pricing.Cross{LegA: "USD/KRW", OpA: "mul", LegB: "USD/CNH", OpB: "div", Scale: 1}})

	// 실 배선: PairMaster ← EtcdPairWatcher, cross formula/symbol 자동 wire.
	pm := pricing.NewPairMaster()
	algo := NewAlgoStreamServer(nil, AlgoStreamOptions{RingSize: 64})
	defer algo.Stop()
	cross := NewCrossRateConsumer(CrossRateOptions{Pairs: pm, DebounceWindow: time.Nanosecond})
	cross.AddDownstream(algo)
	best := NewBestConsumer(BestOptions{}, cross, algo)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w, err := pricing.NewEtcdPairWatcher(ctx, pricing.EtcdPairWatcherOptions{
		Client: cli, M: pm,
		OnChange: func(m *pricing.PairMaster) {
			cross.ReplaceFormulas(m.CrossFormulas())
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	if len(pm.CrossFormulas()) != 1 {
		t.Fatalf("cross formula 로드 실패: %d", len(pm.CrossFormulas()))
	}

	// CNHKRW cross 구독자 (BEST 모드).
	sub := &algoSub{
		symbolSet: map[string]struct{}{"CNHKRW": {}},
		ch:        make(chan *wtgpb.AlgoQuote, 32),
		done:      make(chan struct{}),
	}
	algo.registerSub(sub)

	// mock-lp 시나리오 유입 (raw per-source → BestConsumer → BEST → cross).
	//   USDKRW: SMB 1380.10/1380.25, KMB 1380.05/1380.20 → BEST 1380.10/1380.20
	//   USDCNH: SMB 7.1000/7.1100,   KMB 7.0995/7.1025   → BEST 7.1000/7.1025
	best.OnTick(buildRaw("USDKRW", "SMB", 1380.10, 1380.25))
	best.OnTick(buildRaw("USDKRW", "KMB", 1380.05, 1380.20))
	best.OnTick(buildRaw("USDCNH", "SMB", 7.1000, 7.1100))
	best.OnTick(buildRaw("USDCNH", "KMB", 7.0995, 7.1025))

	// 기대값: CNH/KRW = BEST USDKRW / BEST USDCNH (worse-side)
	//   bid = 1380.10 / 7.1025,  ask = 1380.20 / 7.1000
	wantBid := 1380.10 / 7.1025
	wantAsk := 1380.20 / 7.1000
	wantMid := (wantBid + wantAsk) / 2

	// cross 는 leg 갱신마다 emit 하므로(SMB 반영본 → 양 leg BEST 반영본), 최종
	// 수렴값(BEST USDCNH 반영)을 취한다. 조용해질 때까지 drain 후 마지막 CNHKRW.
	var got *wtgpb.AlgoQuote
	overall := time.After(2 * time.Second)
loop:
	for {
		select {
		case q := <-sub.ch:
			if q.GetSym() == "CNHKRW" {
				got = q
			}
		case <-time.After(200 * time.Millisecond):
			break loop
		case <-overall:
			break loop
		}
	}
	if got == nil {
		t.Fatal("CNHKRW cross AlgoQuote 수신 없음")
	}

	if got.GetSource() != SourceCross {
		t.Errorf("source=%q, want CROSS", got.GetSource())
	}
	assertClose(t, "cross bid", got.GetBid(), wantBid)
	assertClose(t, "cross ask", got.GetAsk(), wantAsk)
	assertClose(t, "cross mid", got.GetMid(), wantMid)
}

func assertClose(t *testing.T, name string, got, want float64) {
	t.Helper()
	d := got - want
	if d < 0 {
		d = -d
	}
	if d > 1e-6 {
		t.Errorf("%s=%.8f, want %.8f (mds worse-side div 산식)", name, got, want)
	}
}

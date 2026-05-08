package policy

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func mkEngine(t *testing.T) *Engine { return NewEngine(nil) }

func TestEngineDefaultsAllow(t *testing.T) {
	e := mkEngine(t)
	d := e.Check(Request{RoutingKey: "NEW", Symbol: "USDKRW"})
	if !d.Allowed {
		t.Errorf("default 차단: %+v", d)
	}
}

func TestKillSwitch(t *testing.T) {
	e := mkEngine(t)
	e.SetKillSwitch(true, "admin01")
	d := e.Check(Request{RoutingKey: "NEW"})
	if d.Allowed {
		t.Error("kill switch 활성인데 통과")
	}
	if d.Reason != ReasonKillSwitch {
		t.Errorf("Reason=%q", d.Reason)
	}

	e.SetKillSwitch(false, "admin01")
	if !e.Check(Request{RoutingKey: "NEW"}).Allowed {
		t.Error("kill switch 해제 후 차단")
	}
}

// 채널별 kill switch — 일부 채널만 차단되고 다른 채널은 통과해야.
func TestKillSwitchScoped(t *testing.T) {
	e := mkEngine(t)
	// 고객 채널만 차단 (WEB/MOB/HTS), 직원 (ADM/EMP) 통과.
	e.SetKillSwitchScoped(true, []string{"WEB", "MOB", "HTS"}, "admin01")

	for _, ch := range []string{"WEB", "MOB", "HTS"} {
		d := e.Check(Request{Channel: ch, RoutingKey: "NEW"})
		if d.Allowed {
			t.Errorf("채널 %s 차단되어야 하는데 통과", ch)
		}
		if d.Reason != ReasonKillSwitch {
			t.Errorf("채널 %s Reason=%q", ch, d.Reason)
		}
	}
	for _, ch := range []string{"ADM", "EMP"} {
		d := e.Check(Request{Channel: ch, RoutingKey: "NEW"})
		if !d.Allowed {
			t.Errorf("직원 채널 %s 통과해야 하는데 차단: %+v", ch, d)
		}
	}

	// 빈 channels + active → 모든 채널 차단 (legacy).
	e.SetKillSwitchScoped(true, nil, "admin01")
	if e.Check(Request{Channel: "ADM"}).Allowed {
		t.Error("scope 비어있으면 ADM 도 차단되어야")
	}

	// 정규화 — 소문자/공백 입력도 대문자로 매칭.
	e.SetKillSwitchScoped(true, []string{" web ", "MOB"}, "admin01")
	if e.Check(Request{Channel: "WEB"}).Allowed {
		t.Error("소문자 입력 정규화 실패")
	}
}

func TestMaintenanceWindow(t *testing.T) {
	var clock atomic.Int64
	clock.Store(time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC).Unix())
	now := func() time.Time { return time.Unix(clock.Load(), 0).UTC() }

	e := NewEngine(now)
	start := time.Date(2026, 5, 1, 22, 0, 0, 0, time.UTC)
	end := time.Date(2026, 5, 2, 6, 0, 0, 0, time.UTC)
	if err := e.SetMaintenance(MaintenanceWindow{Start: start, End: end, Message: "심야 정비"}, "admin"); err != nil {
		t.Fatal(err)
	}

	// 12:00 — 윈도우 밖, 통과.
	if !e.Check(Request{RoutingKey: "NEW"}).Allowed {
		t.Error("윈도우 밖에서 차단됨")
	}

	// 23:00 — 윈도우 안.
	clock.Store(time.Date(2026, 5, 1, 23, 0, 0, 0, time.UTC).Unix())
	d := e.Check(Request{RoutingKey: "NEW"})
	if d.Allowed {
		t.Error("정비 윈도우 내인데 통과")
	}
	if d.Reason != ReasonMaintenance || d.Message != "심야 정비" {
		t.Errorf("decision: %+v", d)
	}

	// 06:00 동등 — 윈도우 끝. End 는 exclusive 라 통과.
	clock.Store(time.Date(2026, 5, 2, 6, 0, 0, 0, time.UTC).Unix())
	if !e.Check(Request{RoutingKey: "NEW"}).Allowed {
		t.Error("end 시각은 통과해야 (exclusive)")
	}
}

func TestMaintenanceInvalidWindow(t *testing.T) {
	e := mkEngine(t)
	if err := e.SetMaintenance(MaintenanceWindow{
		Start: time.Now(), End: time.Now().Add(-time.Hour),
	}, "x"); err == nil {
		t.Error("end < start 인데 통과")
	}
}

func TestBlockedSymbols(t *testing.T) {
	e := mkEngine(t)
	if err := e.AddBlockedSymbol("usdkrw", "admin"); err != nil {
		t.Fatal(err)
	}
	d := e.Check(Request{RoutingKey: "NEW", Symbol: "USDKRW"})
	if d.Allowed || d.Reason != ReasonSymbol {
		t.Errorf("decision: %+v", d)
	}
	// 다른 심볼 통과.
	if !e.Check(Request{RoutingKey: "NEW", Symbol: "EURUSD"}).Allowed {
		t.Error("EURUSD 차단됨")
	}
	// 대소문자 무관 매칭.
	if e.Check(Request{Symbol: "usdkrw", RoutingKey: "NEW"}).Allowed {
		t.Error("소문자 입력 시 매칭 실패")
	}

	e.RemoveBlockedSymbol("USDKRW", "admin")
	if !e.Check(Request{Symbol: "USDKRW", RoutingKey: "NEW"}).Allowed {
		t.Error("해제 후에도 차단")
	}
}

func TestBlockedRoutingKeys(t *testing.T) {
	e := mkEngine(t)
	e.AddBlockedRoutingKey("CANCEL", "admin")
	if e.Check(Request{RoutingKey: "CANCEL"}).Allowed {
		t.Error("CANCEL 차단 실패")
	}
	if !e.Check(Request{RoutingKey: "NEW"}).Allowed {
		t.Error("NEW 통과 실패")
	}
}

// 우선순위: KillSwitch → Maintenance → RoutingKey → Symbol.
func TestPrecedence(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 5, 1, 23, 0, 0, 0, time.UTC) }
	e := NewEngine(now)
	e.SetKillSwitch(true, "x")
	_ = e.SetMaintenance(MaintenanceWindow{
		Start: time.Date(2026, 5, 1, 22, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 5, 2, 6, 0, 0, 0, time.UTC),
	}, "x")
	e.AddBlockedRoutingKey("CANCEL", "x")
	e.AddBlockedSymbol("USDKRW", "x")

	d := e.Check(Request{RoutingKey: "CANCEL", Symbol: "USDKRW"})
	if d.Reason != ReasonKillSwitch {
		t.Errorf("Reason=%q, want kill_switch first", d.Reason)
	}

	e.SetKillSwitch(false, "x")
	d = e.Check(Request{RoutingKey: "CANCEL", Symbol: "USDKRW"})
	if d.Reason != ReasonMaintenance {
		t.Errorf("Reason=%q, want maintenance", d.Reason)
	}
}

func TestSetStateNormalizes(t *testing.T) {
	e := mkEngine(t)
	e.SetState(State{
		BlockedSymbols:     []string{"usdkrw", "USDKRW", "  eur ", ""},
		BlockedRoutingKeys: []string{"new", "NEW", "cancel"},
	}, "admin")
	st := e.State()
	if len(st.BlockedSymbols) != 2 {
		t.Errorf("BlockedSymbols=%v, want 2 unique", st.BlockedSymbols)
	}
	if st.BlockedSymbols[0] != "EUR" {
		t.Errorf("정렬: %v", st.BlockedSymbols)
	}
	if len(st.BlockedRoutingKeys) != 2 {
		t.Errorf("BlockedRoutingKeys=%v", st.BlockedRoutingKeys)
	}
}

func TestStateIsCopy(t *testing.T) {
	e := mkEngine(t)
	e.AddBlockedSymbol("USDKRW", "x")
	st := e.State()
	st.BlockedSymbols[0] = "MUTATED"
	st2 := e.State()
	if st2.BlockedSymbols[0] != "USDKRW" {
		t.Error("State() 가 deep copy 가 아님 — 외부 변이가 반영됨")
	}
}

func TestEngineConcurrent(t *testing.T) {
	e := mkEngine(t)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				e.AddBlockedSymbol("S"+string(rune('A'+i%26)), "x")
			} else {
				e.RemoveBlockedSymbol("S"+string(rune('A'+i%26)), "x")
			}
			e.Check(Request{RoutingKey: "NEW", Symbol: "USDKRW"})
			e.State()
		}(i)
	}
	wg.Wait()
}

// OnChange 가 모든 mutator 에서 호출되는지.
func TestEngineOnChangeFires(t *testing.T) {
	e := mkEngine(t)
	var calls int
	var lastState State
	e.SetOnChange(func(s State) {
		calls++
		lastState = s
	})

	e.SetKillSwitch(true, "u")
	if calls != 1 || !lastState.KillSwitch {
		t.Errorf("kill switch callback: calls=%d state=%+v", calls, lastState)
	}

	_ = e.AddBlockedSymbol("USDKRW", "u")
	if calls != 2 {
		t.Errorf("AddBlockedSymbol callback: calls=%d", calls)
	}

	e.RemoveBlockedSymbol("USDKRW", "u")
	if calls != 3 {
		t.Errorf("RemoveBlockedSymbol callback: calls=%d", calls)
	}

	_ = e.AddBlockedRoutingKey("CANCEL", "u")
	if calls != 4 {
		t.Errorf("AddBlockedRoutingKey callback: calls=%d", calls)
	}

	e.RemoveBlockedRoutingKey("CANCEL", "u")
	if calls != 5 {
		t.Errorf("RemoveBlockedRoutingKey callback: calls=%d", calls)
	}

	e.SetState(State{KillSwitch: true}, "u")
	if calls != 6 {
		t.Errorf("SetState callback: calls=%d", calls)
	}
}

// ApplyRemote 는 callback 을 호출하지 않아야 (etcd watch 무한 루프 방지).
func TestEngineApplyRemoteSuppressesCallback(t *testing.T) {
	e := mkEngine(t)
	var calls int
	e.SetOnChange(func(s State) { calls++ })

	e.ApplyRemote(State{
		KillSwitch:     true,
		BlockedSymbols: []string{"USDKRW"},
	})
	if calls != 0 {
		t.Errorf("ApplyRemote callback 호출됨: %d", calls)
	}

	st := e.State()
	if !st.KillSwitch {
		t.Error("ApplyRemote 가 상태 적용 안 함")
	}
	if d := e.Check(Request{RoutingKey: "NEW", Symbol: "USDKRW"}); d.Allowed {
		t.Error("ApplyRemote 후 차단되지 않음")
	}
}

func TestUpdatedAtBy(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	e := NewEngine(func() time.Time { return now })
	e.AddBlockedSymbol("USDKRW", "admin01")
	st := e.State()
	if !st.UpdatedAt.Equal(now) {
		t.Errorf("UpdatedAt: %v", st.UpdatedAt)
	}
	if st.UpdatedBy != "admin01" {
		t.Errorf("UpdatedBy: %q", st.UpdatedBy)
	}
}

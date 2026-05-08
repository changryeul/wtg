package routing

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

func TestRuleValidate(t *testing.T) {
	tests := []struct {
		name    string
		rule    Rule
		wantErr error
	}{
		{"정상", Rule{Alias: "ORDER_NEW", Exchange: "ORDER", RoutingKey: "NEW"}, nil},
		{"alias 빈값", Rule{Alias: "", RoutingKey: "X"}, ErrAliasRequired},
		{"alias 공백", Rule{Alias: "AAA BBB", RoutingKey: "X"}, ErrAliasInvalid},
		{"alias 슬래시", Rule{Alias: "A/B", RoutingKey: "X"}, ErrAliasInvalid},
		{"alias 너무 김", Rule{Alias: strings.Repeat("a", 65), RoutingKey: "X"}, ErrAliasInvalid},
		{"rkey 빈값", Rule{Alias: "A", RoutingKey: ""}, ErrRkeyRequired},
		{"rkey 너무 김", Rule{Alias: "A", RoutingKey: strings.Repeat("k", 17)}, ErrRkeyTooLong},
		{"exchange 너무 김", Rule{Alias: "A", Exchange: strings.Repeat("e", 9), RoutingKey: "K"}, ErrExchangeTooLong},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.rule.Validate()
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("err=%v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestInMemoryRegistryPutGet(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	reg := NewInMemoryRegistry(fixedClock(now))

	err := reg.Put(&Rule{
		Alias: "ORDER_NEW", Exchange: "ORDER", RoutingKey: "NEW", Active: true,
	}, "admin01")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := reg.Get("ORDER_NEW")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Exchange != "ORDER" || got.RoutingKey != "NEW" {
		t.Errorf("rule: %+v", got)
	}
	if !got.UpdatedAt.Equal(now) {
		t.Errorf("UpdatedAt: %v", got.UpdatedAt)
	}
	if got.UpdatedBy != "admin01" {
		t.Errorf("UpdatedBy: %s", got.UpdatedBy)
	}
}

// Get 이 deep copy 를 돌려줘서 외부 변경이 저장소에 영향 없는지.
func TestInMemoryRegistryGetReturnsCopy(t *testing.T) {
	reg := NewInMemoryRegistry(nil)
	reg.Put(&Rule{Alias: "A", RoutingKey: "K", Active: true}, "x")
	got, _ := reg.Get("A")
	got.Active = false

	again, _ := reg.Get("A")
	if !again.Active {
		t.Error("Get 결과 수정이 저장소에 반영됨 — copy 가 아님")
	}
}

func TestInMemoryRegistryNotFound(t *testing.T) {
	reg := NewInMemoryRegistry(nil)
	if _, err := reg.Get("nope"); !errors.Is(err, ErrRouteNotFound) {
		t.Errorf("err=%v, want ErrRouteNotFound", err)
	}
}

func TestInMemoryRegistryList(t *testing.T) {
	reg := NewInMemoryRegistry(nil)
	for _, alias := range []string{"C", "A", "B"} {
		reg.Put(&Rule{Alias: alias, RoutingKey: "K"}, "u")
	}
	list := reg.List()
	if len(list) != 3 {
		t.Fatalf("len=%d, want 3", len(list))
	}
	if list[0].Alias != "A" || list[1].Alias != "B" || list[2].Alias != "C" {
		t.Errorf("정렬 실패: %v %v %v", list[0].Alias, list[1].Alias, list[2].Alias)
	}
}

func TestInMemoryRegistryPutValidates(t *testing.T) {
	reg := NewInMemoryRegistry(nil)
	if err := reg.Put(&Rule{Alias: "", RoutingKey: "K"}, "u"); !errors.Is(err, ErrAliasRequired) {
		t.Errorf("빈 alias err=%v", err)
	}
	if err := reg.Put(nil, "u"); !errors.Is(err, ErrAliasRequired) {
		t.Errorf("nil rule err=%v", err)
	}
}

func TestInMemoryRegistryDelete(t *testing.T) {
	reg := NewInMemoryRegistry(nil)
	reg.Put(&Rule{Alias: "A", RoutingKey: "K"}, "u")

	if err := reg.Delete("A"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := reg.Get("A"); !errors.Is(err, ErrRouteNotFound) {
		t.Errorf("삭제 후 Get: %v", err)
	}
	if err := reg.Delete("A"); !errors.Is(err, ErrRouteNotFound) {
		t.Errorf("재삭제: %v, want ErrRouteNotFound", err)
	}
}

func TestInMemoryRegistrySetActive(t *testing.T) {
	t1 := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Hour)
	clock := t1
	reg := NewInMemoryRegistry(func() time.Time { return clock })

	reg.Put(&Rule{Alias: "A", RoutingKey: "K", Active: true}, "u1")
	clock = t2
	if err := reg.SetActive("A", false, "u2"); err != nil {
		t.Fatal(err)
	}
	got, _ := reg.Get("A")
	if got.Active {
		t.Error("비활성화 안됨")
	}
	if !got.UpdatedAt.Equal(t2) {
		t.Errorf("UpdatedAt 갱신 안됨: %v", got.UpdatedAt)
	}
	if got.UpdatedBy != "u2" {
		t.Errorf("UpdatedBy: %s", got.UpdatedBy)
	}

	if err := reg.SetActive("nope", true, "u"); !errors.Is(err, ErrRouteNotFound) {
		t.Errorf("미존재: %v", err)
	}
}

func TestResolveActiveOnly(t *testing.T) {
	reg := NewInMemoryRegistry(nil)
	reg.Put(&Rule{Alias: "ON", Exchange: "ORDER", RoutingKey: "NEW", Active: true}, "u")
	reg.Put(&Rule{Alias: "OFF", Exchange: "ORDER", RoutingKey: "NEW", Active: false}, "u")

	if r, err := Resolve(reg, "ON"); err != nil || r.Exchange != "ORDER" {
		t.Errorf("ON: %v %+v", err, r)
	}
	if _, err := Resolve(reg, "OFF"); !errors.Is(err, ErrRouteNotFound) {
		t.Errorf("비활성: %v", err)
	}
	if _, err := Resolve(reg, "MISSING"); !errors.Is(err, ErrRouteNotFound) {
		t.Errorf("미등록: %v", err)
	}
	if _, err := Resolve(nil, "ON"); !errors.Is(err, ErrRouteNotFound) {
		t.Errorf("nil registry: %v", err)
	}
	if _, err := Resolve(reg, ""); !errors.Is(err, ErrRouteNotFound) {
		t.Errorf("빈 alias: %v", err)
	}
}

func TestInMemoryRegistryConcurrent(t *testing.T) {
	reg := NewInMemoryRegistry(nil)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			alias := string(rune('A' + i%26))
			reg.Put(&Rule{Alias: alias, RoutingKey: "K", Active: true}, "u")
			reg.Get(alias)
			reg.SetActive(alias, false, "u")
			reg.List()
		}(i)
	}
	wg.Wait()
}

package pricing

import (
	"sync"
	"testing"
)

func TestCurrencyMaster_GetReplaceList(t *testing.T) {
	m := NewCurrencyMaster()
	if m.Size() != 0 || len(m.List()) != 0 {
		t.Fatal("초기 빈 master")
	}

	m.Replace([]Currency{
		{Code: "KRW", Name: "원", DecimalPlaces: 2, SortOrder: 20, Active: true},
		{Code: "USD", Name: "달러", DecimalPlaces: 4, SortOrder: 10, Active: true},
		{Code: "", Name: "빈 코드"}, // skip
	})
	if m.Size() != 2 {
		t.Errorf("size = %d, want 2 (empty code skip)", m.Size())
	}

	// Get.
	usd, ok := m.Get("USD")
	if !ok || usd.Name != "달러" || usd.DecimalPlaces != 4 {
		t.Errorf("Get USD = %+v ok=%v", usd, ok)
	}
	if _, ok := m.Get("ZZZ"); ok {
		t.Error("미등록 ZZZ 가 찾아짐")
	}

	// List — SortOrder 오름차순.
	list := m.List()
	if len(list) != 2 || list[0].Code != "USD" || list[1].Code != "KRW" {
		t.Errorf("List 정렬: %+v", list)
	}
}

func TestCurrencyMaster_SortStable(t *testing.T) {
	m := NewCurrencyMaster()
	// 같은 SortOrder 면 Code 사전순.
	m.Replace([]Currency{
		{Code: "EUR", SortOrder: 10, Active: true},
		{Code: "AUD", SortOrder: 10, Active: true},
		{Code: "USD", SortOrder: 10, Active: true},
	})
	list := m.List()
	if list[0].Code != "AUD" || list[1].Code != "EUR" || list[2].Code != "USD" {
		t.Errorf("alphabetical tiebreak: %+v", list)
	}
}

func TestCurrencyMaster_ConcurrentReadDuringReplace(t *testing.T) {
	m := NewCurrencyMaster()
	m.Replace([]Currency{{Code: "USD", Active: true}})

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, _ = m.Get("USD")
				_ = m.List()
				_ = m.Size()
			}
		}()
	}
	// 동시 Replace 100회.
	for i := 0; i < 100; i++ {
		m.Replace([]Currency{
			{Code: "USD", Active: true, SortOrder: i},
			{Code: "KRW", Active: true, SortOrder: i + 1},
		})
	}
	close(stop)
	wg.Wait()
}

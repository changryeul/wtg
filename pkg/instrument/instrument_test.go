package instrument

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

func sampleCatalog() *Catalog {
	c := NewCatalog()
	c.Replace([]Instrument{
		{Symbol: "USD/KRW", AssetClass: AssetFX, Market: "OTC", Upstream: UpstreamFX, Active: true},
		{Symbol: "EUR/USD", AssetClass: AssetFX, Market: "OTC", Upstream: UpstreamFX, Active: true},
		{Symbol: "101V6000", AssetClass: AssetFuture, Market: "KRX", Upstream: UpstreamKRX, Active: true},
		{Symbol: "KR1035020310", AssetClass: AssetBond, Market: "KRX", Upstream: UpstreamKRX, Active: true},
		{Symbol: "DEAD/PAIR", AssetClass: AssetFX, Market: "OTC", Upstream: UpstreamFX, Active: false}, // 비활성
	})
	return c
}

func TestCatalog_LookupRoute(t *testing.T) {
	c := sampleCatalog()

	it, ok := c.Lookup("101V6000")
	if !ok || it.AssetClass != AssetFuture || it.Upstream != UpstreamKRX {
		t.Errorf("Lookup(101V6000)=%+v ok=%v", it, ok)
	}
	if _, ok := c.Lookup("NOPE"); ok {
		t.Error("미등록 심볼이 found")
	}

	// Route — active 만.
	if up, ok := c.Route("USD/KRW"); !ok || up != UpstreamFX {
		t.Errorf("Route(USD/KRW)=%q ok=%v want fx/true", up, ok)
	}
	if up, ok := c.Route("101V6000"); !ok || up != UpstreamKRX {
		t.Errorf("Route(101V6000)=%q ok=%v want krx/true", up, ok)
	}
	// 비활성 → 라우팅 불가.
	if _, ok := c.Route("DEAD/PAIR"); ok {
		t.Error("비활성 심볼이 라우팅됨 (active=false 인데)")
	}
	// 미등록 → 불가.
	if _, ok := c.Route("NOPE"); ok {
		t.Error("미등록 심볼이 라우팅됨")
	}
}

func TestCatalog_RouteAll(t *testing.T) {
	c := sampleCatalog()
	byUp, unknown := c.RouteAll([]string{"USD/KRW", "101V6000", "EUR/USD", "KR1035020310", "DEAD/PAIR", "NOPE"})

	fx := byUp[UpstreamFX]
	krx := byUp[UpstreamKRX]
	sort.Strings(fx)
	sort.Strings(krx)
	sort.Strings(unknown)

	if !reflect.DeepEqual(fx, []string{"EUR/USD", "USD/KRW"}) {
		t.Errorf("fx 그룹=%v", fx)
	}
	if !reflect.DeepEqual(krx, []string{"101V6000", "KR1035020310"}) {
		t.Errorf("krx 그룹=%v", krx)
	}
	// 비활성 + 미등록은 unknown.
	if !reflect.DeepEqual(unknown, []string{"DEAD/PAIR", "NOPE"}) {
		t.Errorf("unknown=%v want [DEAD/PAIR NOPE]", unknown)
	}
}

func TestCatalog_ReplaceAtomicAndSize(t *testing.T) {
	c := NewCatalog()
	if c.Size() != 0 {
		t.Fatalf("빈 카탈로그 Size=%d", c.Size())
	}
	c.Replace([]Instrument{{Symbol: "A", Upstream: UpstreamFX, Active: true}})
	if c.Size() != 1 {
		t.Fatalf("Size=%d want 1", c.Size())
	}
	// 중복 Symbol → 뒤가 이김.
	c.Replace([]Instrument{
		{Symbol: "A", Market: "old", Upstream: UpstreamFX, Active: true},
		{Symbol: "A", Market: "new", Upstream: UpstreamFX, Active: true},
	})
	if it, _ := c.Lookup("A"); it.Market != "new" {
		t.Errorf("중복 Symbol 병합 오류: %+v", it)
	}
}

func TestLoadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "instruments.json")
	content := `[
	  {"symbol":"USD/KRW","asset_class":"FX","market":"OTC","upstream":"fx","active":true},
	  {"symbol":"101V6000","asset_class":"FUTURE","market":"KRX","upstream":"krx","active":true}
	]`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	items, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("로드 수=%d want 2", len(items))
	}
	c := NewCatalog()
	c.Replace(items)
	if up, ok := c.Route("101V6000"); !ok || up != UpstreamKRX {
		t.Errorf("로드 후 Route(101V6000)=%q ok=%v", up, ok)
	}

	// 없는 파일 → 에러.
	if _, err := LoadFile(filepath.Join(dir, "nope.json")); err == nil {
		t.Error("없는 파일인데 에러 없음")
	}
}

package lpcatalog

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func sample() *Catalog {
	c := NewCatalog()
	c.Replace([]LP{
		{Code: "SMB", Category: CategoryBroker, Group: "227.10.40.11", Port: 45010, Active: true},
		{Code: "SHB", Category: CategoryBank, Group: "227.10.40.21", Port: 45010, Active: true},
		{Code: "JPM", Category: CategoryForeign, Group: "227.10.40.31", Port: 45010, Active: true},
		{Code: "DUH", Category: CategoryForeign, Group: "227.10.40.32", Port: 45010, Active: false}, // 비활성
	})
	return c
}

func TestLookupAndByGroupPort(t *testing.T) {
	c := sample()
	if lp, ok := c.Lookup("SMB"); !ok || lp.Category != CategoryBroker || lp.Group != "227.10.40.11" {
		t.Errorf("Lookup(SMB)=%+v ok=%v", lp, ok)
	}
	if _, ok := c.Lookup("NOPE"); ok {
		t.Error("미등록 LP found")
	}
	// 수신부 태깅: (group,port) → LP
	if lp, ok := c.ByGroupPort("227.10.40.21", 45010); !ok || lp.Code != "SHB" {
		t.Errorf("ByGroupPort(SHB)=%+v ok=%v", lp, ok)
	}
	if _, ok := c.ByGroupPort("227.10.40.99", 45010); ok {
		t.Error("미등록 group:port 가 매칭됨")
	}
	if _, ok := c.ByGroupPort("227.10.40.21", 9999); ok {
		t.Error("포트 불일치인데 매칭됨")
	}
}

func TestActiveFeeds(t *testing.T) {
	c := sample()
	feeds := c.ActiveFeeds()
	codes := make([]string, 0, len(feeds))
	for _, f := range feeds {
		codes = append(codes, f.Code)
	}
	sort.Strings(codes)
	// DUH 는 비활성 → 제외.
	want := []string{"JPM", "SHB", "SMB"}
	if len(codes) != len(want) {
		t.Fatalf("ActiveFeeds=%v want %v", codes, want)
	}
	for i := range want {
		if codes[i] != want[i] {
			t.Errorf("ActiveFeeds[%d]=%q want %q", i, codes[i], want[i])
		}
	}
}

func TestReplaceAtomic(t *testing.T) {
	c := NewCatalog()
	if c.Size() != 0 {
		t.Fatalf("빈 Size=%d", c.Size())
	}
	c.Replace([]LP{{Code: "SMB", Group: "g1", Port: 1, Active: true}})
	if c.Size() != 1 {
		t.Fatalf("Size=%d want 1", c.Size())
	}
	// 중복 code → 뒤가 이김 + 역색인 갱신.
	c.Replace([]LP{
		{Code: "SMB", Group: "old", Port: 1, Active: true},
		{Code: "SMB", Group: "new", Port: 2, Active: true},
	})
	if lp, _ := c.Lookup("SMB"); lp.Group != "new" || lp.Port != 2 {
		t.Errorf("중복 병합 오류: %+v", lp)
	}
	if _, ok := c.ByGroupPort("new", 2); !ok {
		t.Error("역색인이 새 group:port 로 갱신 안 됨")
	}
}

func TestLoadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lp.json")
	content := `[
	  {"code":"SMB","category":"broker","group":"227.10.40.11","port":45010,"active":true},
	  {"code":"JPM","category":"foreign","group":"227.10.40.31","port":45010,"active":true}
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
	if lp, ok := c.ByGroupPort("227.10.40.11", 45010); !ok || lp.Code != "SMB" {
		t.Errorf("로드 후 ByGroupPort=%+v ok=%v", lp, ok)
	}
	if _, err := LoadFile(filepath.Join(dir, "nope.json")); err == nil {
		t.Error("없는 파일인데 에러 없음")
	}
}

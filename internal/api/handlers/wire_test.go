package handlers

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/winwaysystems/wtg/pkg/svcio"
)

// 테스트용 registry — COMHDR 축소판 + 트랜잭션 1개.
func newTestSvcIO(t *testing.T) *svcio.Registry {
	t.Helper()
	reg := svcio.NewRegistry()
	reg.RegisterHeader("COMHDR", []svcio.Field{
		{Name: "trxc", CType: "char", Size: 16},
		{Name: "usid", CType: "char", Size: 30},
		{Name: "ltyp", CType: "char", Size: 2},
	})
	dir := t.TempDir()
	hdr := `typedef struct {	// Input
	char	prGb			 [   1];  // 작업구분
} W9999T01_I;

typedef struct {	// Output
	char 	result			[ 10];  // 결과
} W9999T01_O;
`
	if err := os.WriteFile(filepath.Join(dir, "W9999T01.h"), []byte(hdr), 0o644); err != nil {
		t.Fatal(err)
	}
	reg.SetDirHeaderDefault(dir, "COMHDR")
	if n, _, err := reg.LoadDir(dir, nil); err != nil || n != 1 {
		t.Fatalf("LoadDir: n=%d err=%v", n, err)
	}
	return reg
}

func TestWireBuildBody(t *testing.T) {
	reg := newTestSvcIO(t)

	// object + 명세 존재 → 고정폭 조립 (usid 서버 강제).
	body, spec, err := wireBuildBody(reg, "W9999T01", "tester01",
		map[string]interface{}{"usid": "hacker", "ltyp": "KR"},
		json.RawMessage(`{"prGb":"1"}`))
	if err != nil || spec == nil {
		t.Fatalf("err=%v spec=%v", err, spec)
	}
	if len(body) != 16+30+2+1 {
		t.Fatalf("body 길이 %d", len(body))
	}
	if got := strings.TrimRight(string(body[0:16]), " "); got != "W9999T01" {
		t.Errorf("trxc=%q", got)
	}
	if got := strings.TrimRight(string(body[16:46]), " "); got != "tester01" {
		t.Errorf("usid 강제 실패: %q", got) // "hacker" 로 덮이면 안 됨
	}
	if body[48] != '1' {
		t.Errorf("prGb=%q", body[48])
	}

	// 문자열 data → passthrough (nil 반환).
	b2, s2, err := wireBuildBody(reg, "W9999T01", "tester01", nil, json.RawMessage(`"RAWSTRING"`))
	if err != nil || b2 != nil || s2 != nil {
		t.Errorf("문자열은 passthrough 여야 함: %v %v %v", b2, s2, err)
	}

	// 명세 없는 rkey → passthrough.
	b3, s3, err := wireBuildBody(reg, "NOSPEC", "tester01", nil, json.RawMessage(`{"a":1}`))
	if err != nil || b3 != nil || s3 != nil {
		t.Errorf("명세 없으면 passthrough: %v %v %v", b3, s3, err)
	}
}

func TestWireParseReply(t *testing.T) {
	reg := newTestSvcIO(t)
	spec, _ := reg.Get("W9999T01")

	reply := []byte("W9999T01        " + // trxc 16
		"tester01                      " + // usid 30
		"KR" + // ltyp 2
		"OK        ") // result 10
	hdr, out, err := wireParseReply(spec, reply)
	if err != nil {
		t.Fatal(err)
	}
	if hdr["usid"] != "tester01" || hdr["ltyp"] != "KR" {
		t.Errorf("hdr=%v", hdr)
	}
	if out["result"] != "OK" {
		t.Errorf("out=%v", out)
	}
}

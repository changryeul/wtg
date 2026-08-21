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
	// 입력 반복부(struct[]) 있는 TR — _irec 매핑 테스트용.
	hdr2 := `typedef struct {	// Input
	char	cnt			 [   2];  // 건수
	struct {
		char	code		 [   3];  // 코드
		char	amt			 [   5];  // 금액
	} LST[1];
} W9998A01_I;

typedef struct {	// Output
	char 	result			[ 10];  // 결과
} W9998A01_O;
`
	if err := os.WriteFile(filepath.Join(dir, "W9998A01.h"), []byte(hdr2), 0o644); err != nil {
		t.Fatal(err)
	}
	reg.SetDirHeaderDefault(dir, "COMHDR")
	if n, _, err := reg.LoadDir(dir, nil); err != nil || n != 2 {
		t.Fatalf("LoadDir: n=%d err=%v", n, err)
	}
	return reg
}

func TestWireBuildBody(t *testing.T) {
	reg := newTestSvcIO(t)

	// object + 명세 존재 → 고정폭 조립 (usid 서버 강제).
	body, spec, err := wireBuildBody(reg, "W9999T01", "tester01", "",
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
	b2, s2, err := wireBuildBody(reg, "W9999T01", "tester01", "", nil, json.RawMessage(`"RAWSTRING"`))
	if err != nil || b2 != nil || s2 != nil {
		t.Errorf("문자열은 passthrough 여야 함: %v %v %v", b2, s2, err)
	}

	// 명세 없는 rkey → passthrough.
	b3, s3, err := wireBuildBody(reg, "NOSPEC", "tester01", "", nil, json.RawMessage(`{"a":1}`))
	if err != nil || b3 != nil || s3 != nil {
		t.Errorf("명세 없으면 passthrough: %v %v %v", b3, s3, err)
	}
}

func TestCtypForChannel(t *testing.T) {
	cases := map[string]string{
		"EMP": "E", "emp": "E", " EMP ": "E",
		"HTS": "1", "MTS": "2", "WTS": "3", "SPI": "S",
		"WEB": "A", "API": "A", "": "A", "unknown": "A",
	}
	for in, want := range cases {
		if got := ctypForChannel(in); got != want {
			t.Errorf("ctypForChannel(%q)=%q want %q", in, got, want)
		}
	}
}

// COMHDR 에 ctyp 필드가 있을 때: 세션 채널로 서버가 확립하고 클라 header 로는 못 바꾼다.
func TestWireBuildBodyCtypServerEstablished(t *testing.T) {
	reg := svcio.NewRegistry()
	reg.RegisterHeader("COMHDR", []svcio.Field{
		{Name: "trxc", CType: "char", Size: 16},
		{Name: "usid", CType: "char", Size: 16},
		{Name: "ctyp", CType: "char", Size: 1},
	})
	dir := t.TempDir()
	hdr := `typedef struct {	// Input
	char	prGb			 [   1];  // 작업구분
} T4304A01_I;

typedef struct {	// Output
	char 	result			[ 10];  // 결과
} T4304A01_O;
`
	if err := os.WriteFile(filepath.Join(dir, "T4304A01.h"), []byte(hdr), 0o644); err != nil {
		t.Fatal(err)
	}
	reg.SetDirHeaderDefault(dir, "COMHDR")
	if _, _, err := reg.LoadDir(dir, nil); err != nil {
		t.Fatal(err)
	}

	// EMP 채널 → ctyp='E'; 클라가 header.ctyp='A' 로 위조 시도해도 무시.
	body, spec, err := wireBuildBody(reg, "T4304A01", "emp01", "EMP",
		map[string]interface{}{"ctyp": "A"}, json.RawMessage(`{"prGb":"1"}`))
	if err != nil || spec == nil {
		t.Fatalf("err=%v spec=%v", err, spec)
	}
	// COMHDR: trxc[16] usid[16] ctyp[1] → ctyp offset 32.
	if body[32] != 'E' {
		t.Errorf("ctyp=%q want 'E' (세션 채널 EMP 강제, 클라 위조 무시)", body[32])
	}

	// WEB 채널 → ctyp='A'.
	body2, _, _ := wireBuildBody(reg, "T4304A01", "web01", "WEB", nil, json.RawMessage(`{"prGb":"1"}`))
	if body2[32] != 'A' {
		t.Errorf("ctyp=%q want 'A' (WEB)", body2[32])
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

func TestWireBuildBodyIrec(t *testing.T) {
	reg := newTestSvcIO(t)
	// 웹은 서버 struct[] 필드명(LST)을 모르고 표준 키 _irec 배열로 보낸다 → LST 에 매핑.
	body, spec, err := wireBuildBody(reg, "W9998A01", "tester01", "", nil,
		json.RawMessage(`{"cnt":"2","_irec":[{"code":"USD","amt":"100"},{"code":"KRW","amt":"200"}]}`))
	if err != nil || spec == nil {
		t.Fatalf("err=%v spec=%v", err, spec)
	}
	// COMHDR(48) + cnt(2) + LST[i] 는 spec Repeat 에 따라 조립. 반복부가 실제로 직렬화됐는지
	// 첫 행 code=USD, amt=100 이 body 에 나타나는지 확인.
	if !strings.Contains(string(body), "USD") || !strings.Contains(string(body), "100") {
		t.Fatalf("입력 반복부 1행 미직렬화: %q", string(body))
	}
	if !strings.Contains(string(body), "KRW") || !strings.Contains(string(body), "200") {
		t.Fatalf("입력 반복부 2행 미직렬화(Repeat=-1 가변 확인): %q", string(body))
	}
	// _irec 키는 소비돼 사라져야(원시 필드로 새지 않음).
}

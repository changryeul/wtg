package svcio

import (
	"strings"
	"testing"
)

// W1104S01_I 와 같은 단순 char[N] 트리에 대한 직렬화 + 길이/내용 검증.
func TestSerialize_Char(t *testing.T) {
	fields := []Field{
		{Name: "lgen_no", CType: "char", Size: 10, Comment: "거래참여자번호"},
		{Name: "lgen_nm", CType: "char", Size: 70, Comment: "거래참여자명"},
		{Name: "scrn", CType: "char", Size: 4, Comment: "화면번호"},
	}
	in := map[string]interface{}{
		"lgen_no": "0001",
		"lgen_nm": "거래자A", // CP949 로 인코딩 — 한글 문자 1자 = 2 byte
		"scrn":    "0001",
	}
	buf, err := Serialize(fields, in)
	if err != nil {
		t.Fatal(err)
	}
	want := 10 + 70 + 4
	if len(buf) != want {
		t.Fatalf("총 길이 %d, want %d", len(buf), want)
	}
	// 첫 10byte = "0001" + 6 space.
	if string(buf[:10]) != "0001      " {
		t.Errorf("lgen_no=%q", string(buf[:10]))
	}
	// 마지막 4byte = "0001"
	if string(buf[80:84]) != "0001" {
		t.Errorf("scrn=%q", string(buf[80:84]))
	}
	// lgen_nm 의 처음 6byte 가 CP949 인코딩 된 "거래자A" (한글 3자 + ASCII 1자 = 7 byte).
	// 정확한 byte 비교는 로케일 / 라이브러리 의존이라 숫자 길이만 확인.
	got := string(buf[10:80])
	if !strings.Contains(strings.TrimRight(got, " "), "A") {
		t.Errorf("lgen_nm 의 끝부분 'A' 미발견: %q", got)
	}
}

// 직렬화 → 역직렬화 round-trip — 입력 = 출력 (공백 trim 후).
func TestSerializeDeserialize_RoundTrip(t *testing.T) {
	fields := []Field{
		{Name: "id", CType: "char", Size: 8, Comment: "ID"},
		{Name: "name", CType: "char", Size: 20, Comment: "이름"},
		{Name: "yn", CType: "char", Size: 1, Comment: "여부"},
	}
	in := map[string]interface{}{
		"id":   "X12345",
		"name": "홍길동",
		"yn":   "Y",
	}
	buf, err := Serialize(fields, in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Deserialize(fields, buf)
	if err != nil {
		t.Fatal(err)
	}
	if out["id"] != "X12345" {
		t.Errorf("id=%q", out["id"])
	}
	if out["name"] != "홍길동" {
		t.Errorf("name=%q", out["name"])
	}
	if out["yn"] != "Y" {
		t.Errorf("yn=%q", out["yn"])
	}
}

// 가변 grid (Repeat=-1) — count 가 직전 char field 에서 와야.
func TestDeserialize_VariableGrid(t *testing.T) {
	fields := []Field{
		{Name: "rcnt", CType: "char", Size: 6, Comment: "건수"},
		{Name: "orec", CType: "REC_T", Repeat: -1, Children: []Field{
			{Name: "code", CType: "char", Size: 4},
			{Name: "name", CType: "char", Size: 10},
		}},
	}
	// rcnt=2, 그 다음 (4+10) byte × 2 row.
	buf := []byte("     2" + "AAA1" + "test1     " + "BBB2" + "test2     ")
	out, err := Deserialize(fields, buf)
	if err != nil {
		t.Fatal(err)
	}
	if out["rcnt"] != "2" {
		t.Errorf("rcnt=%q want '2'", out["rcnt"])
	}
	rows, ok := out["orec"].([]map[string]interface{})
	if !ok {
		t.Fatalf("orec 타입 mismatch: %T", out["orec"])
	}
	if len(rows) != 2 {
		t.Fatalf("rows=%d want 2", len(rows))
	}
	if rows[0]["code"] != "AAA1" || rows[0]["name"] != "test1" {
		t.Errorf("row0=%+v", rows[0])
	}
	if rows[1]["code"] != "BBB2" || rows[1]["name"] != "test2" {
		t.Errorf("row1=%+v", rows[1])
	}
}

// W1101S01-style 통합 round-trip — 헤더가 `orec[1]` 로 선언했지만 broker 가
// `grid01_cnt` 만큼 N 행 보내는 경우. Parse 의 trailing-array heuristic 이
// Repeat=-1 로 reclassify 해서 wire deserialize 가 N 행 모두 읽는지 검증.
func TestDeserialize_W1101S01_TrailingArrayHack(t *testing.T) {
	src := `typedef struct { // Output
  char grid01_cnt[6];
    struct {
      char empid[7];
      char emnm[40];
    } orec[1];
} W1101S01_O;`
	s, err := Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	if s.Output[1].Repeat != -1 {
		t.Fatalf("Parse 후 orec.Repeat=%d (heuristic 미작동 — 기대값 -1)", s.Output[1].Repeat)
	}

	// broker 응답 시뮬: grid01_cnt="     3" + 3 row × (7+40 byte).
	// 이전 동작에선 1 행만 읽고 끝났음 (Repeat=1 기본).
	buf := []byte("     3" +
		"E00001 " + "Alice                                   " +
		"E00002 " + "Bob                                     " +
		"E00003 " + "Charlie                                 ")
	out, err := Deserialize(s.Output, buf)
	if err != nil {
		t.Fatal(err)
	}
	if out["grid01_cnt"] != "3" {
		t.Errorf("grid01_cnt=%q want '3'", out["grid01_cnt"])
	}
	rows, ok := out["orec"].([]map[string]interface{})
	if !ok {
		t.Fatalf("orec 타입 mismatch: %T", out["orec"])
	}
	if len(rows) != 3 {
		t.Fatalf("rows=%d want 3 (이전 buggy 동작은 1)", len(rows))
	}
	if rows[0]["empid"] != "E00001" || rows[0]["emnm"] != "Alice" {
		t.Errorf("row0=%+v", rows[0])
	}
	if rows[2]["empid"] != "E00003" || rows[2]["emnm"] != "Charlie" {
		t.Errorf("row2=%+v", rows[2])
	}
}

// 입력에 없는 필드 — 공백 fill, 잘림 없음.
func TestSerialize_MissingFieldsAreSpaced(t *testing.T) {
	fields := []Field{
		{Name: "a", CType: "char", Size: 5},
		{Name: "b", CType: "char", Size: 5},
	}
	buf, err := Serialize(fields, map[string]interface{}{"a": "X"})
	if err != nil {
		t.Fatal(err)
	}
	if string(buf) != "X    "+"     " {
		t.Errorf("buf=%q", string(buf))
	}
}

// 입력이 size 보다 길면 truncate.
func TestSerialize_Truncates(t *testing.T) {
	fields := []Field{
		{Name: "a", CType: "char", Size: 3},
	}
	buf, err := Serialize(fields, map[string]interface{}{"a": "ABCDEFG"})
	if err != nil {
		t.Fatal(err)
	}
	if string(buf) != "ABC" {
		t.Errorf("buf=%q want 'ABC'", string(buf))
	}
}

// 전문 인코딩 UTF-8 통일 (2026-07-08) — 한글 왕복 + rune 경계 절단 + CP949 fallback.
func TestWireEncodingUTF8(t *testing.T) {
	// 1. UTF-8 왕복 — 한글이 그대로 보존.
	fields := []Field{{Name: "mesg", CType: "char", Size: 40, Comment: "메시지"}}
	buf, err := Serialize(fields, map[string]interface{}{"mesg": "DB System 장애 입니다."})
	if err != nil {
		t.Fatal(err)
	}
	out, err := Deserialize(fields, buf)
	if err != nil {
		t.Fatal(err)
	}
	if out["mesg"] != "DB System 장애 입니다." {
		t.Errorf("UTF-8 왕복 불일치: %q", out["mesg"])
	}

	// 2. char[N] 절단이 rune 중간에 걸리면 경계까지 되돌림 — invalid UTF-8 생성 금지.
	small := []Field{{Name: "nm", CType: "char", Size: 4, Comment: "이름"}}
	buf2, err := Serialize(small, map[string]interface{}{"nm": "가나"}) // UTF-8 6B > 4
	if err != nil {
		t.Fatal(err)
	}
	if len(buf2) != 4 {
		t.Fatalf("길이 %d, want 4", len(buf2))
	}
	if got := strings.Trim(string(buf2), " "); got != "가" {
		t.Errorf("rune 경계 절단 실패: %q (raw=% x)", got, buf2)
	}

	// 3. 레거시 CP949 응답 fallback — EUC-KR bytes 도 올바르게 복원.
	euckr := []byte{0xB0, 0xA1, 0xB3, 0xAA} // "가나" (EUC-KR)
	out3, err := Deserialize([]Field{{Name: "nm", CType: "char", Size: 4}}, euckr)
	if err != nil {
		t.Fatal(err)
	}
	if out3["nm"] != "가나" {
		t.Errorf("CP949 fallback 실패: %q", out3["nm"])
	}
}

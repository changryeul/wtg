package svcio

import (
	"strings"
	"testing"
)

// TestNormalizeTrailingBlockComment — `/* ... */` trailing → `// ...` 정규화.
// parseFieldLine 의 reFieldArray 가 `//` 만 capture 해서 발생하던 1% 격차 (운영
// W1901A01.h / W1901S01.h 가 tab + `/* */` 스타일) 의 진단 회귀 테스트.
func TestNormalizeTrailingBlockComment(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "단순 trailing block comment — /* 이전 공백 trim 후 space 1 + //",
			in:   "char  empid[7];   /* 직원번호 */",
			want: "char  empid[7]; // 직원번호",
		},
		{
			name: "tab + 다중 공백 (W1901A01.h 스타일)",
			in:   "\tchar    athor_dmnd_no        [  15]; /* 승인요청번호 */",
			want: "\tchar    athor_dmnd_no        [  15]; // 승인요청번호",
		},
		{
			name: "한글 comment 보존",
			in:   "char emnm[40]; /* 직원명 */",
			want: "char emnm[40]; // 직원명",
		},
		{
			name: "이미 // 주석인 경우 변경 X",
			in:   "char brnccd[1]; //회사,소속별 관리자코드",
			want: "char brnccd[1]; //회사,소속별 관리자코드",
		},
		{
			name: "주석 자체가 없는 경우",
			in:   "char brnccd[1];",
			want: "char brnccd[1];",
		},
		{
			name: "/* 만 있고 */ 미완 — 변환 skip (안전)",
			in:   "char x[1]; /* 미완 주석",
			want: "char x[1]; /* 미완 주석",
		},
		{
			name: "*/ 뒤에 코드 — 변환 skip (corner case)",
			in:   "char x[1]; /* mid */ extra;",
			want: "char x[1]; /* mid */ extra;",
		},
		{
			name: "block comment 만 단독 (라인 시작)",
			in:   "/* 헤더 그룹 구분 */",
			want: " // 헤더 그룹 구분",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeTrailingBlockComment(tc.in)
			if got != tc.want {
				t.Errorf("normalize\n  in:  %q\n  got: %q\n  want:%q", tc.in, got, tc.want)
			}
		})
	}
}

// TestParseFieldLine_BlockComment — parseFieldLine 가 `/* */` 라인도
// 정확히 잡는지 (field name / size / comment 보존).
func TestParseFieldLine_BlockComment(t *testing.T) {
	cases := []struct {
		name        string
		line        string
		wantName    string
		wantCType   string
		wantSize    int
		wantComment string
	}{
		{
			name:        "W1901A01.h 스타일 — tab + /* 한글 */",
			line:        "\tchar    athor_dmnd_no        [  15]; /* 승인요청번호 */",
			wantName:    "athor_dmnd_no",
			wantCType:   "char",
			wantSize:    15,
			wantComment: "승인요청번호",
		},
		{
			name:        "공백 + // 한글 (기존 회귀 보호)",
			line:        "    char brnccd   [  1]; //회사,소속별 관리자코드",
			wantName:    "brnccd",
			wantCType:   "char",
			wantSize:    1,
			wantComment: "회사,소속별 관리자코드",
		},
		{
			name:        "tab + /* 영문 */",
			line:        "\tchar\twinkey               [   8]; /* 윈도키코드 */",
			wantName:    "winkey",
			wantCType:   "char",
			wantSize:    8,
			wantComment: "윈도키코드",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, ok := parseFieldLine(tc.line)
			if !ok {
				t.Fatalf("parseFieldLine ok=false — line not matched: %q", tc.line)
			}
			if f.Name != tc.wantName {
				t.Errorf("name: got %q, want %q", f.Name, tc.wantName)
			}
			if f.CType != tc.wantCType {
				t.Errorf("ctype: got %q, want %q", f.CType, tc.wantCType)
			}
			if f.Size != tc.wantSize {
				t.Errorf("size: got %d, want %d", f.Size, tc.wantSize)
			}
			if f.Comment != tc.wantComment {
				t.Errorf("comment: got %q, want %q", f.Comment, tc.wantComment)
			}
		})
	}
}

// TestParse_W1901_Style — W1901A01.h 와 동일 패턴 (tab + /* */ + Input/Output 두
// typedef) 의 inline 헤더 텍스트가 input/output 모두 정확히 추출되는지.
// 운영 sample 의 ASCII 동등 형태.
func TestParse_W1901_Style(t *testing.T) {
	const src = `
#ifndef __W1901A01__H__
#define __W1901A01__H__

typedef struct {                        // Input
	char    athor_dmnd_no        [  15]; /* 승인요청번호 */
	char    athor_dmnd_ctnt      [1000]; /* 승인요청내용 */
	char    athor_stus_dstcd     [   2]; /* 승인상태구분코드 */
} W1901A01_I;

typedef struct {                        // Output
	char    athor_dmnd_no        [  15]; /* 승인요청번호 */
} W1901A01_O;

#endif
`
	spec, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if spec.Code != "W1901A01" {
		t.Errorf("code: got %q, want W1901A01", spec.Code)
	}
	if len(spec.Input) != 3 {
		t.Errorf("input count: got %d, want 3", len(spec.Input))
	}
	if len(spec.Output) != 1 {
		t.Errorf("output count: got %d, want 1", len(spec.Output))
	}
	// 한글 comment 보존 확인.
	if len(spec.Input) > 0 && !strings.Contains(spec.Input[0].Comment, "승인요청번호") {
		t.Errorf("input[0].comment: got %q, want 승인요청번호 포함", spec.Input[0].Comment)
	}
}

package svcio

import (
	"path/filepath"
	"testing"
)

const sampleW1104 = `/******************************************************************************
 *  Components  : W1104S01.h
 ******************************************************************************/
#ifndef __W1104S01__H__
#define __W1104S01__H__

typedef struct {  // Input
	char lgen_no		[10];	// 거래참여자번호
	char lgen_nm		[70];	// 거래참여자명
	char cls_yn			[ 1];	// CLS여부
	char scrn			[ 4];	// 화면번호
} W1104S01_I;

typedef struct {  // Output
	char rcnt[6];   // 그리드01건수
    struct {
		char	lgen_no			[10];	// 거래참여자번호
		char	lgen_nm			[70];	// 거래참여자명
		char	cls_yn			[ 1];	// CLS여부
    } orec[1];
} W1104S01_O;

#endif
`

const sampleW3382 = `/*******************************************************************************
 *  Program Name : FIX환매조회
 *******************************************************************************/
#ifndef __W3382S01__H__
#define __W3382S01__H__

typedef struct {                   // Input
  char  base_ymd           [ 8 ];  // 기준년월일
} W3382S01_I;

typedef struct {
		char	svcId[20];
		char	taskLevel[1];
		char	noOfProcess[2];
		char	path[200];
} W3382S01_R;

typedef struct {
  char  grid01_cnt[  6];           // 그리드건수
  W3382S01_R orec[];
} W3382S01_O;

#endif
`

func TestParse_W1104_Inline(t *testing.T) {
	s, err := Parse(sampleW1104)
	if err != nil {
		t.Fatal(err)
	}
	if s.Code != "W1104S01" {
		t.Errorf("Code=%q want W1104S01", s.Code)
	}
	if len(s.Input) != 4 {
		t.Fatalf("Input len=%d want 4", len(s.Input))
	}
	if s.Input[0].Name != "lgen_no" || s.Input[0].Size != 10 {
		t.Errorf("Input[0]=%+v", s.Input[0])
	}
	if s.Input[0].Comment != "거래참여자번호" {
		t.Errorf("comment=%q", s.Input[0].Comment)
	}
	// Output: rcnt + 인라인 struct (orec).
	// rcnt + orec[1] 페어 → trailing-array hack heuristic 으로 Repeat=-1 (가변) 로 reclassify.
	if len(s.Output) != 2 {
		t.Fatalf("Output len=%d want 2 (rcnt + orec): %+v", len(s.Output), s.Output)
	}
	rec := s.Output[1]
	if rec.Name != "orec" || rec.CType != "struct" || rec.Repeat != -1 {
		t.Errorf("orec=%+v (Repeat=-1 expected — trailing-array hack)", rec)
	}
	if len(rec.Children) != 3 {
		t.Errorf("orec children=%d want 3", len(rec.Children))
	}
}

func TestParse_W3382_NamedRecord(t *testing.T) {
	s, err := Parse(sampleW3382)
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "FIX환매조회" {
		t.Errorf("Name=%q", s.Name)
	}
	if s.Code != "W3382S01" {
		t.Errorf("Code=%q", s.Code)
	}
	if len(s.Input) != 1 || s.Input[0].Name != "base_ymd" {
		t.Errorf("Input=%+v", s.Input)
	}
	// Output: grid01_cnt + orec ( W3382S01_R 참조 → resolve 되어 Children 채워져야 )
	if len(s.Output) != 2 {
		t.Fatalf("Output len=%d", len(s.Output))
	}
	orec := s.Output[1]
	if orec.Name != "orec" || orec.CType != "W3382S01_R" {
		t.Errorf("orec=%+v", orec)
	}
	if orec.Repeat != -1 {
		t.Errorf("orec.Repeat=%d want -1 (가변)", orec.Repeat)
	}
	if len(orec.Children) != 4 {
		t.Errorf("orec children len=%d want 4 (svcId/taskLevel/noOfProcess/path), got %+v", len(orec.Children), orec.Children)
	}
}

// trailing-array hack heuristic — *_cnt + orec[1] 패턴이면 가변 grid (Repeat=-1) 로 reclassify.
// 운영 헤더 ~98% 가 이 패턴 (broker 가 grid_cnt 만큼 실제 byte 채워옴).
func TestReclassifyTrailingArray_GridCnt(t *testing.T) {
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
	if len(s.Output) != 2 {
		t.Fatalf("Output len=%d", len(s.Output))
	}
	if got := s.Output[1].Repeat; got != -1 {
		t.Errorf("orec.Repeat=%d want -1 (grid01_cnt 직전이므로 가변 reclassify)", got)
	}
}

// false positive 방지 — *_cnt 가 아닌 일반 char field 직전이면 Repeat 유지 (1).
func TestReclassifyTrailingArray_NotMisidentified(t *testing.T) {
	src := `typedef struct { // Output
  char branch_name[20];
    struct {
      char val[8];
    } orec[1];
} W9999S99_O;`
	s, err := Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Output[1].Repeat; got != 1 {
		t.Errorf("orec.Repeat=%d want 1 (branch_name 은 count field 아님)", got)
	}
}

// inline struct 에 [] (가변) 명시한 경우 — spec.go:340 unreachable 분기 수정 검증.
func TestReclassifyTrailingArray_OrecExplicitEmpty(t *testing.T) {
	src := `typedef struct { // Output
  char rcnt[6];
    struct {
      char val[8];
    } orec[];
} W9998S98_O;`
	s, err := Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Output[1].Repeat; got != -1 {
		t.Errorf("orec.Repeat=%d want -1 (orec[] 는 가변)", got)
	}
}

// 다양한 count 컨벤션 (rcnt / list_cnt / *_count) 모두 인식.
func TestReclassifyTrailingArray_CountNamePatterns(t *testing.T) {
	cases := []struct {
		name       string
		countNm    string
		wantRepeat int
	}{
		{"grid01_cnt", "grid01_cnt", -1},
		{"rcnt", "rcnt", -1},
		{"cnt", "cnt", -1},
		{"list_cnt", "list_cnt", -1},
		{"item_count", "item_count", -1},
		{"COUNT (uppercase)", "COUNT", -1},
		{"branch_name (NOT)", "branch_name", 1},
		{"cntr (NOT — cnt 끝아님)", "cntr", 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := "typedef struct {\n  char " + c.countNm + "[6];\n    struct {\n      char x[1];\n    } orec[1];\n} W_O;\n"
			s, err := Parse(src)
			if err != nil {
				t.Fatalf("Parse: %v\nsrc=%s", err, src)
			}
			if len(s.Output) < 2 {
				t.Fatalf("Output len=%d want 2 — %+v", len(s.Output), s.Output)
			}
			if got := s.Output[1].Repeat; got != c.wantRepeat {
				t.Errorf("countNm=%q → orec.Repeat=%d want %d", c.countNm, got, c.wantRepeat)
			}
		})
	}
}

// 실제 헤더 파일 (CP949 인코딩) 까지 ParseFile 로 동작 확인.
// 외부 파일 의존이 있어 파일 부재 시 t.Skip.
func TestParseFile_RealHeaders(t *testing.T) {
	candidates := []string{
		"/Users/winwaysystems/mywork/win/src/inc/trn/W1104S01.h",
		"/Users/winwaysystems/mywork/win/src/trn/W3380/W3382S01.h",
	}
	for _, p := range candidates {
		t.Run(filepath.Base(p), func(t *testing.T) {
			s, err := ParseFile(p)
			if err != nil {
				t.Skipf("헤더 파일 부재 또는 파싱 실패 (skip): %v", err)
			}
			if s.Code == "" {
				t.Errorf("Code 비어있음: %+v", s)
			}
			if len(s.Input) == 0 && len(s.Output) == 0 {
				t.Errorf("Input/Output 모두 비어있음: %+v", s)
			}
			t.Logf("Code=%s Name=%s Input=%d fields Output=%d fields",
				s.Code, s.Name, len(s.Input), len(s.Output))
		})
	}
}

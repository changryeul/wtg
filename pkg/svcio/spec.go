// Package svcio — 매매 svc 의 input/output 헤더파일 (`win/src/inc/trn/*.h`)
// 를 파싱해서 mci-admin UI 가 채널 무관하게 표시할 수 있는 metadata 로 변환.
//
// 헤더 컨벤션 (실측 831 개 표본):
//
//   - 파일명: WnnnnSnn.h (transaction code).
//   - 인코딩: CP949 (한글 주석). UTF-8 입력도 그대로 받는다.
//   - typedef 두 개: WnnnnSnn_I (Input), WnnnnSnn_O (Output).
//   - "Program Name :" 헤더 주석에 한글 설명. 옵션.
//   - field 형식: `<ctype> <name> [<size>]; // <comment>`
//   - nested grid: 인라인 `struct { ... } orec[N];` 또는 외부 `_R` typedef 참조.
//
// 본 1차 파서는 char/int/double 같은 기본 타입과 위 4 패턴만 다룬다 — 나머지는
// 사용자가 만나는 만큼 점진적으로 확장.
package svcio

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/transform"
)

// SvcSpec — 단일 매매 svc 의 I/O metadata. UI 가 채널 무관하게 보여주는 단위.
type SvcSpec struct {
	// Code 는 파일명에서 추출한 transaction 식별자 (예: "W1104S01").
	Code string `json:"code"`

	// Name 은 "Program Name :" 주석에서 뽑은 한글 설명. 없을 수 있다.
	Name string `json:"name,omitempty"`

	// SourcePath — 파싱 출처 파일 절대 경로. 디버그/감사용.
	SourcePath string `json:"source_path,omitempty"`

	// HeaderType — 이 svc 가 사용하는 공통 헤더 이름. "COMHDR" / "" (없음) 등.
	// wire frame = [HeaderFields][Input] 으로 구성.
	// 운영 svc (win/src/inc/trn) 는 default "COMHDR", dev svc (svc-headers) 는
	// 기본 "" (raw body). 헤더 파일 안에 `@wtg-header: NAME` 주석으로 override.
	HeaderType string `json:"header_type,omitempty"`

	// HeaderFields — HeaderType 으로 lookup 한 공통 헤더의 필드 트리. 응답
	// 편의를 위해 spec 자체에 inline 으로 채워서 반환 (UI 가 한 번에 표시 가능).
	HeaderFields []Field `json:"header_fields,omitempty"`

	// Input 은 _I struct 의 필드 트리. 비어있을 수 있다 (no-input 호출 svc).
	Input []Field `json:"input,omitempty"`

	// Output 은 _O struct 의 필드 트리.
	Output []Field `json:"output,omitempty"`

	// Records — 외부 named typedef (`_R` 등) — 외부 reference 해소를 위해 보관.
	// UI 표시는 Output 의 Children 에 inline 으로 풀린 형태가 우선이며, 이 맵은
	// 디버그 용도.
	Records map[string][]Field `json:"records,omitempty"`
}

// Field — Input/Output 한 칸. nested 면 Children 채워짐.
type Field struct {
	Name     string  `json:"name"`
	CType    string  `json:"ctype"`              // "char" / "int" / "double" / 외부 typedef 이름
	Size     int     `json:"size,omitempty"`     // [N] 의 N. char [10] → 10. nested 면 0
	Repeat   int     `json:"repeat,omitempty"`   // grid 1행 (orec[1]) → 1. orec[] (가변) → -1. 그 외 0
	Comment  string  `json:"comment,omitempty"`  // 한글 주석 (CP949 → UTF-8)
	Children []Field `json:"children,omitempty"` // nested struct field
}

// ParseFile — 헤더 파일을 읽어 SvcSpec 으로 변환.
//
// 인코딩은 자동 감지: 첫 K바이트가 valid UTF-8 이면 그대로, 아니면 CP949 변환.
// 파일이 없거나 typedef 하나도 못 찾으면 error.
func ParseFile(path string) (*SvcSpec, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text, err := decodeKorean(raw)
	if err != nil {
		return nil, err
	}
	spec, err := Parse(text)
	if err != nil {
		return nil, err
	}
	if spec.Code == "" {
		// 파일명 기반 fallback (확장자 제거).
		spec.Code = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	abs, _ := filepath.Abs(path)
	spec.SourcePath = abs
	return spec, nil
}

// Parse — 이미 UTF-8 로 변환된 헤더 텍스트를 SvcSpec 으로.
func Parse(text string) (*SvcSpec, error) {
	spec := &SvcSpec{Records: map[string][]Field{}}

	// "Program Name :" 라인. trailing `*`/공백 정리.
	if m := reProgramName.FindStringSubmatch(text); len(m) > 1 {
		spec.Name = cleanProgramName(m[1])
	}
	// `@wtg-header: NAME` marker — 공통 헤더 override (없으면 dir-default 적용).
	if m := reWtgHeader.FindStringSubmatch(text); len(m) > 1 {
		spec.HeaderType = strings.TrimSpace(m[1])
	}

	// 모든 typedef struct 블록 추출 + 블록명별 필드 파싱.
	blocks, err := extractTypedefBlocks(text)
	if err != nil {
		return nil, err
	}
	if len(blocks) == 0 {
		return nil, errors.New("svcio: typedef struct 블록을 찾지 못함")
	}

	for _, b := range blocks {
		fields, err := parseFields(b.body, spec.Records)
		if err != nil {
			return nil, err
		}
		switch {
		case strings.HasSuffix(b.name, "_I"):
			spec.Input = fields
			if spec.Code == "" {
				spec.Code = strings.TrimSuffix(b.name, "_I")
			}
		case strings.HasSuffix(b.name, "_O"):
			spec.Output = resolveRecords(fields, spec.Records)
			if spec.Code == "" {
				spec.Code = strings.TrimSuffix(b.name, "_O")
			}
		default:
			// _R 또는 다른 named record. spec.Records 에 보관.
			spec.Records[b.name] = fields
		}
	}

	// _O 가 _R 보다 먼저 파싱됐을 가능성 — 한 번 더 resolve.
	spec.Output = resolveRecords(spec.Output, spec.Records)

	// trailing-array hack 보정 — `*_cnt` + `orec[1]` 패턴이면 가변 grid 로 reclassify.
	// 운영 헤더의 ~98% 가 이 패턴 (orec[1] 선언 + grid_cnt 직전) 인데 wire 에는 N 행
	// 채워옴 → 1 행 고정으로 읽으면 N-1 행 손실. 본 후처리는 spec 에서 Repeat=-1 로
	// 미리 표시해서 wire.go 의 가변 grid 로직(직전 char field ASCII int) 이 동작하게.
	reclassifyTrailingArrays(spec.Input)
	reclassifyTrailingArrays(spec.Output)
	return spec, nil
}

// reclassifyTrailingArrays — fields 안의 (count_field, orec[1]) 페어를 검출해
// orec.Repeat 을 -1 (가변) 로 갱신. 재귀 — nested grid 안에 또 grid 가 있어도 처리.
//
// 검출 규칙:
//   - prev 가 children 없는 char-style 필드 (Children 비어있고 Size>0) +
//     이름이 isCountFieldName 패턴
//   - cur 가 nested struct (Children > 0) + 현재 Repeat == 1
//     → cur.Repeat = -1
func reclassifyTrailingArrays(fields []Field) {
	for i := range fields {
		if i > 0 {
			prev := fields[i-1]
			cur := &fields[i]
			if cur.Repeat == 1 && len(cur.Children) > 0 &&
				len(prev.Children) == 0 && prev.Size > 0 &&
				isCountFieldName(prev.Name) {
				cur.Repeat = -1
			}
		}
		if len(fields[i].Children) > 0 {
			reclassifyTrailingArrays(fields[i].Children)
		}
	}
}

// isCountFieldName — record-count field 작명 컨벤션 인식.
// 운영 헤더 sample 에 등장하는 패턴: grid01_cnt / grid02_cnt / rcnt / cnt /
// list_cnt / *_count 등.
func isCountFieldName(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return false
	}
	switch s {
	case "rcnt", "cnt", "count":
		return true
	}
	return strings.HasSuffix(s, "_cnt") || strings.HasSuffix(s, "_count")
}

// resolveRecords — Field.CType 이 spec.Records 의 key 와 일치하면 Children 으로 풀어 inline.
func resolveRecords(fields []Field, recs map[string][]Field) []Field {
	out := make([]Field, len(fields))
	for i, f := range fields {
		if children, ok := recs[f.CType]; ok && len(f.Children) == 0 {
			f.Children = resolveRecords(children, recs)
		} else if len(f.Children) > 0 {
			f.Children = resolveRecords(f.Children, recs)
		}
		out[i] = f
	}
	return out
}

// ─── 인코딩 ──────────────────────────────────────────────────────────────────

// DecodeKorean — exported wrapper for decodeKorean (admin source-edit endpoint
// 이 raw bytes 를 UTF-8 으로 변환할 때 사용).
func DecodeKorean(b []byte) (string, error) {
	return decodeKorean(b)
}

// decodeKorean — UTF-8 valid 면 그대로, 아니면 CP949(EUC-KR superset) 변환.
func decodeKorean(b []byte) (string, error) {
	if utf8.Valid(b) {
		return string(b), nil
	}
	r := transform.NewReader(bytes.NewReader(b), korean.EUCKR.NewDecoder())
	out, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// ─── typedef 블록 추출 ───────────────────────────────────────────────────────

type typedefBlock struct {
	name string
	body string
}

// extractTypedefBlocks — `typedef struct { ... } NAME[ ...];` 블록 배열 반환.
// nested struct 의 brace 도 같이 카운트해서 정확한 close 를 찾는다.
func extractTypedefBlocks(text string) ([]typedefBlock, error) {
	var out []typedefBlock
	for {
		idx := reTypedefStart.FindStringIndex(text)
		if idx == nil {
			break
		}
		// idx[1] 가 첫 `{` 다음 위치.
		bodyStart := idx[1]
		end, err := matchBrace(text, bodyStart-1)
		if err != nil {
			return nil, err
		}
		// `} NAME` 부분 추출 (end 다음의 `;` 직전 까지).
		afterClose := text[end+1:]
		nm := reBlockName.FindStringSubmatch(afterClose)
		if len(nm) < 2 {
			return nil, errors.New("svcio: typedef 닫는 NAME 추출 실패")
		}
		out = append(out, typedefBlock{
			name: nm[1],
			body: text[bodyStart:end],
		})
		// 다음 검색 위치 — close NAME 이후로 잘라낸다.
		text = afterClose[len(nm[0]):]
	}
	return out, nil
}

// matchBrace — text[openIdx]=='{' 가 짝지어지는 close brace 위치 반환.
func matchBrace(text string, openIdx int) (int, error) {
	if openIdx < 0 || openIdx >= len(text) || text[openIdx] != '{' {
		return -1, errors.New("svcio: matchBrace 시작 위치가 '{' 아님")
	}
	depth := 0
	inLineComment := false
	inBlockComment := false
	for i := openIdx; i < len(text); i++ {
		c := text[i]
		if inLineComment {
			if c == '\n' {
				inLineComment = false
			}
			continue
		}
		if inBlockComment {
			if c == '*' && i+1 < len(text) && text[i+1] == '/' {
				inBlockComment = false
				i++
			}
			continue
		}
		if c == '/' && i+1 < len(text) {
			if text[i+1] == '/' {
				inLineComment = true
				i++
				continue
			}
			if text[i+1] == '*' {
				inBlockComment = true
				i++
				continue
			}
		}
		if c == '{' {
			depth++
		} else if c == '}' {
			depth--
			if depth == 0 {
				return i, nil
			}
		}
	}
	return -1, errors.New("svcio: 닫는 brace 못 찾음")
}

// ─── 필드 파싱 ───────────────────────────────────────────────────────────────

// parseFields — typedef body 텍스트를 Field 배열로.
// nested `struct { ... } orec[N];` 도 처리.
func parseFields(body string, recordIndex map[string][]Field) ([]Field, error) {
	var out []Field
	// 토큰 단위 처리를 위해 line scan + brace stack.
	scanner := bufio.NewScanner(strings.NewReader(body))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		// 인라인 nested struct 시작.
		if reInlineStructOpen.MatchString(line) {
			// 본문을 모아서 close 까지 가져오고, 재귀적으로 parseFields.
			rest, name, repeat, err := readInlineStruct(scanner)
			if err != nil {
				return nil, err
			}
			children, err := parseFields(rest, recordIndex)
			if err != nil {
				return nil, err
			}
			out = append(out, Field{
				Name:     name,
				CType:    "struct",
				Repeat:   repeat,
				Children: children,
			})
			continue
		}
		// 일반 필드 라인.
		if f, ok := parseFieldLine(line); ok {
			out = append(out, f)
			continue
		}
		// 무시 가능한 라인 (주석만, 빈 brace 등) — silent skip.
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// readInlineStruct — `struct {` 이후 본문 + close 라인 ( `} NAME[REPEAT];` ) 을 합쳐 반환.
func readInlineStruct(scanner *bufio.Scanner) (body, name string, repeat int, err error) {
	depth := 1
	var sb strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		trim := strings.TrimSpace(line)
		// brace 카운트 — 한 줄 안에 여럿 있을 수 있으니 문자별 카운트.
		for i := 0; i < len(trim); i++ {
			if trim[i] == '{' {
				depth++
			} else if trim[i] == '}' {
				depth--
				if depth == 0 {
					// 닫는 라인. 이후 `} orec[1];` 해석.
					afterClose := trim[i+1:]
					m := reInlineStructClose.FindStringSubmatch(afterClose)
					if len(m) < 4 {
						return "", "", 0, errors.New("svcio: 인라인 struct close 패턴 불일치: " + afterClose)
					}
					name = m[1]
					// m[2] 가 비어있지 않으면 "[N]" 또는 "[]" 가 있는 것.
					// m[3] 은 그 안 N (빈 값이면 가변).
					if m[2] != "" {
						if m[3] == "" {
							repeat = -1 // "[]" — 가변
						} else {
							n, _ := strconv.Atoi(m[3])
							repeat = n
						}
					}
					// m[2] 비어있으면 "} orec;" — 괄호 없음. repeat=0 그대로
					// (wire.go 가 1 로 정규화).
					return sb.String(), name, repeat, nil
				}
			}
		}
		sb.WriteString(line)
		sb.WriteByte('\n')
	}
	return "", "", 0, errors.New("svcio: 인라인 struct close 미발견")
}

// parseFieldLine — `char name [size]; // comment` 또는 `TYPE_T name[];` 한 줄 파싱.
func parseFieldLine(line string) (Field, bool) {
	if m := reFieldArray.FindStringSubmatch(line); len(m) > 0 {
		size, _ := strconv.Atoi(m[3])
		f := Field{
			Name:    m[2],
			CType:   m[1],
			Size:    size,
			Comment: cleanComment(m[4]),
		}
		return f, true
	}
	if m := reFieldArrayUnsized.FindStringSubmatch(line); len(m) > 0 {
		// `TYPE name[];` — 가변 grid.
		f := Field{
			Name:    m[2],
			CType:   m[1],
			Repeat:  -1,
			Comment: cleanComment(m[3]),
		}
		return f, true
	}
	return Field{}, false
}

func cleanComment(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "//")
	return strings.TrimSpace(s)
}

func cleanProgramName(s string) string {
	s = strings.TrimSpace(s)
	// trailing `*` (주석 닫는 시각적 정렬) 들 제거.
	s = strings.TrimRight(s, "*/ \t")
	return strings.TrimSpace(s)
}

// ─── 정규식 ─────────────────────────────────────────────────────────────────

var (
	// `Program Name : XXXX`  (* 주석 안에서 매칭). multiline + 우측 trailing `*` 제거.
	reProgramName = regexp.MustCompile(`(?im)Program\s+Name\s*:\s*(.+)$`)

	// `@wtg-header: NAME` — 헤더 override marker (주석 어디든).
	reWtgHeader = regexp.MustCompile(`(?im)@wtg-header\s*:\s*([A-Za-z_][A-Za-z0-9_]*)`)

	// `typedef struct { ` 시작 — 직후 `{` 까지의 인덱스 반환을 위해 두 번째 group.
	reTypedefStart = regexp.MustCompile(`typedef\s+struct\s*\{`)

	// 닫는 brace 다음의 `WnnnnSnn_I/_O` 또는 `_R` 등 이름.
	reBlockName = regexp.MustCompile(`^\s*([A-Za-z_][A-Za-z0-9_]*)\b[^;]*;`)

	// `<type> <name> [<size>]; // <comment>`
	reFieldArray = regexp.MustCompile(`^\s*(\w+)\s+(\w+)\s*\[\s*(\d+)\s*\]\s*;\s*(?://(.*))?$`)

	// `<type> <name>[]; // <comment>` (가변).
	reFieldArrayUnsized = regexp.MustCompile(`^\s*(\w+)\s+(\w+)\s*\[\s*\]\s*;\s*(?://(.*))?$`)

	// 인라인 nested 의 시작 — `struct {`.
	reInlineStructOpen = regexp.MustCompile(`^\s*struct\s*\{`)

	// 인라인 nested 의 종료 — `} NAME[N];` / `} NAME[];` / `} NAME;`.
	//   group 1 = name
	//   group 2 = "[N]" 또는 "[]" 그대로 (괄호 자체 — 빈 값이면 괄호 미존재)
	//   group 3 = N (digits, 빈 값이면 [] 가변)
	// group 2/3 분리는 "[]" (가변) 와 괄호 미존재 (single struct) 의 모호성 제거용.
	reInlineStructClose = regexp.MustCompile(`\s*(\w+)(\s*\[\s*(\d*)\s*\])?\s*;`)
)

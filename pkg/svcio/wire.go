package svcio

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/transform"
)

// Wire 직렬화/역직렬화 — legacy cs native client (HTS/EMP) 가 broker 에 보내는
// byte 배치 그대로 만들어 mci 경유로 broker 에 송신, 응답 byte 를 Output layout
// 으로 다시 parse.
//
// 전문 인코딩 원칙 (2026-07-15 운영 결정): 엔진/DB 는 CP949, WTG 경계에서 변환.
//   - 송신: UTF-8(JSON) → CP949 로 인코딩해 char[N] 에 배치. 절단은 CP949
//     byte-width 기준 + 다중바이트 문자 분할 금지 (encodeWire).
//   - 수신: UTF-8 valid(=ASCII 등) 면 그대로, 아니면 CP949 → UTF-8 (decodeWire).
//   - 레거시 EUC-KR 클라의 raw 전문(octet-stream)은 이 경로가 아니라 무손상 통과.
//
// 1차 prototype 범위 (실측 830 헤더 압도적 다수가 이 패턴):
//   - char[N] 필드 — 우측 공백 fill
//   - 단일 nested struct (orec[1]) — children 직렬화 후 그대로 추가
//   - 가변 grid (orec[]) — Output 만 — rcnt 필드 (각 record 직전의 count) 로
//     반복 횟수 결정해서 parse
//
// 미지원 (필요 시 점진 확장):
//   - int / double / float (별도 endianness, alignment 결정 필요)
//   - bit field
//   - union

// Serialize — Input fields 를 layout 순서대로 byte buffer 로 직렬화.
//
// input map 의 key 는 Field.Name. 누락된 key 는 빈 문자열 (= 공백 fill).
// 값은 string 만 허용 (1차 prototype). nested grid 는 input[name] 이
// []map[string]interface{} 또는 단일 map 이면 1회 직렬화.
func Serialize(fields []Field, input map[string]interface{}) ([]byte, error) {
	var buf bytes.Buffer
	if err := writeFields(&buf, fields, input); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// SerializeWithHeader — [HeaderFields(headerInput)][Input(input)] 구조로 직렬화.
// 운영 svc 의 wire frame 형식. headerFields 가 비어있으면 Serialize 와 동일.
func SerializeWithHeader(headerFields []Field, headerInput map[string]interface{},
	inputFields []Field, input map[string]interface{}) ([]byte, error) {
	var buf bytes.Buffer
	if len(headerFields) > 0 {
		if err := writeFields(&buf, headerFields, headerInput); err != nil {
			return nil, err
		}
	}
	if err := writeFields(&buf, inputFields, input); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// DeserializeWithHeader — [HeaderFields][Output] 의 byte buffer 를 두 map 으로
// 분리해서 반환. 응답 first N byte 가 header, 나머지가 output.
// headerFields 가 비어있으면 header=nil + Output 만.
func DeserializeWithHeader(headerFields []Field, outputFields []Field, buf []byte) (
	header map[string]interface{}, output map[string]interface{}, err error) {
	cursor := 0
	if len(headerFields) > 0 {
		header = map[string]interface{}{}
		cursor, err = readFields(buf, 0, headerFields, header)
		if err != nil {
			return nil, nil, err
		}
	}
	output = map[string]interface{}{}
	if _, err = readFields(buf, cursor, outputFields, output); err != nil {
		return header, nil, err
	}
	return header, output, nil
}

func writeFields(buf *bytes.Buffer, fields []Field, input map[string]interface{}) error {
	for _, f := range fields {
		if len(f.Children) > 0 {
			// nested struct — orec[N] 또는 orec[].
			rep := f.Repeat
			if rep <= 0 {
				rep = 1 // input 미명시 시 1회
			}
			rows := nestedRows(input, f.Name)
			for i := 0; i < rep; i++ {
				var rowInput map[string]interface{}
				if i < len(rows) {
					rowInput = rows[i]
				}
				if err := writeFields(buf, f.Children, rowInput); err != nil {
					return err
				}
			}
			continue
		}
		// 단일 char[N] 필드.
		sz := f.Size
		if sz <= 0 {
			continue
		}
		v := strFromInput(input, f.Name)
		encoded := encodeWire(v, sz)
		buf.Write(encoded)
	}
	return nil
}

// Deserialize — Output fields 를 layout 순서대로 byte buffer 에서 parse 해서
// JSON-friendly map 반환. 가변 grid (Repeat == -1) 는 *바로 위* 의 char field
// 가 reccnt 라 가정 (예: "rcnt", "grid01_cnt", ..._CNT 등) — 그 값 ASCII int
// 로 record 횟수 결정.
//
// strict=false 면 buf 길이 부족해도 잘라서 진행 (legacy 응답 호환).
func Deserialize(fields []Field, buf []byte) (map[string]interface{}, error) {
	out := map[string]interface{}{}
	_, err := readFields(buf, 0, fields, out)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// readFields — buf 의 offset 부터 fields 를 순차 읽어서 out 채움. 진행한 byte 수 반환.
func readFields(buf []byte, off int, fields []Field, out map[string]interface{}) (int, error) {
	cursor := off
	var lastCntStr string // 직전 char field 의 정수 표현 — 가변 grid count.
	for _, f := range fields {
		if len(f.Children) > 0 {
			// 반복 횟수 결정.
			rep := f.Repeat
			if rep == -1 {
				// 가변 — 직전 *_cnt 류 필드의 ASCII int.
				n, _ := strconv.Atoi(strings.TrimSpace(lastCntStr))
				if n < 0 {
					n = 0
				}
				rep = n
			}
			if rep <= 0 {
				rep = 1
			}
			rows := make([]map[string]interface{}, 0, rep)
			for i := 0; i < rep; i++ {
				row := map[string]interface{}{}
				moved, err := readFields(buf, cursor, f.Children, row)
				if err != nil {
					return cursor, err
				}
				cursor = moved
				rows = append(rows, row)
			}
			out[f.Name] = rows
			lastCntStr = ""
			continue
		}
		sz := f.Size
		if sz <= 0 {
			continue
		}
		// buf 부족 — strict 아닌 모드: 빈 문자열로 채우고 cursor 만 진행 (legacy 호환).
		var raw []byte
		if cursor+sz <= len(buf) {
			raw = buf[cursor : cursor+sz]
		} else if cursor < len(buf) {
			raw = buf[cursor:]
		} else {
			raw = nil
		}
		decoded := decodeWire(raw)
		// 양측 공백/null trim — legacy 가 좌측 padding 으로 우측 정렬한 numeric
		// 필드 (rcnt 같은) 와 우측 padding 의 text 필드 모두 깨끗이 회복.
		decoded = strings.Trim(decoded, " \x00")
		out[f.Name] = decoded
		// 가변 grid count 후보로 기억 (이름이 *_cnt / rcnt / 끝이 cnt 든 아니든
		// "직전 필드" 라는 위치 규칙만 유지).
		lastCntStr = decoded
		cursor += sz
	}
	return cursor, nil
}

// ─── 헬퍼 ───────────────────────────────────────────────────────────────

func strFromInput(in map[string]interface{}, key string) string {
	if in == nil {
		return ""
	}
	if v, ok := in[key]; ok {
		switch t := v.(type) {
		case string:
			return t
		case json.Number:
			return t.String()
		case float64:
			// JSON 숫자 — int 처럼 보이게.
			if t == float64(int64(t)) {
				return strconv.FormatInt(int64(t), 10)
			}
			return strconv.FormatFloat(t, 'f', -1, 64)
		case bool:
			if t {
				return "Y"
			}
			return "N"
		case nil:
			return ""
		default:
			return fmt.Sprintf("%v", t)
		}
	}
	return ""
}

func nestedRows(in map[string]interface{}, key string) []map[string]interface{} {
	if in == nil {
		return nil
	}
	v, ok := in[key]
	if !ok {
		return nil
	}
	switch t := v.(type) {
	case []interface{}:
		out := make([]map[string]interface{}, 0, len(t))
		for _, row := range t {
			if m, ok := row.(map[string]interface{}); ok {
				out = append(out, m)
			}
		}
		return out
	case map[string]interface{}:
		return []map[string]interface{}{t}
	}
	return nil
}

// encodeWire — 전문 필드 값(UTF-8 문자열)을 CP949 로 인코딩해 char[N] 에 배치
// (우측 공백 fill). WTG 경계에서 JSON(UTF-8) → 엔진(CP949) 변환.
//
// rune 단위로 CP949 로 변환하며 누적해, char[N] 초과 시 다음 글자를 통째로
// 버린다 — CP949 다중바이트(한글 2byte) 문자가 반쪽으로 잘리지 않도록 보장.
// CP949 에 매핑 없는 rune 은 '?' 로 대체한다.
func encodeWire(s string, size int) []byte {
	out := make([]byte, size)
	for i := range out {
		out[i] = ' ' // 우측 공백 fill 기본값
	}
	enc := korean.EUCKR.NewEncoder()
	pos := 0
	for _, r := range s {
		cb, err := enc.Bytes([]byte(string(r)))
		if err != nil || len(cb) == 0 {
			cb = []byte{'?'} // CP949 미매핑 rune 대체
		}
		if pos+len(cb) > size {
			break // 남은 자리에 이 글자가 온전히 안 들어감 → 절단 (분할 금지)
		}
		copy(out[pos:], cb)
		pos += len(cb)
	}
	return out
}

// decodeWire — 수신 필드 bytes 를 문자열로. UTF-8 valid 면 그대로 (통일 원칙),
// 아니면 CP949(EUC-KR superset) 변환 시도 (레거시 응답 호환). 그마저 실패하면
// raw byte string 그대로 (손상 없이 통과).
func decodeWire(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	if utf8.Valid(b) {
		return string(b)
	}
	r := transform.NewReader(bytes.NewReader(b), korean.EUCKR.NewDecoder())
	out, err := io.ReadAll(r)
	if err != nil {
		return string(b)
	}
	return string(out)
}

// ErrSpecRequired — Serialize/Deserialize 에 spec 비어있을 때.
var ErrSpecRequired = errors.New("svcio: SvcSpec 의 fields 가 비어있음")

package svcio

import (
	"encoding/json"
	"testing"
)

func sampleSpec() *SvcSpec {
	return &SvcSpec{
		Code: "W3500S01",
		Name: "체결내역 조회",
		Input: []Field{
			{Name: "ordnNo", CType: "char", Size: 20, Comment: "주문번호"},
			{Name: "cnt", CType: "int", Comment: "건수"},
			{Name: "rate", CType: "double", Comment: "환율"},
			{Name: "irec", CType: "struct", Repeat: -1, Comment: "가변 grid", Children: []Field{
				{Name: "tnrId", CType: "char", Size: 3, Comment: "테너"},
			}},
		},
		Output: []Field{
			{Name: "crncPairId", CType: "char", Size: 7, Comment: "통화쌍"},
			{Name: "orec", CType: "struct", Repeat: 1, Comment: "1행 grid", Children: []Field{
				{Name: "prc", CType: "char", Size: 16, Comment: "가격"},
			}},
		},
	}
}

func TestFieldToSchema(t *testing.T) {
	cases := []struct {
		name       string
		f          Field
		wantType   string
		wantFormat string
	}{
		{"char → string+maxLength", Field{Name: "x", CType: "char", Size: 10}, "string", ""},
		{"int → integer", Field{Name: "x", CType: "int"}, "integer", ""},
		{"long → integer", Field{Name: "x", CType: "long"}, "integer", ""},
		{"double → number", Field{Name: "x", CType: "double"}, "number", ""},
		{"가변 array (Repeat -1)", Field{Name: "x", CType: "struct", Repeat: -1, Children: []Field{{Name: "y", CType: "char", Size: 2}}}, "array", ""},
		{"grid 1행 (Repeat 1) 도 array", Field{Name: "x", CType: "struct", Repeat: 1, Children: []Field{{Name: "y", CType: "char", Size: 2}}}, "array", ""},
		{"nested struct → object", Field{Name: "x", CType: "struct", Children: []Field{{Name: "y", CType: "char", Size: 2}}}, "object", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc := fieldToSchema(tc.f)
			if sc.Type != tc.wantType {
				t.Fatalf("type=%q want %q", sc.Type, tc.wantType)
			}
		})
	}

	// char maxLength 반영 + comment → description
	sc := fieldToSchema(Field{Name: "x", CType: "char", Size: 20, Comment: "주문번호"})
	if sc.MaxLength == nil || *sc.MaxLength != 20 {
		t.Fatalf("maxLength 미반영: %+v", sc.MaxLength)
	}
	if sc.Description != "주문번호" {
		t.Fatalf("description=%q", sc.Description)
	}
}

func TestBuildOpenAPI(t *testing.T) {
	specs := []*SvcSpec{sampleSpec()}
	doc := BuildOpenAPI(specs, OpenAPIOptions{
		Title:   "WTG 매매 API",
		Version: "1.0.0",
		Server:  "https://api.example.com",
		AliasFor: func(code string) string {
			return code // alias = code (routing 미해석 fallback)
		},
	})

	if doc.OpenAPI != "3.0.3" {
		t.Fatalf("openapi=%q", doc.OpenAPI)
	}
	if doc.Info.Title != "WTG 매매 API" {
		t.Fatalf("title=%q", doc.Info.Title)
	}
	// 가상 path: /v1/tx#W3500S01 로 TR 별 분리
	path, ok := doc.Paths["/v1/tx#W3500S01"]
	if !ok {
		t.Fatalf("가상 path 없음: keys=%v", pathKeys(doc.Paths))
	}
	if path.Post == nil {
		t.Fatal("POST operation 없음")
	}
	if path.Post.OperationID != "W3500S01" {
		t.Fatalf("operationId=%q", path.Post.OperationID)
	}
	// requestBody 에 alias 고정 + data 스키마
	rb := path.Post.RequestBody
	if rb == nil {
		t.Fatal("requestBody 없음")
	}
	body := rb.Content["application/json"].Schema
	aliasProp, ok := body.Properties["alias"]
	if !ok || aliasProp.Enum == nil || len(aliasProp.Enum) != 1 || aliasProp.Enum[0] != "W3500S01" {
		t.Fatalf("alias enum 고정 실패: %+v", aliasProp)
	}
	dataProp, ok := body.Properties["data"]
	if !ok || dataProp.Type != "object" {
		t.Fatalf("data object 아님: %+v", dataProp)
	}
	// data.input.irec 가 array
	irec, ok := dataProp.Properties["irec"]
	if !ok || irec.Type != "array" {
		t.Fatalf("irec array 아님: %+v", irec)
	}
	// 응답 스키마 (200)
	resp, ok := path.Post.Responses["200"]
	if !ok {
		t.Fatal("200 응답 없음")
	}
	outSchema := resp.Content["application/json"].Schema
	dataOut, ok := outSchema.Properties["data"]
	if !ok {
		t.Fatalf("응답 data 래핑 누락: %+v", outSchema.Properties)
	}
	if _, ok := dataOut.Properties["crncPairId"]; !ok {
		t.Fatalf("output 스키마 누락: %+v", dataOut.Properties)
	}

	// 직렬화 왕복 (표준 OpenAPI 파서가 읽을 수 있는 JSON)
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back["openapi"] != "3.0.3" {
		t.Fatalf("왕복 실패")
	}
}

func TestBuildOpenAPI_Empty(t *testing.T) {
	doc := BuildOpenAPI(nil, OpenAPIOptions{Title: "x", Version: "1"})
	if doc.Paths == nil || len(doc.Paths) != 0 {
		t.Fatalf("빈 spec 은 빈 paths: %v", doc.Paths)
	}
}

func pathKeys(m map[string]PathItem) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

func TestCodeMatch(t *testing.T) {
	cases := []struct {
		filter, code string
		want         bool
	}{
		{"W35*", "W3500S01", true},
		{"W35*", "W3200S01", false},
		{"w35*", "W3500S01", true}, // 대소문자 무시
		{"W3200S01", "W3200S01", true},
		{"W3200S01", "W3200S02", false},
		{"W35*,W3200S01", "W3200S01", true},
		{"W35*,W3200S01", "W3500S09", true},
		{"", "W3500S01", false},
	}
	for _, c := range cases {
		if got := CodeMatch(c.filter, c.code); got != c.want {
			t.Fatalf("CodeMatch(%q,%q)=%v want %v", c.filter, c.code, got, c.want)
		}
	}
}

func TestBuildOpenAPI_HeaderParams(t *testing.T) {
	doc := BuildOpenAPI([]*SvcSpec{sampleSpec()}, OpenAPIOptions{Title: "x", Version: "1"})
	op := doc.Paths["/v1/tx#W3500S01"].Post
	if len(op.Parameters) == 0 {
		t.Fatal("헤더 파라미터 없음 — Swagger UI 에서 편집 불가")
	}
	names := map[string]Parameter{}
	for _, p := range op.Parameters {
		names[p.Name] = p
	}
	for _, want := range []string{"X-WTG-User", "X-WTG-Channel"} {
		p, ok := names[want]
		if !ok {
			t.Fatalf("헤더 파라미터 %s 누락", want)
		}
		if p.In != "header" {
			t.Fatalf("%s in=%q, want header", want, p.In)
		}
		if p.Schema == nil || p.Schema.Type != "string" {
			t.Fatalf("%s 스키마 불량: %+v", want, p.Schema)
		}
	}
}

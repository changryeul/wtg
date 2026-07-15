package svcio

import "strings"

// OpenAPI 3.0 생성기 — svcio SvcSpec 트리를 OpenAPI 문서로 변환한다.
// 클라이언트 개발자 전달용 (Swagger UI / Postman / codegen 소비).
//
// API 형태: 실제 endpoint 는 POST /v1/tx 하나이고 alias 로 TR 을 구분한다.
// OpenAPI 는 (path, method) 가 유일해야 하므로 TR 별로 가상 path
// "/v1/tx#<code>" 로 갈라 Swagger UI 에서 TR 마다 보이게 한다 (fragment
// 뒤는 서버가 무시 — 실제 호출은 /v1/tx). 문서 description 에 명시.

// OpenAPIOptions — 생성 파라미터.
type OpenAPIOptions struct {
	Title   string
	Version string
	Server  string // 예: "https://api.example.com" (비면 servers 생략)
	// AliasFor 는 svc code → routing alias 해석. nil 이면 code 를 그대로 alias 로.
	AliasFor func(code string) string
}

// ── OpenAPI 문서 모델 (필요 최소 subset) ──

type OpenAPIDoc struct {
	OpenAPI    string              `json:"openapi"`
	Info       OpenAPIInfo         `json:"info"`
	Servers    []OpenAPIServer     `json:"servers,omitempty"`
	Paths      map[string]PathItem `json:"paths"`
	Components *Components         `json:"components,omitempty"`
}

type OpenAPIInfo struct {
	Title       string `json:"title"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
}

type OpenAPIServer struct {
	URL string `json:"url"`
}

type PathItem struct {
	Post *Operation `json:"post,omitempty"`
}

type Operation struct {
	OperationID string                `json:"operationId"`
	Summary     string                `json:"summary,omitempty"`
	Description string                `json:"description,omitempty"`
	Tags        []string              `json:"tags,omitempty"`
	Parameters  []Parameter           `json:"parameters,omitempty"`
	RequestBody *RequestBody          `json:"requestBody,omitempty"`
	Responses   map[string]Response   `json:"responses"`
	Security    []map[string][]string `json:"security,omitempty"`
}

// Parameter — OpenAPI 파라미터 (여기선 편집 가능한 header 용).
type Parameter struct {
	Name        string  `json:"name"`
	In          string  `json:"in"`
	Description string  `json:"description,omitempty"`
	Required    bool    `json:"required,omitempty"`
	Schema      *Schema `json:"schema,omitempty"`
}

type RequestBody struct {
	Required bool                 `json:"required,omitempty"`
	Content  map[string]MediaType `json:"content"`
}

type Response struct {
	Description string               `json:"description"`
	Content     map[string]MediaType `json:"content,omitempty"`
}

type MediaType struct {
	Schema *Schema `json:"schema,omitempty"`
}

type Components struct {
	SecuritySchemes map[string]SecurityScheme `json:"securitySchemes,omitempty"`
}

type SecurityScheme struct {
	Type         string `json:"type"`
	Scheme       string `json:"scheme,omitempty"`
	BearerFormat string `json:"bearerFormat,omitempty"`
}

// Schema — JSON Schema (OpenAPI subset).
type Schema struct {
	Type        string             `json:"type,omitempty"`
	Format      string             `json:"format,omitempty"`
	Description string             `json:"description,omitempty"`
	MaxLength   *int               `json:"maxLength,omitempty"`
	Enum        []string           `json:"enum,omitempty"`
	Properties  map[string]*Schema `json:"properties,omitempty"`
	Items       *Schema            `json:"items,omitempty"`
	Example     any                `json:"example,omitempty"`
}

// fieldToSchema 는 svcio Field 1칸을 JSON Schema 로 변환한다.
func fieldToSchema(f Field) *Schema {
	// 배열 (grid): Repeat != 0 (1행 grid, 가변 배열 모두) → array of item.
	if f.Repeat != 0 {
		item := structSchema(f.Children)
		return &Schema{Type: "array", Description: f.Comment, Items: item}
	}
	// nested struct (children 있고 배열 아님) → object.
	if len(f.Children) > 0 {
		s := structSchema(f.Children)
		s.Description = f.Comment
		return s
	}
	// scalar.
	s := &Schema{Description: f.Comment}
	switch f.CType {
	case "int", "long", "short", "unsigned":
		s.Type = "integer"
	case "double", "float":
		s.Type = "number"
	default: // char 및 기타 → 고정폭 문자열
		s.Type = "string"
		if f.Size > 0 {
			n := f.Size
			s.MaxLength = &n
		}
	}
	return s
}

// structSchema 는 Field 슬라이스를 object 스키마로 만든다.
func structSchema(fields []Field) *Schema {
	s := &Schema{Type: "object", Properties: map[string]*Schema{}}
	for _, f := range fields {
		s.Properties[f.Name] = fieldToSchema(f)
	}
	return s
}

// BuildOpenAPI 는 SvcSpec 슬라이스를 OpenAPI 문서로 변환한다 (순수 함수).
func BuildOpenAPI(specs []*SvcSpec, opts OpenAPIOptions) OpenAPIDoc {
	doc := OpenAPIDoc{
		OpenAPI: "3.0.3",
		Info: OpenAPIInfo{
			Title:   opts.Title,
			Version: opts.Version,
			Description: "실제 endpoint 는 `POST /v1/tx` 하나이며, 각 TR 은 " +
				"요청 body 의 `alias` 로 구분한다. 아래 경로의 `#<code>` 는 " +
				"Swagger UI 표시용 가상 구분자로 서버는 무시한다.",
		},
		Paths: map[string]PathItem{},
	}
	if opts.Server != "" {
		doc.Servers = []OpenAPIServer{{URL: opts.Server}}
	}
	doc.Components = &Components{SecuritySchemes: map[string]SecurityScheme{
		"bearerAuth": {Type: "http", Scheme: "bearer", BearerFormat: "JWT"},
	}}

	aliasFor := opts.AliasFor
	if aliasFor == nil {
		aliasFor = func(code string) string { return code }
	}

	for _, spec := range specs {
		if spec == nil {
			continue
		}
		alias := aliasFor(spec.Code)

		// 요청 body: {alias: <고정>, data: {input 필드}}
		dataSchema := structSchema(spec.Input)
		dataSchema.Description = "입력 전문 (svc I/O 명세 기반, 고정폭 자동 조립)"
		reqSchema := &Schema{
			Type: "object",
			Properties: map[string]*Schema{
				"alias": {Type: "string", Enum: []string{alias},
					Description: "routing alias (이 TR 고정값)"},
				"data": dataSchema,
			},
		}
		// 응답 body: {errn, data: {output 필드}}
		outSchema := structSchema(spec.Output)
		respSchema := &Schema{
			Type: "object",
			Properties: map[string]*Schema{
				"errn": {Type: "integer", Description: "엔진 에러코드 (0=정상)"},
				"data": outSchema,
			},
		}

		op := &Operation{
			OperationID: spec.Code,
			Summary:     spec.Code + " " + spec.Name,
			Description: "실제 호출: `POST /v1/tx`, body 의 alias=`" + alias + "`.",
			Tags:        []string{tagOf(spec.Code)},
			Parameters:  commonHeaders(),
			Security:    []map[string][]string{{"bearerAuth": {}}},
			RequestBody: &RequestBody{
				Required: true,
				Content:  map[string]MediaType{"application/json": {Schema: reqSchema}},
			},
			Responses: map[string]Response{
				"200": {
					Description: "정상 응답 (errn≠0 이어도 200 — 비즈니스 에러는 errn 으로 판단)",
					Content:     map[string]MediaType{"application/json": {Schema: respSchema}},
				},
			},
		}
		doc.Paths["/v1/tx#"+spec.Code] = PathItem{Post: op}
	}
	return doc
}

// tagOf 는 code 앞 5자 (W + 4자리 그룹) 를 태그로 — Swagger UI 그룹핑.
func tagOf(code string) string {
	if len(code) >= 5 {
		return code[:5]
	}
	return code
}

// CodeMatch 는 codes 필터 (콤마 구분, 후행 * 와일드카드) 가 code 에 맞는지.
// 예: "W35*,W3200S01" — 대소문자 무시.
func CodeMatch(filter, code string) bool {
	code = strings.ToUpper(code)
	for _, pat := range splitComma(filter) {
		pat = strings.ToUpper(pat)
		if pat == "" {
			continue
		}
		if pat[len(pat)-1] == '*' {
			if strings.HasPrefix(code, pat[:len(pat)-1]) {
				return true
			}
		} else if code == pat {
			return true
		}
	}
	return false
}

func splitComma(s string) []string {
	out := []string{}
	cur := ""
	for _, c := range s {
		if c == ',' {
			out = append(out, strings.TrimSpace(cur))
			cur = ""
		} else {
			cur += string(c)
		}
	}
	return append(out, strings.TrimSpace(cur))
}

// commonHeaders 는 /v1/tx 호출에 붙는 편집 가능한 header 파라미터.
// Swagger UI 의 Try it out 에서 값을 직접 보고 수정할 수 있게 노출한다.
// (Authorization 은 bearerAuth security scheme 의 Authorize 버튼으로 별도 입력)
func commonHeaders() []Parameter {
	return []Parameter{
		{Name: "X-WTG-User", In: "header",
			Description: "DevMode 사용자 ID (JWT 미사용 시). 운영은 Authorize 의 Bearer 토큰 사용",
			Schema:      &Schema{Type: "string"}},
		{Name: "X-WTG-Channel", In: "header",
			Description: "채널 코드 (예: HTS / WEB). 생략 가능",
			Schema:      &Schema{Type: "string"}},
	}
}

package svcio

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Registry — 헤더 디렉터리에서 파싱한 SvcSpec 의 in-memory 인덱스.
//
// mci-admin 부팅 시 한 번 Load 해서 메모리에 보관. 헤더 변경 시 SIGHUP 핸들러나
// 재시작으로 반영 (런타임 watch 는 Phase 3-future).
type Registry struct {
	mu      sync.RWMutex
	specs   map[string]*SvcSpec
	headers map[string][]Field // 공통 헤더 (예: "COMHDR" → field 트리)
	// dirHeaderDefaults — 디렉터리 prefix 별 default header 이름.
	// LoadDir 시 디렉터리 path 가 어느 prefix 에 매칭되는지 검사하고, spec 의
	// HeaderType 이 비어 있으면 이 default 로 채운다 (svc 별 override 가능).
	dirHeaderDefaults map[string]string
}

// SvcSummary — 목록 화면용 가벼운 표현.
type SvcSummary struct {
	Code         string `json:"code"`
	Name         string `json:"name,omitempty"`
	HasInput     bool   `json:"has_input"`
	HasOutput    bool   `json:"has_output"`
	InputFields  int    `json:"input_fields"`
	OutputFields int    `json:"output_fields"`
	// Dev — 헤더가 dev svc 디렉터리 (`svc-headers`) 에서 왔는지. true 면 dev
	// broker 에 svc 가 떠있을 가능성이 높아 wire 테스트가 동작. false 면 운영
	// spec 만 있고 dev broker 에 svc 가 없을 가능성 — wire 호출 시 MB-1002.
	Dev bool `json:"dev"`
}

// NewRegistry — 빈 Registry 반환.
func NewRegistry() *Registry {
	return &Registry{
		specs:             map[string]*SvcSpec{},
		headers:           map[string][]Field{},
		dirHeaderDefaults: map[string]string{},
	}
}

// SetDirHeaderDefault — 특정 디렉터리 prefix 의 default 헤더 이름 등록.
// path 가 해당 prefix 로 시작하는 모든 spec 의 HeaderType 이 비어있으면
// 이 default 로 설정된다 (override 는 spec 의 `@wtg-header:` 주석으로).
func (r *Registry) SetDirHeaderDefault(dirPrefix, headerName string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dirHeaderDefaults[dirPrefix] = headerName
}

// RegisterHeader — 공통 헤더의 fields 를 등록. svc spec 에서 HeaderType 으로
// lookup. 헤더 파일 자체에서 파싱한 결과를 저장하는 게 일반적 (loadHeaderFile).
func (r *Registry) RegisterHeader(name string, fields []Field) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.headers == nil {
		r.headers = map[string][]Field{}
	}
	r.headers[name] = fields
}

// LoadHeaderFile — 헤더 파일 (예: comhdr.h) 을 파싱해서 그 안의 *모든* typedef
// struct 를 named header 로 등록. 한 파일에 여러 헤더 (COMHDR / BROADCAST_H /
// CHGHDR 등) 있을 수 있다.
func (r *Registry) LoadHeaderFile(path string, logger *slog.Logger) error {
	if path == "" {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			logger.Info("svcio: 공통 헤더 파일 없음 — skip", slog.String("path", path))
			return nil
		}
		return err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	text, err := decodeKorean(raw)
	if err != nil {
		return err
	}
	blocks, err := extractTypedefBlocks(text)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.headers == nil {
		r.headers = map[string][]Field{}
	}
	for _, b := range blocks {
		fields, ferr := parseFields(b.body, map[string][]Field{})
		if ferr != nil {
			logger.Warn("svcio: 공통 헤더 typedef 파싱 실패",
				slog.String("name", b.name), slog.Any("err", ferr))
			continue
		}
		r.headers[b.name] = fields
	}
	logger.Info("svcio: 공통 헤더 등록",
		slog.String("path", path), slog.Int("count", len(blocks)))
	return nil
}

// HeaderFields — name 으로 등록된 공통 헤더의 fields 반환. 미등록이면 nil.
func (r *Registry) HeaderFields(name string) []Field {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if name == "" || r.headers == nil {
		return nil
	}
	return r.headers[name]
}

// HeaderEntry — UI 표시용 named header 한 개 정보.
type HeaderEntry struct {
	Name       string  `json:"name"`
	Fields     []Field `json:"fields"`
	Size       int     `json:"size"`        // 모든 필드의 byte 합
	FieldCount int     `json:"field_count"` // top-level 필드 개수
}

// ListHeaders — 등록된 모든 공통 헤더 entry, 이름 알파벳 순.
func (r *Registry) ListHeaders() []HeaderEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]HeaderEntry, 0, len(r.headers))
	for name, fields := range r.headers {
		size := 0
		for _, f := range fields {
			size += f.Size
		}
		out = append(out, HeaderEntry{
			Name:       name,
			Fields:     fields,
			Size:       size,
			FieldCount: len(fields),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// LoadDirs — 콤마 구분 다중 디렉터리 + 단일 디렉터리 모두 받음.
// 같은 code 가 여러 디렉터리에 있으면 *나중* 디렉터리가 이김 — dev 헤더가 운영
// 헤더를 override 하는 패턴 (예: 운영 W1104 + dev ECHOSVC_PING).
func (r *Registry) LoadDirs(spec string, logger *slog.Logger) (loaded, failed int, err error) {
	if spec == "" {
		return 0, 0, nil
	}
	for _, p := range strings.Split(spec, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		l, f, e := r.LoadDir(p, logger)
		loaded += l
		failed += f
		if e != nil {
			return loaded, failed, e
		}
	}
	return loaded, failed, nil
}

// LoadDir — dir 안의 모든 *.h 를 파싱해서 등록. (loaded, failed, err) 반환.
// dir 가 비어있으면 noop. 파일 부재는 정상 — 운영에서 헤더가 매핑되지 않을 수
// 있으므로 (보안 격리 등) 부팅을 막지 않는다.
func (r *Registry) LoadDir(dir string, logger *slog.Logger) (int, int, error) {
	if dir == "" {
		return 0, 0, nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			logger.Info("svcio: 헤더 디렉터리 부재 — skip", slog.String("dir", dir))
			return 0, 0, nil
		}
		return 0, 0, err
	}

	matches, err := filepath.Glob(filepath.Join(dir, "*.h"))
	if err != nil {
		return 0, 0, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	loaded, failed := 0, 0
	for _, p := range matches {
		spec, perr := ParseFile(p)
		if perr != nil {
			failed++
			logger.Debug("svcio: 헤더 파싱 실패",
				slog.String("path", p), slog.Any("err", perr))
			continue
		}
		if spec.Code == "" {
			failed++
			continue
		}
		// HeaderType — spec 자신이 명시 안 했으면 디렉터리 default 적용.
		if spec.HeaderType == "" {
			for prefix, name := range r.dirHeaderDefaults {
				if strings.Contains(spec.SourcePath, prefix) {
					spec.HeaderType = name
					break
				}
			}
		}
		// HeaderFields inline — 등록된 헤더면 fields 채움 (UI 에서 한 번에 보임).
		if spec.HeaderType != "" {
			if fields, ok := r.headers[spec.HeaderType]; ok {
				spec.HeaderFields = fields
			}
		}
		r.specs[spec.Code] = spec
		loaded++
	}
	logger.Info("svcio: 헤더 인덱싱 완료",
		slog.String("dir", dir),
		slog.Int("loaded", loaded),
		slog.Int("failed", failed),
		slog.Int("total", len(matches)))
	return loaded, failed, nil
}

// Get — code 로 SvcSpec 조회. 미등록이면 (nil, false).
func (r *Registry) Get(code string) (*SvcSpec, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.specs[code]
	return s, ok
}

// List — 전체 svc 의 가벼운 summary, code 알파벳 순.
func (r *Registry) List() []SvcSummary {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]SvcSummary, 0, len(r.specs))
	for _, s := range r.specs {
		out = append(out, SvcSummary{
			Code:         s.Code,
			Name:         s.Name,
			HasInput:     len(s.Input) > 0,
			HasOutput:    len(s.Output) > 0,
			InputFields:  countFields(s.Input),
			OutputFields: countFields(s.Output),
			Dev:          isDevSpec(s),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out
}

// isDevSpec — SourcePath 가 dev svc 디렉터리 (`svc-headers`) 안에 있으면 true.
// 운영 헤더 (예: win/src/inc/trn) 는 false.
func isDevSpec(s *SvcSpec) bool {
	return s != nil && strings.Contains(s.SourcePath, "svc-headers")
}

// Search — code/name prefix 일치 (case insensitive). max 0 이면 전체.
func (r *Registry) Search(q string, max int) []SvcSummary {
	q = strings.ToLower(strings.TrimSpace(q))
	all := r.List()
	if q == "" {
		if max > 0 && len(all) > max {
			return all[:max]
		}
		return all
	}
	out := make([]SvcSummary, 0, 32)
	for _, s := range all {
		if strings.Contains(strings.ToLower(s.Code), q) ||
			strings.Contains(strings.ToLower(s.Name), q) {
			out = append(out, s)
			if max > 0 && len(out) >= max {
				break
			}
		}
	}
	return out
}

// Count — 등록된 spec 개수.
func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.specs)
}

// ReloadFile — 단일 헤더 파일 하나만 재파싱해서 registry 갱신.
// 파일 편집 후 즉시 반영용. ParseFile 실패 시 기존 entry 유지하고 err 반환.
// HeaderType / HeaderFields 도 LoadDir 와 동일 로직으로 채운다.
func (r *Registry) ReloadFile(path string) (*SvcSpec, error) {
	spec, err := ParseFile(path)
	if err != nil {
		return nil, err
	}
	if spec.Code == "" {
		return nil, errSpecNoCode
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if spec.HeaderType == "" {
		for prefix, name := range r.dirHeaderDefaults {
			if strings.Contains(spec.SourcePath, prefix) {
				spec.HeaderType = name
				break
			}
		}
	}
	if spec.HeaderType != "" {
		if fields, ok := r.headers[spec.HeaderType]; ok {
			spec.HeaderFields = fields
		}
	}
	r.specs[spec.Code] = spec
	return spec, nil
}

var errSpecNoCode = errors.New("svcio: 파싱된 spec 의 Code 가 비어있음")

// countFields — 트리 깊이 합. nested grid 의 children 도 카운트.
func countFields(fs []Field) int {
	n := 0
	for _, f := range fs {
		n++
		n += countFields(f.Children)
	}
	return n
}

// ErrNotFound — Get 의 명시적 에러형 (caller 가 errors.Is 로 체크 가능).
var ErrNotFound = errors.New("svcio: code 미등록")

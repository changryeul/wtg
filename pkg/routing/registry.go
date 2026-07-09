// Package routing 은 transaction alias → MyMQ (exchange, routing_key) 매핑.
//
// 동기:
//
//   - 클라이언트는 짧고 안정적인 alias ("ORDER_NEW") 만 인지하고,
//     실제 broker 라우팅 (Exchange/RoutingKey) 은 운영팀이 동적으로 관리.
//   - blue-green / canary 전환 시 클라이언트 코드 변경 없이 alias 재매핑만.
//   - passthrough 원칙은 유지 — alias 가 등록되어 있지 않으면 envelope 의
//     exchange/routing_key 가 그대로 broker 로 전달된다.
//
// 1차 prototype 은 InMemoryRegistry 만 제공한다. 운영에서는 etcd / Postgres
// 등으로 차환하되, Registry 인터페이스가 동일하면 호출자 변경은 필요 없다.
package routing

import (
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/winwaysystems/wtg/pkg/mymq"
)

// Rule 은 단일 alias 매핑.
type Rule struct {
	Alias      string    `json:"alias"`
	Exchange   string    `json:"exchange"`
	RoutingKey string    `json:"routing_key"`
	Active     bool      `json:"active"`
	Comment    string    `json:"comment,omitempty"`
	UpdatedAt  time.Time `json:"updated_at"`
	UpdatedBy  string    `json:"updated_by,omitempty"` // admin Usid
}

// IsPattern — trailing "*" 를 가진 prefix 패턴 rule 인지.
// 예: "W11*" 는 W11 로 시작하는 모든 alias 를 커버 (도메인 단위 노출 정책).
func (r *Rule) IsPattern() bool {
	return strings.HasSuffix(r.Alias, "*")
}

// 표준 에러 sentinel.
var (
	ErrAliasRequired   = errors.New("routing: alias 필수")
	ErrAliasInvalid    = errors.New("routing: alias 형식 오류")
	ErrRouteNotFound   = errors.New("routing: alias 미등록")
	ErrExchangeTooLong = errors.New("routing: exchange 길이 초과")
	ErrRkeyTooLong     = errors.New("routing: routing_key 길이 초과")
	ErrRkeyRequired    = errors.New("routing: routing_key 필수")
)

// Validate 는 Rule 의 transport-level 정합성 검증.
//
// 비즈니스 규칙(어떤 exchange 가 허용 가능한가) 은 이 패키지가 모른다 —
// 운영자가 admin endpoint 로 직접 입력하므로 폼 검증만.
func (r *Rule) Validate() error {
	r.Alias = strings.TrimSpace(r.Alias)
	if r.Alias == "" {
		return ErrAliasRequired
	}
	if len(r.Alias) > 64 || strings.ContainsAny(r.Alias, " \t\r\n/") {
		return ErrAliasInvalid
	}
	// * 는 패턴 rule 의 trailing 1개만 허용 ("W11*"). 중간/복수 * 는 거부.
	if i := strings.IndexByte(r.Alias, '*'); i >= 0 && i != len(r.Alias)-1 {
		return ErrAliasInvalid
	}
	// 패턴 rule 은 rkey 생략 가능 — 빈값이면 Resolve 시 요청 alias 가 rkey.
	if r.RoutingKey == "" && !r.IsPattern() {
		return ErrRkeyRequired
	}
	if len(r.Exchange) > mymq.LXchg {
		return ErrExchangeTooLong
	}
	if len(r.RoutingKey) > mymq.LRkey {
		return ErrRkeyTooLong
	}
	return nil
}

// Registry 는 alias 매핑 저장소 인터페이스.
//
// 모든 메서드는 goroutine-safe.
type Registry interface {
	// Get 은 alias 로 룰을 조회한다. ErrRouteNotFound 가능.
	Get(alias string) (*Rule, error)

	// List 는 정렬된 alias 순서로 모든 룰을 반환한다.
	List() []*Rule

	// Put 은 룰을 저장 (생성 또는 수정). 검증 후 UpdatedAt 자동 갱신.
	// updatedBy 는 변경한 admin 의 Usid (감사 로깅용).
	Put(rule *Rule, updatedBy string) error

	// Delete 는 alias 를 제거. 미존재면 ErrRouteNotFound.
	Delete(alias string) error

	// SetActive 는 enable/disable 토글. 미존재면 ErrRouteNotFound.
	SetActive(alias string, active bool, updatedBy string) error
}

// InMemoryRegistry 는 sync.RWMutex 기반의 in-process Registry.
type InMemoryRegistry struct {
	now func() time.Time

	mu    sync.RWMutex
	rules map[string]*Rule
}

// NewInMemoryRegistry 는 빈 registry 를 만든다.
// nowFunc 가 nil 이면 time.Now.
func NewInMemoryRegistry(nowFunc func() time.Time) *InMemoryRegistry {
	if nowFunc == nil {
		nowFunc = time.Now
	}
	return &InMemoryRegistry{
		now:   nowFunc,
		rules: make(map[string]*Rule),
	}
}

func (r *InMemoryRegistry) Get(alias string) (*Rule, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rule, ok := r.rules[alias]
	if !ok {
		return nil, ErrRouteNotFound
	}
	cp := *rule
	return &cp, nil
}

func (r *InMemoryRegistry) List() []*Rule {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Rule, 0, len(r.rules))
	for _, v := range r.rules {
		cp := *v
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Alias < out[j].Alias })
	return out
}

func (r *InMemoryRegistry) Put(rule *Rule, updatedBy string) error {
	if rule == nil {
		return ErrAliasRequired
	}
	if err := rule.Validate(); err != nil {
		return err
	}
	cp := *rule
	cp.UpdatedAt = r.now()
	cp.UpdatedBy = updatedBy
	r.mu.Lock()
	r.rules[cp.Alias] = &cp
	r.mu.Unlock()
	return nil
}

func (r *InMemoryRegistry) Delete(alias string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.rules[alias]; !ok {
		return ErrRouteNotFound
	}
	delete(r.rules, alias)
	return nil
}

func (r *InMemoryRegistry) SetActive(alias string, active bool, updatedBy string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	rule, ok := r.rules[alias]
	if !ok {
		return ErrRouteNotFound
	}
	rule.Active = active
	rule.UpdatedAt = r.now()
	rule.UpdatedBy = updatedBy
	return nil
}

// Resolve 는 alias 를 활성 룰로 조회한다 — mci-api 의 transaction 핸들러가
// envelope 에 alias 가 있을 때 호출하는 편의 메서드.
//
// alias 가 등록되었지만 비활성이면 ErrRouteNotFound 와 동일하게 처리한다 —
// 호출자는 fallback 으로 envelope 의 raw exchange/routing_key 를 사용.
func Resolve(reg Registry, alias string) (*Rule, error) {
	if reg == nil || alias == "" {
		return nil, ErrRouteNotFound
	}
	rule, err := reg.Get(alias)
	if err == nil {
		if !rule.Active {
			return nil, ErrRouteNotFound
		}
		return rule, nil
	}
	if !errors.Is(err, ErrRouteNotFound) {
		return nil, err
	}
	// 정확 매칭 없음 — 패턴 rule (trailing *) 로 fallback. 가장 긴 prefix 승리.
	// rkey 빈 패턴은 요청 alias 를 rkey 로 (svc code 계열 일괄 노출).
	var best *Rule
	for _, r := range reg.List() {
		if !r.Active || !r.IsPattern() {
			continue
		}
		prefix := strings.TrimSuffix(r.Alias, "*")
		if !strings.HasPrefix(alias, prefix) {
			continue
		}
		if best == nil || len(prefix) > len(strings.TrimSuffix(best.Alias, "*")) {
			best = r
		}
	}
	if best == nil {
		return nil, ErrRouteNotFound
	}
	resolved := *best
	resolved.Alias = alias
	if resolved.RoutingKey == "" {
		resolved.RoutingKey = alias
	}
	// 패턴 유래 rkey 는 Put 검증을 안 거치므로 wire 한도 방어.
	if len(resolved.RoutingKey) > mymq.LRkey {
		return nil, ErrRkeyTooLong
	}
	return &resolved, nil
}

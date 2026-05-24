// Package policy 는 WTG 의 web-layer 운영 정책 엔진.
//
// 책임 범위 (auth.md §1 위임 원칙 유지):
//
//   - 비즈니스 룰 (거래 권한, 한도, 거래시간, 통화쌍 활성화) → 매매 엔진
//   - 운영 정책 (kill switch, 정비 창, 일시 심볼/route 차단) → WTG 가 처리
//
// 즉 본 패키지는 "비즈니스 거부" 가 아닌 "운영 차단" 만 다룬다. 정상적 거래
// 시간이라도 운영자가 정비를 위해 차단할 수 있고, 매매 엔진이 정상이어도
// 보안/장애 상황에서 web 측에서 즉시 차단할 수 있는 비상 기관 역할.
package policy

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Decision 은 정책 검사 결과.
type Decision struct {
	Allowed bool
	// Reason 은 차단 사유 코드 — 응답에 노출. 빈 값이면 정상.
	Reason string
	// Message 는 사람이 읽는 부가 설명.
	Message string
}

// Allow 는 통과 결과.
func Allow() Decision { return Decision{Allowed: true} }

// Block 은 차단 결과.
func Block(reason, msg string) Decision {
	return Decision{Allowed: false, Reason: reason, Message: msg}
}

// Request 는 정책 검사 입력. envelope 의 raw 값을 wrap.
type Request struct {
	Usid       string
	Channel    string
	Alias      string
	Exchange   string
	RoutingKey string
	// Symbol 은 envelope.data 안의 symbol 필드 (옵셔널).
	// 호출자가 사전 추출해서 채운다 (transaction 핸들러).
	Symbol string
}

// 표준 차단 사유 코드 — 호출자 응답 매핑용.
const (
	ReasonKillSwitch  = "kill_switch"
	ReasonMaintenance = "maintenance"
	ReasonSymbol      = "blocked_symbol"
	ReasonRoutingKey  = "blocked_routing_key"
)

// State 는 정책 엔진의 직렬화 가능한 상태.
//
// 운영자가 admin endpoint 로 직접 편집하고, UI 가 그대로 표시.
// JSON 직렬화 가능 → etcd / DB 차환 시에도 그대로 사용.
type State struct {
	// KillSwitch 가 true 면 매매 transaction 차단. 차단 *범위* 는
	// KillSwitchChannels 에 따라 달라진다.
	KillSwitch bool `json:"kill_switch"`

	// KillSwitchChannels — kill switch 가 적용될 채널 코드 목록 (대문자).
	// nil/empty + KillSwitch=true → 모든 채널 차단 (legacy 의미, 호환성 유지).
	// 비어있지 않으면 *그 채널 목록만* 차단 — 예: 사고 시 ["WEB","MOB","HTS"] 로
	// 고객만 차단하고 직원 (ADM/EMP) 은 비상 거래 유지.
	// pkg/mymq.ChannelCode 와 정렬 (WEB/MOB/HTS/ADM/EMP/...).
	KillSwitchChannels []string `json:"kill_switch_channels,omitempty"`

	// Maintenance 가 활성이면 그 윈도우 안에서 transaction 차단.
	// 시작/끝이 모두 채워져 있어야 활성 — 둘 중 하나라도 비면 비활성.
	Maintenance MaintenanceWindow `json:"maintenance"`

	// BlockedSymbols 는 거래 차단 심볼 목록 (대문자 비교).
	BlockedSymbols []string `json:"blocked_symbols,omitempty"`

	// BlockedRoutingKeys 는 차단할 transaction code 목록.
	BlockedRoutingKeys []string `json:"blocked_routing_keys,omitempty"`

	// UpdatedAt/By — admin 액션 audit 용.
	UpdatedAt time.Time `json:"updated_at,omitempty"`
	UpdatedBy string    `json:"updated_by,omitempty"`
}

// MaintenanceWindow — Start ≤ now < End 면 차단.
//
// Start/End 가 zero 면 비활성. 시간대는 항상 UTC 로 비교 (운영자 입력 시 UTC 변환).
type MaintenanceWindow struct {
	Start   time.Time `json:"start,omitempty"`
	End     time.Time `json:"end,omitempty"`
	Message string    `json:"message,omitempty"` // 사용자 안내 문구 (예: "심야 정비 중")
}

// active 는 now 가 정비 윈도우 안에 있는지.
func (m MaintenanceWindow) active(now time.Time) bool {
	if m.Start.IsZero() || m.End.IsZero() {
		return false
	}
	if !m.End.After(m.Start) {
		return false
	}
	return !now.Before(m.Start) && now.Before(m.End)
}

// 입력 검증 에러.
var (
	ErrAliasNotApplicable = errors.New("policy: alias only — N/A")
	ErrInvalidWindow      = errors.New("policy: 유효하지 않은 정비 창 (start < end 필요)")
	ErrSymbolEmpty        = errors.New("policy: symbol 비어있음")
)

// Engine 은 mutable 정책 보유 + 검사. goroutine-safe.
type Engine struct {
	now func() time.Time

	mu    sync.RWMutex
	state State
	// 차단 심볼/routing-key 빠른 조회용 set.
	blockedSymbols map[string]struct{}
	blockedRkeys   map[string]struct{}

	// onChange — 모든 mutator 끝에 호출되는 콜백 모음 (다중 sink 지원: etcd + hub 등).
	// remote 적용 (ApplyRemote) 시에는 호출되지 않아 watch 루프 방지.
	onChange []func(State)
	// suppressChange — ApplyRemote 가 callback 일시 정지.
	suppressChange bool
}

// SetOnChange — 변경 callback 설정 (이전 등록 모두 대체).
// nil 전달 시 등록 해제.
func (e *Engine) SetOnChange(f func(State)) {
	e.mu.Lock()
	if f == nil {
		e.onChange = nil
	} else {
		e.onChange = []func(State){f}
	}
	e.mu.Unlock()
}

// AddOnChange — 콜백을 추가 (기존 callback 유지). 다중 sink 용.
func (e *Engine) AddOnChange(f func(State)) {
	if f == nil {
		return
	}
	e.mu.Lock()
	e.onChange = append(e.onChange, f)
	e.mu.Unlock()
}

// ApplyRemote — etcd watch 등 외부 sink 가 변경을 받았을 때 호출.
// Engine 이 가진 callback (onChange) 은 호출하지 않아 무한 루프 방지.
func (e *Engine) ApplyRemote(s State) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.suppressChange = true
	defer func() { e.suppressChange = false }()
	// 직접 state 갱신 — 정규화는 SetState 코드 인라인.
	s.BlockedSymbols = normalizeUpper(s.BlockedSymbols)
	s.BlockedRoutingKeys = normalizeUpper(s.BlockedRoutingKeys)
	e.state = s
	e.rebuildSetsLocked()
}

// NewEngine 은 빈 (모두 허용) 상태의 Engine 을 만든다.
func NewEngine(nowFn func() time.Time) *Engine {
	if nowFn == nil {
		nowFn = time.Now
	}
	return &Engine{
		now:            nowFn,
		blockedSymbols: make(map[string]struct{}),
		blockedRkeys:   make(map[string]struct{}),
	}
}

// State 는 현재 상태의 deep copy.
func (e *Engine) State() State {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.snapshotLocked()
}

// SetState 는 전체 상태를 교체 (운영자 일괄 적용).
func (e *Engine) SetState(s State, updatedBy string) {
	e.mu.Lock()
	s.UpdatedAt = e.now()
	s.UpdatedBy = updatedBy
	s.BlockedSymbols = normalizeUpper(s.BlockedSymbols)
	s.BlockedRoutingKeys = normalizeUpper(s.BlockedRoutingKeys)
	e.state = s
	e.rebuildSetsLocked()
	cbs, snap, fire := e.notifyPrepLocked()
	e.mu.Unlock()
	if fire {
		for _, cb := range cbs {
			cb(snap)
		}
	}
}

// SetKillSwitch 는 kill switch 만 토글 (전체 채널 적용 — legacy).
// 채널별 적용은 SetKillSwitchScoped 사용.
func (e *Engine) SetKillSwitch(active bool, updatedBy string) {
	e.SetKillSwitchScoped(active, nil, updatedBy)
}

// SetKillSwitchScoped — kill switch 활성/비활성 + 적용 채널 지정.
// channels 가 비어있으면 전체 채널 차단 (active=true 시). 채널 코드는 대문자
// 정규화 (예: "web" → "WEB").
func (e *Engine) SetKillSwitchScoped(active bool, channels []string, updatedBy string) {
	e.mu.Lock()
	e.state.KillSwitch = active
	e.state.KillSwitchChannels = normalizeChannels(channels)
	e.state.UpdatedAt = e.now()
	e.state.UpdatedBy = updatedBy
	cbs, snap, fire := e.notifyPrepLocked()
	e.mu.Unlock()
	if fire {
		for _, cb := range cbs {
			cb(snap)
		}
	}
}

// SetMaintenance 는 정비 창 만 갱신.
func (e *Engine) SetMaintenance(w MaintenanceWindow, updatedBy string) error {
	if !w.Start.IsZero() && !w.End.IsZero() && !w.End.After(w.Start) {
		return ErrInvalidWindow
	}
	e.mu.Lock()
	e.state.Maintenance = w
	e.state.UpdatedAt = e.now()
	e.state.UpdatedBy = updatedBy
	cbs, snap, fire := e.notifyPrepLocked()
	e.mu.Unlock()
	if fire {
		for _, cb := range cbs {
			cb(snap)
		}
	}
	return nil
}

// AddBlockedSymbol — 단일 심볼 추가. 대문자 정규화. 중복은 무시.
func (e *Engine) AddBlockedSymbol(sym, updatedBy string) error {
	sym = strings.TrimSpace(strings.ToUpper(sym))
	if sym == "" {
		return ErrSymbolEmpty
	}
	e.mu.Lock()
	if _, ok := e.blockedSymbols[sym]; !ok {
		e.blockedSymbols[sym] = struct{}{}
		e.state.BlockedSymbols = append(e.state.BlockedSymbols, sym)
		sort.Strings(e.state.BlockedSymbols)
	}
	e.state.UpdatedAt = e.now()
	e.state.UpdatedBy = updatedBy
	cbs, snap, fire := e.notifyPrepLocked()
	e.mu.Unlock()
	if fire {
		for _, cb := range cbs {
			cb(snap)
		}
	}
	return nil
}

// RemoveBlockedSymbol — 미존재 무시.
func (e *Engine) RemoveBlockedSymbol(sym, updatedBy string) {
	sym = strings.TrimSpace(strings.ToUpper(sym))
	if sym == "" {
		return
	}
	e.mu.Lock()
	if _, ok := e.blockedSymbols[sym]; !ok {
		e.mu.Unlock()
		return
	}
	delete(e.blockedSymbols, sym)
	out := make([]string, 0, len(e.state.BlockedSymbols)-1)
	for _, s := range e.state.BlockedSymbols {
		if s != sym {
			out = append(out, s)
		}
	}
	e.state.BlockedSymbols = out
	e.state.UpdatedAt = e.now()
	e.state.UpdatedBy = updatedBy
	cbs, snap, fire := e.notifyPrepLocked()
	e.mu.Unlock()
	if fire {
		for _, cb := range cbs {
			cb(snap)
		}
	}
}

// AddBlockedRoutingKey / Remove — 위와 동일 패턴.
func (e *Engine) AddBlockedRoutingKey(rk, updatedBy string) error {
	rk = strings.TrimSpace(strings.ToUpper(rk))
	if rk == "" {
		return errors.New("routing_key 비어있음")
	}
	e.mu.Lock()
	if _, ok := e.blockedRkeys[rk]; !ok {
		e.blockedRkeys[rk] = struct{}{}
		e.state.BlockedRoutingKeys = append(e.state.BlockedRoutingKeys, rk)
		sort.Strings(e.state.BlockedRoutingKeys)
	}
	e.state.UpdatedAt = e.now()
	e.state.UpdatedBy = updatedBy
	cbs, snap, fire := e.notifyPrepLocked()
	e.mu.Unlock()
	if fire {
		for _, cb := range cbs {
			cb(snap)
		}
	}
	return nil
}

func (e *Engine) RemoveBlockedRoutingKey(rk, updatedBy string) {
	rk = strings.TrimSpace(strings.ToUpper(rk))
	if rk == "" {
		return
	}
	e.mu.Lock()
	if _, ok := e.blockedRkeys[rk]; !ok {
		e.mu.Unlock()
		return
	}
	delete(e.blockedRkeys, rk)
	out := make([]string, 0, len(e.state.BlockedRoutingKeys)-1)
	for _, s := range e.state.BlockedRoutingKeys {
		if s != rk {
			out = append(out, s)
		}
	}
	e.state.BlockedRoutingKeys = out
	e.state.UpdatedAt = e.now()
	e.state.UpdatedBy = updatedBy
	cbs, snap, fire := e.notifyPrepLocked()
	e.mu.Unlock()
	if fire {
		for _, cb := range cbs {
			cb(snap)
		}
	}
}

// Check 는 정책 검사. 차단되면 Decision.Allowed=false + Reason.
//
// 평가 순서 (먼저 매치되는 것 우선):
//
//  1. KillSwitch
//  2. Maintenance window
//  3. BlockedRoutingKeys
//  4. BlockedSymbols
func (e *Engine) Check(req Request) Decision {
	e.mu.RLock()
	defer e.mu.RUnlock()

	if e.state.KillSwitch {
		// 적용 채널 목록이 비어있으면 모든 채널 차단 (legacy).
		// 비어있지 않으면 그 채널만 차단.
		if len(e.state.KillSwitchChannels) == 0 {
			return Block(ReasonKillSwitch, "운영 정책으로 모든 거래가 일시 차단됨")
		}
		reqCh := strings.ToUpper(strings.TrimSpace(req.Channel))
		for _, ch := range e.state.KillSwitchChannels {
			if ch == reqCh {
				return Block(ReasonKillSwitch,
					fmt.Sprintf("채널 %q 거래가 일시 차단됨 (kill switch)", reqCh))
			}
		}
	}
	if e.state.Maintenance.active(e.now()) {
		msg := e.state.Maintenance.Message
		if msg == "" {
			msg = "정비 진행 중"
		}
		return Block(ReasonMaintenance, msg)
	}
	if rk := strings.ToUpper(strings.TrimSpace(req.RoutingKey)); rk != "" {
		if _, ok := e.blockedRkeys[rk]; ok {
			return Block(ReasonRoutingKey, fmt.Sprintf("transaction %q 가 일시 차단됨", rk))
		}
	}
	if sym := strings.ToUpper(strings.TrimSpace(req.Symbol)); sym != "" {
		if _, ok := e.blockedSymbols[sym]; ok {
			return Block(ReasonSymbol, fmt.Sprintf("심볼 %q 거래가 일시 차단됨", sym))
		}
	}
	return Allow()
}

// normalizeChannels — 입력 채널 코드를 대문자 + 공백제거 + 빈값/중복 제거.
// nil 입력은 nil 그대로.
func normalizeChannels(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, c := range in {
		c = strings.ToUpper(strings.TrimSpace(c))
		if c == "" {
			continue
		}
		if _, ok := seen[c]; ok {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// notifyPrepLocked — mutator 끝에서 호출. callback 이 등록되어 있고 ApplyRemote
// 가 아닌 경우에만 ([]cb, snapshot, true) 반환. 호출자는 lock 해제 후 모두 fire.
//
// 락 안에서 callback 직접 실행하면 callback 안에서 다시 Engine 메서드 호출 시
// deadlock. 따라서 lock 밖에서 fire.
func (e *Engine) notifyPrepLocked() ([]func(State), State, bool) {
	if e.suppressChange || len(e.onChange) == 0 {
		return nil, State{}, false
	}
	cbs := make([]func(State), len(e.onChange))
	copy(cbs, e.onChange)
	return cbs, e.snapshotLocked(), true
}

// snapshotLocked — 락 보유 상태에서 deep copy.
func (e *Engine) snapshotLocked() State {
	cp := e.state
	if cp.BlockedSymbols != nil {
		cp.BlockedSymbols = append([]string(nil), cp.BlockedSymbols...)
	}
	if cp.BlockedRoutingKeys != nil {
		cp.BlockedRoutingKeys = append([]string(nil), cp.BlockedRoutingKeys...)
	}
	return cp
}

// rebuildSetsLocked — state slice 변경 후 set 재생성.
func (e *Engine) rebuildSetsLocked() {
	e.blockedSymbols = make(map[string]struct{}, len(e.state.BlockedSymbols))
	for _, s := range e.state.BlockedSymbols {
		e.blockedSymbols[s] = struct{}{}
	}
	e.blockedRkeys = make(map[string]struct{}, len(e.state.BlockedRoutingKeys))
	for _, s := range e.state.BlockedRoutingKeys {
		e.blockedRkeys[s] = struct{}{}
	}
}

// normalizeUpper — trim + 대문자 + 중복제거 + 정렬.
func normalizeUpper(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(strings.ToUpper(s))
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

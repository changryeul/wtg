// MultiClient — 다중 mci-push 인스턴스에 user-sticky 라우팅으로 push.
//
// 운영 시나리오:
//
//	ws 사용자 A 가 mci-push #1 에 연결, B 가 #2 에 연결.
//	운영 svc 가 A 에게 push → #1 에 보내야 dispatcher fan-out 도달.
//	#2 로 보내면 dispatcher drop_unknown_user (그 인스턴스의 ws Registry 에 A 없음).
//
// 두 가지 라우팅:
//  1. user 명시 push → consistent hashing (hash(user) mod N) — 같은 user 는
//     항상 같은 인스턴스. ws LB 도 같은 hash 로 사용자 → 인스턴스 sticky 보장 필요.
//  2. broadcast push (user 빈) → fan-out all instances — 모든 인스턴스의
//     dispatcher 가 자기 ws 사용자 전체에 broadcast.
//
// MultiClient 는 thread-safe. 한 번 만들어 재사용 권장.
package push

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"sync/atomic"
)

// MultiClient — 다중 mci-push endpoint 의 user-sticky 라우팅 + broadcast fan-out.
type MultiClient struct {
	clients []*Client // 인스턴스 순서 보장 — consistent hash 결정성 위해.
	// errCount — 인스턴스별 누적 실패 카운트 (관측용, atomic).
	errCount []atomic.Uint64
	// ring — consistent hash ring (virtual node). nil 이면 hash mod N (기존 동작).
	ring *HashRing
}

// MultiClientOptions — MultiClient 생성 의존성.
type MultiClientOptions struct {
	// Endpoints — mci-push 인스턴스 base URL 목록. 순서 변경 시 사용자 → 인스턴스
	// 매핑이 깨지므로 운영에서 endpoint list 는 고정 (인스턴스 추가/제거 시 사전 공지).
	Endpoints []string

	// Secret — 모든 인스턴스 공통 X-Push-Secret. 각 인스턴스가 다른 secret 사용 시
	// PerEndpointSecrets 사용.
	Secret string

	// PerEndpointSecrets — Endpoints 와 같은 길이. 인덱스별 secret. nil 이면 Secret 사용.
	PerEndpointSecrets []string

	// VirtualNodes — > 0 이면 consistent hash ring 사용 (각 인스턴스를 N 개 v-node 로 ring 에 배치).
	// 0 이면 hash mod N (기존 Phase 2.2 동작). 운영 권장 100~200.
	//
	// ring 의 이점: 인스턴스 추가/제거 시 sticky 유지율 ~85% (mod 는 ~20%).
	// — broker session 끊김 / cache 재구축 / dispatcher fan-out 누락 최소화.
	VirtualNodes int

	// TLS — Phase 2.4 mTLS. 모든 인스턴스에 동일 client cert 적용 (단일 svc identity).
	// 인스턴스마다 다른 cert 가 필요한 시나리오는 거의 없음 (mci-push 들은 동일 trust pool).
	TLSClientCertFile string
	TLSClientKeyFile  string
	TLSServerCAFile   string
	TLSServerName     string
	TLSInsecure       bool
}

// NewMultiClient — 다중 endpoint MultiClient 생성. Endpoints 빈값이면 error.
func NewMultiClient(opts MultiClientOptions) (*MultiClient, error) {
	if len(opts.Endpoints) == 0 {
		return nil, errors.New("push: MultiClient — Endpoints 필요 (1개 이상)")
	}
	clients := make([]*Client, len(opts.Endpoints))
	for i, ep := range opts.Endpoints {
		secret := opts.Secret
		if i < len(opts.PerEndpointSecrets) && opts.PerEndpointSecrets[i] != "" {
			secret = opts.PerEndpointSecrets[i]
		}
		cli, err := newClient(ClientOptions{
			BaseURL:           ep,
			Secret:            secret,
			TLSClientCertFile: opts.TLSClientCertFile,
			TLSClientKeyFile:  opts.TLSClientKeyFile,
			TLSServerCAFile:   opts.TLSServerCAFile,
			TLSServerName:     opts.TLSServerName,
			TLSInsecure:       opts.TLSInsecure,
		})
		if err != nil {
			return nil, fmt.Errorf("push: MultiClient — endpoint[%d] %q TLS: %w", i, ep, err)
		}
		clients[i] = cli
	}
	mc := &MultiClient{
		clients:  clients,
		errCount: make([]atomic.Uint64, len(clients)),
	}
	if opts.VirtualNodes > 0 {
		mc.ring = NewRing(opts.Endpoints, opts.VirtualNodes)
	}
	return mc, nil
}

// Push — user 명시 시 consistent hash 라우팅, 빈 시 broadcast (모든 인스턴스 fan-out).
//
// 반환:
//   - user 명시: 단일 인스턴스 결과
//   - broadcast: nil + 첫 발생 error (모든 인스턴스 실패한 경우만 error 전달).
//     fan-out 의 부분 실패는 errCount 카운트 + nil error.
func (m *MultiClient) Push(ctx context.Context, msg Message) (*Result, error) {
	if msg.User == "" {
		// broadcast — 모든 인스턴스 fan-out. 부분 실패는 errCount 만 누적.
		var lastErr error
		successCount := 0
		for i, cli := range m.clients {
			res, err := cli.Push(ctx, msg)
			if err != nil {
				m.errCount[i].Add(1)
				lastErr = err
				continue
			}
			successCount++
			_ = res
		}
		if successCount == 0 {
			return nil, fmt.Errorf("push: 모든 %d 인스턴스 실패. 마지막 err: %w", len(m.clients), lastErr)
		}
		return &Result{Injected: true, User: ""}, nil
	}
	// user 명시 — ring 우선 (있으면), 아니면 hash mod N.
	idx := m.IndexForUser(msg.User)
	res, err := m.clients[idx].Push(ctx, msg)
	if err != nil {
		m.errCount[idx].Add(1)
	}
	return res, err
}

// IndexForUser — 사용자가 어느 인스턴스에 매핑되는지 노출 (LB 설정 / 디버깅용).
// ws LB 도 같은 hash 로 사용자 → 인스턴스 sticky 매핑해야 정상 동작.
// ring (consistent hash) 활성 시 ring lookup, 아니면 hash mod N.
func (m *MultiClient) IndexForUser(user string) int {
	if m.ring != nil {
		return m.ring.Lookup(user)
	}
	return userIndex(user, len(m.clients))
}

// HasRing — consistent hash ring 활성 여부 (admin UI / 디버깅용).
func (m *MultiClient) HasRing() bool {
	return m.ring != nil
}

// Endpoints — 등록된 인스턴스 base URL 목록 (debug / admin UI 표시용).
func (m *MultiClient) Endpoints() []string {
	out := make([]string, len(m.clients))
	for i, c := range m.clients {
		out[i] = c.baseURL
	}
	return out
}

// ErrCounts — 인스턴스별 누적 실패 카운트 snapshot (atomic Load).
func (m *MultiClient) ErrCounts() []uint64 {
	out := make([]uint64, len(m.errCount))
	for i := range m.errCount {
		out[i] = m.errCount[i].Load()
	}
	return out
}

// Close — 모든 인스턴스의 HTTP client idle connection 정리.
func (m *MultiClient) Close() {
	for _, c := range m.clients {
		c.Close()
	}
}

// userIndex — FNV-1a hash(user) mod N. consistent 하면서 분포 균등.
// 인스턴스 수 (N) 가 변경되면 매핑 전체가 재배치 — 운영자가 endpoint list 변경
// 시 사전 공지 + ws LB 동시 업데이트 필요.
//
// 고급 시나리오 (인스턴스 추가/제거 시 sticky 유지율 높음) 는 consistent hash
// ring (virtual node) 으로 후속 업그레이드 — 본 PoC 는 hash mod N 으로 충분.
func userIndex(user string, n int) int {
	if n <= 0 {
		return 0
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(user))
	return int(h.Sum32() % uint32(n))
}

// HashRing — consistent hashing 구현 (virtual node 기반).
//
// Phase 2.2 의 `hash(user) mod N` 은 인스턴스 추가/제거 시 거의 모든 사용자가
// 다른 인스턴스로 재배치 (broker session reset / cache miss / fan-out 누락 등
// 운영 사고 위험). HashRing 은 V 개 virtual node 로 ring 을 구성해 sticky 유지율
// 을 N→N+1 변경 시 ~1/N 만 재배치 수준으로 낮춤.
//
// 알고리즘:
//  1. 각 인스턴스 i 를 V 개 virtual node (vnode-0, vnode-1, ..., vnode-(V-1)) 로
//     ring 에 배치. 각 v-node 의 hash = SHA-1("{v}#{baseURL}") 의 상위 32-bit.
//     (SHA-1 은 분포 균등 ↑, FNV-1a 는 짧은 유사 키에 편향 발생)
//  2. user 의 hash = SHA-1(user) 의 상위 32-bit → ring 위에서 시계 방향 다음
//     v-node 의 인스턴스 idx 반환 (binary search).
//
// 성능: O(log(N×V)) lookup. N=10 / V=100 → 10 step.
// 안정성: sorted ring 은 NewRing 시 1회 계산. Lookup 은 read-only — thread-safe.
package push

import (
	"crypto/sha1"
	"encoding/binary"
	"hash/fnv"
	"sort"
	"strconv"
)

// HashRing — consistent hash ring. NewRing 으로 생성 후 immutable.
type HashRing struct {
	nodes     []ringNode // hash 오름차순 정렬
	endpoints []string   // 인스턴스 base URL 인덱스 → 순서 보존
}

type ringNode struct {
	hash       uint32
	instanceIx int // endpoints 의 인덱스
}

// NewRing — endpoints × vnodes 의 v-node 들을 ring 에 배치.
// vnodes <= 0 이면 default 100 (운영 권장 100~200 — 분포 균등 + 메모리 적당).
// endpoints 빈값이면 nil 반환.
func NewRing(endpoints []string, vnodes int) *HashRing {
	if len(endpoints) == 0 {
		return nil
	}
	if vnodes <= 0 {
		vnodes = 100
	}
	r := &HashRing{
		nodes:     make([]ringNode, 0, len(endpoints)*vnodes),
		endpoints: make([]string, len(endpoints)),
	}
	copy(r.endpoints, endpoints)
	for i, ep := range endpoints {
		for v := 0; v < vnodes; v++ {
			// v-node 시퀀스를 prefix 로 — FNV-1a 의 prefix 편향 우회 + SHA-1 정착성.
			key := strconv.Itoa(v) + "#" + ep
			r.nodes = append(r.nodes, ringNode{hash: sha1Hash(key), instanceIx: i})
		}
	}
	sort.Slice(r.nodes, func(a, b int) bool { return r.nodes[a].hash < r.nodes[b].hash })
	return r
}

// Lookup — user 의 hash 가 ring 의 시계 방향 다음 v-node 의 인스턴스 idx 반환.
// ring 마지막 v-node 보다 큰 hash 는 wrap-around (첫 v-node).
func (r *HashRing) Lookup(user string) int {
	if r == nil || len(r.nodes) == 0 {
		return 0
	}
	h := sha1Hash(user)
	// binary search — 첫 nodes[i].hash >= h.
	i := sort.Search(len(r.nodes), func(i int) bool { return r.nodes[i].hash >= h })
	if i == len(r.nodes) {
		i = 0 // wrap-around
	}
	return r.nodes[i].instanceIx
}

// Endpoints — 등록된 인스턴스 순서 (debug 용).
func (r *HashRing) Endpoints() []string {
	if r == nil {
		return nil
	}
	out := make([]string, len(r.endpoints))
	copy(out, r.endpoints)
	return out
}

// VNodeCount — ring 전체 v-node 수 (debug / metric 용).
func (r *HashRing) VNodeCount() int {
	if r == nil {
		return 0
	}
	return len(r.nodes)
}

// sha1Hash — SHA-1 의 상위 32-bit 를 ring 키로 사용. consistent hashing 표준 관행.
// SHA-1 자체는 cryptographic security 용도가 아니며 (분포 균등성만 활용), V=100
// × N=10 = 1000 v-node 1회 hash 는 < 1ms. user lookup 도 < 1µs.
func sha1Hash(s string) uint32 {
	sum := sha1.Sum([]byte(s))
	return binary.BigEndian.Uint32(sum[:4])
}

// fnvHash — backward compat. userIndex (hash mod N) 가 계속 사용.
func fnvHash(s string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return h.Sum32()
}

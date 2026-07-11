package admin

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// mci_health.go — GET /v1/admin/mci-health.
//
// admin 이 각 WTG 서비스의 HTTP 진단 endpoint 에 병렬 fan-out ping 을 쏴서
// 프로세스 실상태 (up/down + latency) 를 한 번에 반환한다. 대시보드의
// "MCI 프로세스 상태" 패널이 소비 — Prometheus 없이도 동작하는 경량 경로.
//
// 대상 목록은 --mci-health-targets ("name=url,name=url") 로 재정의 가능.
// 빈값이면 단일 호스트 dev 배치 기준 기본 목록 사용.

// McifHealthTimeout — 개별 ping timeout. 느린 서비스가 전체 응답을 오래
// 붙잡지 않도록 짧게 유지 (병렬이라 전체 소요 ≈ 이 값 상한).
const mciHealthTimeout = 1500 * time.Millisecond

// MciTarget — 체크 대상 하나.
// Tier: "dmz"(외부 edge) / "internal"(mci 코어) / "infra"(broker·etcd).
// Upstream: 이 서비스가 트래픽을 forward 하는 대상 서비스 name (토폴로지 연결선).
type MciTarget struct {
	Name     string `json:"name"`
	URL      string `json:"url"`
	Tier     string `json:"tier,omitempty"`
	Upstream string `json:"upstream,omitempty"`
}

// MciHealthEntry — 개별 서비스 체크 결과. 토폴로지 렌더용 Tier/Upstream 포함.
type MciHealthEntry struct {
	Name      string `json:"name"`
	URL       string `json:"url"`
	Tier      string `json:"tier,omitempty"`
	Upstream  string `json:"upstream,omitempty"`
	Up        bool   `json:"up"`
	LatencyMs int64  `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
}

// defaultMciTargets — 단일 호스트 배치 (dev EC2) 기준 진단 endpoint 카탈로그.
// 포트/경로는 CLAUDE.md 컴포넌트 표와 docs/observability.md 가 출처.
func defaultMciTargets() []MciTarget {
	return []MciTarget{
		// Infra — 코어 의존 (broker 는 TCP-only 라 HTTP health 없음 → 상단 broker KPI 로 관측).
		{Name: "etcd", URL: "http://127.0.0.1:2379/health", Tier: "infra"},
		// Internal — mci 코어. api/price 는 broker 로 나감.
		{Name: "mci-admin", URL: "http://127.0.0.1:9090/", Tier: "internal", Upstream: "etcd"}, // self
		{Name: "mci-api", URL: "http://127.0.0.1:8080/v1/ping", Tier: "internal", Upstream: "broker"},
		{Name: "mci-price", URL: "http://127.0.0.1:8082/v1/price-stats", Tier: "internal", Upstream: "broker"},
		{Name: "quote-forwarder", URL: "http://127.0.0.1:9091/metrics", Tier: "internal", Upstream: "mci-price"},
		// DMZ — 외부 채널 종단. 각자 바라보는 internal upstream 명시.
		{Name: "mci-edge-api", URL: "https://127.0.0.1:8090/v1/ping", Tier: "dmz", Upstream: "mci-api"},
		{Name: "mci-edge-tcp", URL: "http://127.0.0.1:5022/healthz", Tier: "dmz", Upstream: "mci-api"},
		{Name: "mci-edge-fix", URL: "http://127.0.0.1:5002/stats", Tier: "dmz", Upstream: "mci-api"},
		{Name: "mci-edge-price", URL: "http://127.0.0.1:8083/metrics", Tier: "dmz", Upstream: "mci-price"},
		{Name: "mci-edge-md", URL: "http://127.0.0.1:5012/stats", Tier: "dmz", Upstream: "mci-price"},
	}
}

// parseMciTargets — "name=url,name=url" 파싱. 형식 오류 항목은 skip.
func parseMciTargets(spec string) []MciTarget {
	var out []MciTarget
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, url, ok := strings.Cut(part, "=")
		if !ok || name == "" || url == "" {
			continue
		}
		out = append(out, MciTarget{Name: strings.TrimSpace(name), URL: strings.TrimSpace(url)})
	}
	return out
}

// mciHealthClient — 자가서명 edge (dev) 도 체크 가능해야 하므로 서버 인증서
// 검증 skip. 상태 확인 (도달 + HTTP status) 목적이라 신뢰성 요건이 아님.
var mciHealthClient = &http.Client{
	Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	},
}

// checkMciTargets — 병렬 fan-out. 2xx~3xx 를 up 으로 판정.
func checkMciTargets(ctx context.Context, targets []MciTarget) []MciHealthEntry {
	out := make([]MciHealthEntry, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func(i int, t MciTarget) {
			defer wg.Done()
			e := MciHealthEntry{Name: t.Name, URL: t.URL, Tier: t.Tier, Upstream: t.Upstream}
			cctx, cancel := context.WithTimeout(ctx, mciHealthTimeout)
			defer cancel()
			start := time.Now()
			req, err := http.NewRequestWithContext(cctx, http.MethodGet, t.URL, nil)
			if err != nil {
				e.Error = err.Error()
				out[i] = e
				return
			}
			resp, err := mciHealthClient.Do(req)
			e.LatencyMs = time.Since(start).Milliseconds()
			if err != nil {
				e.Error = err.Error()
				out[i] = e
				return
			}
			defer resp.Body.Close()
			e.Up = resp.StatusCode < 400
			if !e.Up {
				e.Error = resp.Status
			}
			out[i] = e
		}(i, t)
	}
	wg.Wait()
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out
}

// MciHealth — GET /v1/admin/mci-health 핸들러.
func MciHealth(targetsSpec string) http.HandlerFunc {
	targets := parseMciTargets(targetsSpec)
	if len(targets) == 0 {
		targets = defaultMciTargets()
	}
	return func(w http.ResponseWriter, r *http.Request) {
		entries := checkMciTargets(r.Context(), targets)
		up := 0
		for _, e := range entries {
			if e.Up {
				up++
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"services": entries,
			"up":       up,
			"total":    len(entries),
		})
	}
}

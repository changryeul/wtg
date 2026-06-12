package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// edge_proxy.go — mci-edge-price 다중 인스턴스 fan-out proxy.
//
// admin UI 의 "연결" / "Customer 검색" 페이지가 same-origin 으로 호출하면
// 등록된 모든 edge 인스턴스를 병렬 조회 → 결과 통합.
//
// 응답 형식:
//   {
//     "count": <필터 후 전체 합>,
//     "by_instance": {"edge-A:8083": 3, "edge-B:8083": 2},
//     "by_profile":  {"WEB.BRANCH.VIP": 3, ...},   // 각 instance 의 by_profile 합산
//     "connections": [{..., "instance": "edge-A:8083"}, ...],
//     "instance_errors": [{"instance": "edge-C:8083", "error": "..."}]
//   }
//
// 한 instance 실패는 instance_errors 에 기록하고 나머지는 정상 응답.
// 운영자가 "한 인스턴스 죽었나" 즉시 인지.

// instanceLabel — URL 에서 host:port 만 추출하여 사람이 읽기 쉬운 label.
//
//	http://edge-A.internal:8083/ → "edge-A.internal:8083"
func instanceLabel(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		// 파싱 실패 시 rawURL 의 scheme://, trailing / 만 trim.
		s := strings.TrimSuffix(rawURL, "/")
		s = strings.TrimPrefix(s, "http://")
		s = strings.TrimPrefix(s, "https://")
		return s
	}
	return u.Host
}

// edgeConnectionsResponse — 각 instance 의 응답 형태 (edge-price /v1/connections).
type edgeConnectionsResponse struct {
	Count       int                      `json:"count"`
	ByProfile   map[string]int           `json:"by_profile"`
	Connections []map[string]interface{} `json:"connections"`
}

// EdgeConnectionsProxy — GET /v1/admin/edge/connections (?customer_id=&profile=).
// 모든 EdgeURLs 인스턴스 병렬 호출 → 통합 응답.
func EdgeConnectionsProxy(edgeURLs []string) http.HandlerFunc {
	bases := make([]string, 0, len(edgeURLs))
	for _, u := range edgeURLs {
		t := strings.TrimSuffix(u, "/")
		if t != "" {
			bases = append(bases, t)
		}
	}
	if len(bases) == 0 {
		bases = []string{"http://127.0.0.1:8083"}
	}
	client := &http.Client{Timeout: 5 * time.Second}
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
		defer cancel()

		type result struct {
			label string
			body  edgeConnectionsResponse
			err   error
		}
		ch := make(chan result, len(bases))
		var wg sync.WaitGroup
		for _, base := range bases {
			wg.Add(1)
			go func(b string) {
				defer wg.Done()
				label := instanceLabel(b)
				target := b + "/v1/connections"
				if q := r.URL.RawQuery; q != "" {
					target += "?" + q
				}
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
				if err != nil {
					ch <- result{label: label, err: err}
					return
				}
				if v := r.Header.Get("X-WTG-User"); v != "" {
					req.Header.Set("X-WTG-User", v)
				}
				if v := r.Header.Get("Authorization"); v != "" {
					req.Header.Set("Authorization", v)
				}
				resp, err := client.Do(req)
				if err != nil {
					ch <- result{label: label, err: err}
					return
				}
				defer resp.Body.Close()
				if resp.StatusCode >= 400 {
					body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
					ch <- result{label: label, err: fmt.Errorf("upstream HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))}
					return
				}
				var parsed edgeConnectionsResponse
				if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
					ch <- result{label: label, err: fmt.Errorf("decode: %w", err)}
					return
				}
				ch <- result{label: label, body: parsed}
			}(base)
		}
		wg.Wait()
		close(ch)

		// 통합 응답 빌드 — 순서 보장 X (병렬), instance 라벨 별 정렬은 UI 가 처리.
		merged := map[string]interface{}{
			"count":           0,
			"by_instance":     map[string]int{},
			"by_profile":      map[string]int{},
			"connections":     []map[string]interface{}{},
			"instance_errors": []map[string]string{},
		}
		count := 0
		byInst := merged["by_instance"].(map[string]int)
		byProf := merged["by_profile"].(map[string]int)
		conns := merged["connections"].([]map[string]interface{})
		errs := merged["instance_errors"].([]map[string]string)
		for r := range ch {
			if r.err != nil {
				errs = append(errs, map[string]string{"instance": r.label, "error": r.err.Error()})
				continue
			}
			count += r.body.Count
			byInst[r.label] = r.body.Count
			for k, v := range r.body.ByProfile {
				byProf[k] += v
			}
			for _, c := range r.body.Connections {
				c["instance"] = r.label
				conns = append(conns, c)
			}
		}
		merged["count"] = count
		merged["connections"] = conns
		merged["instance_errors"] = errs

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(merged)
	}
}

// EdgePingProxy — GET /v1/admin/edge/ping — 모든 instance health 일괄 조회.
func EdgePingProxy(edgeURLs []string) http.HandlerFunc {
	bases := make([]string, 0, len(edgeURLs))
	for _, u := range edgeURLs {
		t := strings.TrimSuffix(u, "/")
		if t != "" {
			bases = append(bases, t)
		}
	}
	if len(bases) == 0 {
		bases = []string{"http://127.0.0.1:8083"}
	}
	client := &http.Client{Timeout: 3 * time.Second}
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		type pingOut struct {
			Instance string `json:"instance"`
			OK       bool   `json:"ok"`
			Status   int    `json:"status,omitempty"`
			Error    string `json:"error,omitempty"`
		}
		results := make([]pingOut, len(bases))
		var wg sync.WaitGroup
		for i, base := range bases {
			wg.Add(1)
			go func(idx int, b string) {
				defer wg.Done()
				out := pingOut{Instance: instanceLabel(b)}
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, b+"/v1/ping", nil)
				if err != nil {
					out.Error = err.Error()
					results[idx] = out
					return
				}
				if v := r.Header.Get("X-WTG-User"); v != "" {
					req.Header.Set("X-WTG-User", v)
				}
				resp, err := client.Do(req)
				if err != nil {
					out.Error = err.Error()
					results[idx] = out
					return
				}
				defer resp.Body.Close()
				out.Status = resp.StatusCode
				out.OK = resp.StatusCode == http.StatusOK
				results[idx] = out
			}(i, base)
		}
		wg.Wait()
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"instances": results})
	}
}

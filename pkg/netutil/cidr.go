// Package netutil — IP / CIDR / 미들웨어 공용 헬퍼.
//
// edge 서비스 (mci-edge-api / -price / -push) 와 admin 이 공통으로 쓰는
// IP allowlist 처리 + 콤마 구분 CIDR 문자열 파싱을 제공한다. admin/ipallow.go
// 도 같은 의도지만 edge 가 admin 패키지를 import 할 수 없어 (의존 방향 금지)
// 별도 패키지로 추출.
package netutil

import (
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strings"
)

// ParseCIDRs 는 콤마 구분 문자열을 *net.IPNet 슬라이스로 변환한다.
// 빈 문자열은 nil 반환 (= 모두 허용). 잘못된 항목은 에러.
func ParseCIDRs(s string) ([]*net.IPNet, error) {
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	out := make([]*net.IPNet, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		_, n, err := net.ParseCIDR(p)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

// RemoteIP 는 요청 발신 IP 를 반환. r.RemoteAddr 만 본다 (X-Forwarded-For 는
// upstream LB 가 신뢰될 때만 의미가 있어 호출측이 명시적으로 처리).
func RemoteIP(r *http.Request) net.IP {
	host := r.RemoteAddr
	if idx := strings.LastIndex(host, ":"); idx > 0 {
		host = host[:idx]
	}
	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")
	return net.ParseIP(host)
}

// IPAllowed — IP 가 allowed 중 하나의 CIDR 에 속하는지.
func IPAllowed(ip net.IP, allowed []*net.IPNet) bool {
	for _, n := range allowed {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// IPAllowList 는 allowed CIDR 외 접근을 403 으로 차단하는 미들웨어.
// allowed 가 비어있으면 모두 허용 (DevMode / trust-network).
//
// edge 서비스에 wire 할 때는 가능한 한 체인의 *최선두* — auth / rate-limit /
// 라우팅 모두 이전 — 에 둔다. 차단 결정은 인증 정보를 요구하지 않으며,
// 조기 거부가 가장 저렴하다.
func IPAllowList(allowed []*net.IPNet, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if len(allowed) == 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := RemoteIP(r)
			if ip == nil {
				writeForbidden(w, "remote IP 파싱 불가")
				return
			}
			if !IPAllowed(ip, allowed) {
				if logger != nil {
					logger.WarnContext(r.Context(), "허용 CIDR 외 IP 차단",
						slog.String("ip", ip.String()),
						slog.String("path", r.URL.Path),
					)
				}
				writeForbidden(w, "허용 CIDR 외부 접근 차단")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// writeForbidden — admin 패키지의 writeJSONError 와 호환되는 단순 JSON 에러.
func writeForbidden(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":   "forbidden",
		"message": msg,
	})
}

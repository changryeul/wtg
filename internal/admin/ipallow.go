package admin

import (
	"log/slog"
	"net"
	"net/http"
	"strings"
)

// IPAllowList 는 사내망 CIDR 외 접근을 차단하는 미들웨어.
//
// 요청 IP 결정 우선순위:
//  1. r.RemoteAddr 의 IP (직접 연결)
//  2. (운영시 reverse proxy 뒤라면) X-Forwarded-For — 단, mci-admin 은
//     DMZ 미경유 사내망 직접 접속이 원칙이라 일반적으로 1번만 사용.
//
// allowed 가 비어있으면 모든 IP 허용 (DevMode 또는 trust-network 환경).
func IPAllowList(allowed []*net.IPNet, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if len(allowed) == 0 {
				next.ServeHTTP(w, r)
				return
			}
			ip := remoteIP(r)
			if ip == nil {
				writeJSONError(w, http.StatusForbidden, "forbidden", "remote IP 파싱 불가")
				return
			}
			if !ipAllowed(ip, allowed) {
				if logger != nil {
					logger.WarnContext(r.Context(), "사내망 외 IP 차단",
						slog.String("ip", ip.String()),
						slog.String("path", r.URL.Path),
					)
				}
				writeJSONError(w, http.StatusForbidden, "forbidden", "사내망 외부 접근 차단")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// remoteIP 는 요청 발신 IP 를 반환.
func remoteIP(r *http.Request) net.IP {
	host := r.RemoteAddr
	// "ip:port" 또는 "[ipv6]:port".
	if idx := strings.LastIndex(host, ":"); idx > 0 {
		host = host[:idx]
	}
	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")
	return net.ParseIP(host)
}

// ipAllowed 는 IP 가 화이트리스트 CIDR 중 하나에 속하는지.
func ipAllowed(ip net.IP, allowed []*net.IPNet) bool {
	for _, n := range allowed {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

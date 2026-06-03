// Package chart (edge) 는 WTG 의 DMZ 측 챠트 reverse proxy (mci-edge-chart).
//
// mci-chart 는 그 자체로 HTTP REST + WS + 정적 SPA 를 제공하므로, edge 는
// 추가 비즈니스 로직 없이 TLS termination + 인증 + IP 화이트리스트 + rate-limit
// 만 입혀서 그대로 reverse-proxy 한다.
//
// 흐름:
//
//	브라우저 → mci-edge-chart (DMZ)
//	  - TLS termination
//	  - IP 화이트리스트
//	  - rate limit
//	  - (옵션) JWT 검증
//	  → mci-chart (Internal)
//	     · / (UI SPA)
//	     · /v1/chart (REST historical)
//	     · /v1/chart/stream (WS live)
//
// httputil.ReverseProxy 가 Upgrade 헤더 (WebSocket) 도 자동 통과한다 (Go 1.12+).
package chart

import (
	"flag"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/winwaysystems/wtg/pkg/netutil"
	"github.com/winwaysystems/wtg/pkg/ratelimit"
)

// Config 는 mci-edge-chart 런타임 설정.
type Config struct {
	ListenAddr string

	// Internal mci-chart 의 base URL.
	UpstreamURL string

	// 인증 모드 — JWT 검증기가 채워졌을 때만 /v1/* 보호. / (UI) 는 항상 통과.
	// DevMode=true 면 X-WTG-User 헤더만 신뢰.
	DevMode    bool
	JWTPubFile string // RS256 public key PEM (옵션 — 채워지면 JWT 활성)

	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	IdleTimeout  time.Duration

	// upstream 호출 timeout — WS 는 별도 (long-lived), REST 만 영향.
	UpstreamTimeout time.Duration

	// Rate limit fallback — 어느 룰도 매칭 안 된 path 의 한도. 0=비활성.
	IPRatePerSec float64
	IPBurst      int

	// Path-aware rate limit 룰셋. nil → DefaultRateLimitRules().
	RateLimitRules []ratelimit.Rule

	// etcd 기반 hot reload. 비면 정적 룰만.
	EtcdEndpoints    string
	EtcdRateLimitKey string // default "wtg/ratelimit/edge-chart"

	// Redis backend — 다중 인스턴스 단일 카운터. 비면 in-memory.
	RateLimitRedisAddr     string
	RateLimitRedisPassword string
	RateLimitRedisDB       int

	LogLevel string

	// 외부 TLS — 브라우저 ↔ edge. ingress / LB 가 종단 권장이지만 옵셔널.
	TLSCertFile     string
	TLSKeyFile      string
	TLSClientCAFile string

	// Upstream mTLS — DMZ → Internal mci-chart 호출.
	TLSUpstreamCertFile   string
	TLSUpstreamKeyFile    string
	TLSUpstreamCAFile     string
	TLSUpstreamServerName string

	// AllowCIDRs — 외부 허용 CIDR. 비면 모두 허용 (dev).
	AllowCIDRs []*net.IPNet
}

// DefaultConfig 는 합리적인 디폴트.
func DefaultConfig() Config {
	return Config{
		ListenAddr:       ":8087",
		UpstreamURL:      "http://127.0.0.1:8086",
		DevMode:          false,
		ReadTimeout:      10 * time.Second,
		WriteTimeout:     30 * time.Second,
		IdleTimeout:      120 * time.Second,
		UpstreamTimeout:  30 * time.Second,
		IPRatePerSec:     100,
		IPBurst:          200,
		EtcdRateLimitKey: "wtg/ratelimit/edge-chart",
		LogLevel:         "info",
	}
}

// LoadConfig — env (WTG_ECHART_*) + flag 합산.
func LoadConfig(args []string) (Config, error) {
	cfg := DefaultConfig()

	if v := os.Getenv("WTG_ECHART_LISTEN"); v != "" {
		cfg.ListenAddr = v
	}
	if v := os.Getenv("WTG_ECHART_UPSTREAM"); v != "" {
		cfg.UpstreamURL = v
	}
	if v := os.Getenv("WTG_ECHART_DEV"); v == "1" || v == "true" {
		cfg.DevMode = true
	}
	if v := os.Getenv("WTG_ECHART_JWT_PUB"); v != "" {
		cfg.JWTPubFile = v
	}
	if v := os.Getenv("WTG_ECHART_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
	if v := os.Getenv("WTG_ECHART_TLS_CERT"); v != "" {
		cfg.TLSCertFile = v
	}
	if v := os.Getenv("WTG_ECHART_TLS_KEY"); v != "" {
		cfg.TLSKeyFile = v
	}
	if v := os.Getenv("WTG_ECHART_TLS_CLIENT_CA"); v != "" {
		cfg.TLSClientCAFile = v
	}
	if v := os.Getenv("WTG_ECHART_TLS_UPSTREAM_CERT"); v != "" {
		cfg.TLSUpstreamCertFile = v
	}
	if v := os.Getenv("WTG_ECHART_TLS_UPSTREAM_KEY"); v != "" {
		cfg.TLSUpstreamKeyFile = v
	}
	if v := os.Getenv("WTG_ECHART_TLS_UPSTREAM_CA"); v != "" {
		cfg.TLSUpstreamCAFile = v
	}
	if v := os.Getenv("WTG_ECHART_TLS_UPSTREAM_SNI"); v != "" {
		cfg.TLSUpstreamServerName = v
	}
	cidrStr := os.Getenv("WTG_ECHART_ALLOW_CIDRS")

	fs := flag.NewFlagSet("mci-edge-chart", flag.ContinueOnError)
	fs.StringVar(&cfg.ListenAddr, "listen", cfg.ListenAddr, "외부 HTTP/HTTPS listen 주소")
	fs.StringVar(&cfg.UpstreamURL, "upstream", cfg.UpstreamURL, "Internal mci-chart base URL")
	fs.BoolVar(&cfg.DevMode, "dev", cfg.DevMode, "개발 모드 — JWT 검증 우회")
	fs.StringVar(&cfg.JWTPubFile, "jwt-pub", cfg.JWTPubFile, "JWT 검증용 public key PEM (옵션)")
	fs.DurationVar(&cfg.ReadTimeout, "read-timeout", cfg.ReadTimeout, "HTTP read timeout")
	fs.DurationVar(&cfg.WriteTimeout, "write-timeout", cfg.WriteTimeout, "HTTP write timeout")
	fs.DurationVar(&cfg.IdleTimeout, "idle-timeout", cfg.IdleTimeout, "HTTP idle timeout (ws 영향)")
	fs.DurationVar(&cfg.UpstreamTimeout, "upstream-timeout", cfg.UpstreamTimeout, "upstream REST round-trip timeout (ws 영향 없음)")
	fs.Float64Var(&cfg.IPRatePerSec, "ip-rate", cfg.IPRatePerSec, "fallback rate limit TPS (룰 매칭 안 된 path, 0=비활성)")
	fs.IntVar(&cfg.IPBurst, "ip-burst", cfg.IPBurst, "fallback burst 한도")
	fs.StringVar(&cfg.EtcdEndpoints, "etcd", cfg.EtcdEndpoints, "etcd endpoints (콤마 구분, 비면 정적 룰만)")
	fs.StringVar(&cfg.EtcdRateLimitKey, "etcd-ratelimit-key", cfg.EtcdRateLimitKey, "etcd PolicyDoc key (default wtg/ratelimit/edge-chart)")
	fs.StringVar(&cfg.RateLimitRedisAddr, "ratelimit-redis", cfg.RateLimitRedisAddr, "Redis addr — rate limit 분산 backend (host:port)")
	fs.StringVar(&cfg.RateLimitRedisPassword, "ratelimit-redis-pass", cfg.RateLimitRedisPassword, "Redis password")
	fs.IntVar(&cfg.RateLimitRedisDB, "ratelimit-redis-db", cfg.RateLimitRedisDB, "Redis DB index")
	fs.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "로그 레벨 debug/info/warn/error")
	fs.StringVar(&cfg.TLSCertFile, "tls-cert", cfg.TLSCertFile, "외부 TLS cert PEM (있으면 HTTPS)")
	fs.StringVar(&cfg.TLSKeyFile, "tls-key", cfg.TLSKeyFile, "외부 TLS key PEM")
	fs.StringVar(&cfg.TLSClientCAFile, "tls-client-ca", cfg.TLSClientCAFile, "외부 mTLS 클라이언트 CA bundle")
	fs.StringVar(&cfg.TLSUpstreamCertFile, "tls-upstream-cert", cfg.TLSUpstreamCertFile, "Upstream mTLS 클라이언트 cert")
	fs.StringVar(&cfg.TLSUpstreamKeyFile, "tls-upstream-key", cfg.TLSUpstreamKeyFile, "Upstream mTLS 클라이언트 key")
	fs.StringVar(&cfg.TLSUpstreamCAFile, "tls-upstream-ca", cfg.TLSUpstreamCAFile, "Upstream 서버 검증 CA")
	fs.StringVar(&cfg.TLSUpstreamServerName, "tls-upstream-sni", cfg.TLSUpstreamServerName, "Upstream SNI / hostname")
	fs.StringVar(&cidrStr, "allow-cidrs", cidrStr, "외부 허용 CIDR (콤마 구분, 비면 모두 허용)")
	if v := os.Getenv("WTG_ECHART_IP_RATE"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.IPRatePerSec = f
		}
	}

	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	if cidrs, err := netutil.ParseCIDRs(cidrStr); err != nil {
		return cfg, err
	} else {
		cfg.AllowCIDRs = cidrs
	}
	return cfg, nil
}

// Package api (edge) 는 WTG 의 DMZ 측 REST 프록시 (mci-edge-api).
//
// 외부 web 클라이언트의 HTTP 요청을 받아 인증 후 Internal mci-api 로 forward
// 한다. 비즈니스 로직 없음 — 순수 transport + 인증 layer.
//
// 흐름:
//
//	브라우저 → mci-edge-api (DMZ)
//	  - JWT 검증 (DevMode: X-WTG-User)
//	  - 헤더 sanitization
//	  - rate limit
//	  → mci-api (Internal)
//	  → broker → 매매 엔진
//	응답: 그대로 reverse 흐름
package api

import (
	"flag"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/winwaysystems/wtg/pkg/netutil"
	"github.com/winwaysystems/wtg/pkg/ratelimit"
)

// Config 는 mci-edge-api 런타임 설정.
type Config struct {
	// 외부 listen 주소 (HTTP — 운영시 TLS termination 추가).
	ListenAddr string

	// Internal mci-api 의 base URL (예: "http://10.0.0.20:8080").
	UpstreamURL string

	// 인증 모드.
	DevMode bool

	// HTTP timeout.
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	IdleTimeout  time.Duration

	// upstream 호출 timeout (전체 reverse proxy round-trip).
	UpstreamTimeout time.Duration

	// 최대 요청 본문 크기 (bytes). 0 = 제한 없음.
	MaxRequestBody int64

	// Rate limit fallback — 어느 룰도 매칭 안 된 path 의 한도. 0 = fallback 비활성
	// (default 룰셋만 작동, 다른 path 는 통과).
	IPRatePerSec float64
	IPBurst      int

	// Path-aware rate limit 룰셋. nil 이면 DefaultRateLimitRules() 사용.
	// 빈 슬라이스 [] 면 룰셋 자체를 비활성 (fallback 만 작동, 또는 비활성).
	RateLimitRules []ratelimit.Rule

	// 로그 레벨.
	LogLevel string

	// TLS — 외부 종단점 (브라우저 ↔ edge). 일반적으로 운영에서는 ingress / LB 가
	// TLS termination 을 담당하지만, edge 자체에서도 처리 가능하게 옵셔널 지원.
	// TLSClientCAFile 이 함께 채워지면 mTLS — B2B / API key 시나리오용.
	TLSCertFile     string
	TLSKeyFile      string
	TLSClientCAFile string

	// Upstream mTLS — DMZ → Internal 호출 시 클라이언트 인증서.
	// (TLSUpstreamCertFile, TLSUpstreamKeyFile, TLSUpstreamCAFile, TLSUpstreamServerName)
	TLSUpstreamCertFile   string
	TLSUpstreamKeyFile    string
	TLSUpstreamCAFile     string
	TLSUpstreamServerName string

	// AllowCIDRs — 외부에서 접근 가능한 CIDR 화이트리스트. 비면 모두 허용.
	// 운영 시엔 회사 사무실 / 파트너사 출구 IP 만 열어두는 용도. flag/env 는
	// 콤마 구분 문자열, 내부 표현은 LoadConfig 에서 파싱된 *net.IPNet 슬라이스.
	AllowCIDRs []*net.IPNet
}

// DefaultConfig 는 합리적인 디폴트.
func DefaultConfig() Config {
	return Config{
		ListenAddr:      ":8090",
		UpstreamURL:     "http://127.0.0.1:8080",
		DevMode:         false,
		ReadTimeout:     10 * time.Second,
		WriteTimeout:    10 * time.Second,
		IdleTimeout:     60 * time.Second,
		UpstreamTimeout: 10 * time.Second,
		MaxRequestBody:  1 << 20, // 1 MiB
		IPRatePerSec:    100,
		IPBurst:         200,
		LogLevel:        "info",
	}
}

// LoadConfig 는 flag + env 합산.
//
// 환경변수 prefix: WTG_EAPI_*
func LoadConfig(args []string) (Config, error) {
	cfg := DefaultConfig()

	if v := os.Getenv("WTG_EAPI_LISTEN"); v != "" {
		cfg.ListenAddr = v
	}
	if v := os.Getenv("WTG_EAPI_UPSTREAM"); v != "" {
		cfg.UpstreamURL = v
	}
	if v := os.Getenv("WTG_EAPI_DEV"); v == "1" || v == "true" {
		cfg.DevMode = true
	}
	if v := os.Getenv("WTG_EAPI_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
	if v := os.Getenv("WTG_EAPI_MAX_BODY"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.MaxRequestBody = n
		}
	}
	if v := os.Getenv("WTG_EAPI_TLS_CERT"); v != "" {
		cfg.TLSCertFile = v
	}
	if v := os.Getenv("WTG_EAPI_TLS_KEY"); v != "" {
		cfg.TLSKeyFile = v
	}
	if v := os.Getenv("WTG_EAPI_TLS_CLIENT_CA"); v != "" {
		cfg.TLSClientCAFile = v
	}
	if v := os.Getenv("WTG_EAPI_UPSTREAM_TLS_CERT"); v != "" {
		cfg.TLSUpstreamCertFile = v
	}
	if v := os.Getenv("WTG_EAPI_UPSTREAM_TLS_KEY"); v != "" {
		cfg.TLSUpstreamKeyFile = v
	}
	if v := os.Getenv("WTG_EAPI_UPSTREAM_TLS_CA"); v != "" {
		cfg.TLSUpstreamCAFile = v
	}
	if v := os.Getenv("WTG_EAPI_UPSTREAM_TLS_SNI"); v != "" {
		cfg.TLSUpstreamServerName = v
	}
	cidrStr := os.Getenv("WTG_EAPI_ALLOW_CIDRS")

	fs := flag.NewFlagSet("mci-edge-api", flag.ContinueOnError)
	fs.StringVar(&cidrStr, "allow-cidrs", cidrStr, "외부 접근 허용 CIDR (콤마 구분, 비면 모두 허용)")
	fs.StringVar(&cfg.ListenAddr, "listen", cfg.ListenAddr, "외부 HTTP listen 주소")
	fs.StringVar(&cfg.UpstreamURL, "upstream", cfg.UpstreamURL, "Internal mci-api base URL")
	fs.BoolVar(&cfg.DevMode, "dev", cfg.DevMode, "개발 모드 — JWT 우회")
	fs.DurationVar(&cfg.UpstreamTimeout, "upstream-timeout", cfg.UpstreamTimeout, "upstream 호출 timeout")
	fs.Int64Var(&cfg.MaxRequestBody, "max-body", cfg.MaxRequestBody, "최대 요청 본문 크기 (0=무제한)")
	fs.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "로그 레벨")
	fs.Float64Var(&cfg.IPRatePerSec, "ip-rate", cfg.IPRatePerSec, "IP 단위 sustained TPS (0=비활성)")
	fs.IntVar(&cfg.IPBurst, "ip-burst", cfg.IPBurst, "IP 단위 burst 한도")
	fs.StringVar(&cfg.TLSCertFile, "tls-cert", cfg.TLSCertFile, "외부 TLS 서버 cert PEM (있으면 HTTPS)")
	fs.StringVar(&cfg.TLSKeyFile, "tls-key", cfg.TLSKeyFile, "외부 TLS 서버 key PEM")
	fs.StringVar(&cfg.TLSClientCAFile, "tls-client-ca", cfg.TLSClientCAFile, "외부 mTLS 클라이언트 CA bundle")
	fs.StringVar(&cfg.TLSUpstreamCertFile, "upstream-tls-cert", cfg.TLSUpstreamCertFile, "Internal upstream mTLS 클라이언트 cert")
	fs.StringVar(&cfg.TLSUpstreamKeyFile, "upstream-tls-key", cfg.TLSUpstreamKeyFile, "Internal upstream mTLS 클라이언트 key")
	fs.StringVar(&cfg.TLSUpstreamCAFile, "upstream-tls-ca", cfg.TLSUpstreamCAFile, "Internal upstream 서버 검증용 CA")
	fs.StringVar(&cfg.TLSUpstreamServerName, "upstream-tls-sni", cfg.TLSUpstreamServerName, "Internal upstream SNI")

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

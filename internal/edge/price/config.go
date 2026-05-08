// Package price (edge) 는 WTG (Winway Trading Gateway) 의 DMZ 측 시세 fan-out
// 서비스 (mci-edge-price).
//
// 책임:
//   - Internal mci-price 와 gRPC stream 으로 연결 → Tick 수신
//   - DMZ 측 WebSocket 서버 — 외부 web 클라이언트 fan-out
//   - 인증 (JWT 검증, DevMode: X-WTG-User)
//   - edge conflation (느린 ws 클라이언트 격리)
//   - rate limit / TLS termination (Phase 8)
//
// 보안 원칙:
//   - 비즈니스 로직 없음. 순수 transport layer.
//   - DMZ → Internal 정방향 gRPC 만 허용 (방화벽 정책 일치).
package price

import (
	"flag"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/winwaysystems/wtg/pkg/netutil"
)

// Config 는 mci-edge-price 의 런타임 설정.
type Config struct {
	// 외부 WebSocket listen 주소.
	ListenAddr string

	// Internal mci-price 의 gRPC endpoint (DMZ → Internal).
	UpstreamGRPC string

	// 이 edge 인스턴스 식별자 (PriceService.Subscribe 에 보내는 subscriber_id).
	SubscriberID string

	// 인증 모드.
	DevMode bool

	// WebSocket 옵션.
	WsPingInterval time.Duration
	WsPongTimeout  time.Duration
	SendQueueSize  int

	// HTTP 서버 timeout.
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	IdleTimeout  time.Duration

	// gRPC dial 옵션.
	DialTimeout time.Duration

	// IP 단위 rate limit (0=비활성).
	IPRatePerSec float64
	IPBurst      int

	// 로그.
	LogLevel string

	// Upstream gRPC TLS — DMZ → Internal mci-price 호출용 클라이언트 인증서.
	// CertFile/KeyFile 가 채워지면 mTLS 클라이언트 인증.
	GRPCTLSCertFile   string
	GRPCTLSKeyFile    string
	GRPCTLSCAFile     string
	GRPCTLSServerName string

	// 외부 HTTPS — 브라우저 ↔ edge ws. 운영에서는 일반적으로 ingress 가 종단.
	// TLSClientCAFile 채워지면 mTLS — B2B / 기관 클라이언트용.
	TLSCertFile     string
	TLSKeyFile      string
	TLSClientCAFile string

	// AllowCIDRs — 외부 접근 허용 CIDR 화이트리스트 (콤마 구분 → IPNet).
	// pkg/netutil.IPAllowList 미들웨어가 사용. 비면 모두 허용.
	AllowCIDRs []*net.IPNet
}

// DefaultConfig 는 합리적인 디폴트.
func DefaultConfig() Config {
	host, _ := os.Hostname()
	return Config{
		ListenAddr:     ":8083",
		UpstreamGRPC:   "127.0.0.1:50051",
		SubscriberID:   "mci-edge-price@" + host,
		DevMode:        false,
		WsPingInterval: 30 * time.Second,
		WsPongTimeout:  60 * time.Second,
		SendQueueSize:  256,
		ReadTimeout:    10 * time.Second,
		WriteTimeout:   10 * time.Second,
		IdleTimeout:    120 * time.Second,
		DialTimeout:    5 * time.Second,
		IPRatePerSec:   100,
		IPBurst:        200,
		LogLevel:       "info",
	}
}

// LoadConfig 는 flag + env 합산.
//
// 환경변수 prefix: WTG_EPRICE_*
func LoadConfig(args []string) (Config, error) {
	cfg := DefaultConfig()

	if v := os.Getenv("WTG_EPRICE_LISTEN"); v != "" {
		cfg.ListenAddr = v
	}
	if v := os.Getenv("WTG_EPRICE_UPSTREAM"); v != "" {
		cfg.UpstreamGRPC = v
	}
	if v := os.Getenv("WTG_EPRICE_SUBSCRIBER"); v != "" {
		cfg.SubscriberID = v
	}
	if v := os.Getenv("WTG_EPRICE_DEV"); v == "1" || v == "true" {
		cfg.DevMode = true
	}
	if v := os.Getenv("WTG_EPRICE_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
	if v := os.Getenv("WTG_EPRICE_QUEUE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.SendQueueSize = n
		}
	}
	if v := os.Getenv("WTG_EPRICE_GRPC_TLS_CERT"); v != "" {
		cfg.GRPCTLSCertFile = v
	}
	if v := os.Getenv("WTG_EPRICE_GRPC_TLS_KEY"); v != "" {
		cfg.GRPCTLSKeyFile = v
	}
	if v := os.Getenv("WTG_EPRICE_GRPC_TLS_CA"); v != "" {
		cfg.GRPCTLSCAFile = v
	}
	if v := os.Getenv("WTG_EPRICE_GRPC_TLS_SNI"); v != "" {
		cfg.GRPCTLSServerName = v
	}
	if v := os.Getenv("WTG_EPRICE_TLS_CERT"); v != "" {
		cfg.TLSCertFile = v
	}
	if v := os.Getenv("WTG_EPRICE_TLS_KEY"); v != "" {
		cfg.TLSKeyFile = v
	}
	if v := os.Getenv("WTG_EPRICE_TLS_CLIENT_CA"); v != "" {
		cfg.TLSClientCAFile = v
	}
	cidrStr := os.Getenv("WTG_EPRICE_ALLOW_CIDRS")

	fs := flag.NewFlagSet("mci-edge-price", flag.ContinueOnError)
	fs.StringVar(&cidrStr, "allow-cidrs", cidrStr, "외부 접근 허용 CIDR (콤마 구분, 비면 모두 허용)")
	fs.StringVar(&cfg.ListenAddr, "listen", cfg.ListenAddr, "HTTP/WS listen 주소")
	fs.StringVar(&cfg.UpstreamGRPC, "upstream", cfg.UpstreamGRPC, "Internal mci-price gRPC endpoint")
	fs.StringVar(&cfg.SubscriberID, "subscriber-id", cfg.SubscriberID, "edge 인스턴스 식별자")
	fs.BoolVar(&cfg.DevMode, "dev", cfg.DevMode, "개발 모드 — JWT 검증 우회")
	fs.IntVar(&cfg.SendQueueSize, "send-queue", cfg.SendQueueSize, "ws 클라이언트별 send queue 크기")
	fs.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "로그 레벨")
	fs.DurationVar(&cfg.DialTimeout, "dial-timeout", cfg.DialTimeout, "gRPC dial timeout")
	fs.Float64Var(&cfg.IPRatePerSec, "ip-rate", cfg.IPRatePerSec, "IP 단위 sustained TPS (0=비활성)")
	fs.IntVar(&cfg.IPBurst, "ip-burst", cfg.IPBurst, "IP burst 한도")
	fs.StringVar(&cfg.GRPCTLSCertFile, "grpc-tls-cert", cfg.GRPCTLSCertFile, "Upstream gRPC mTLS 클라이언트 cert PEM")
	fs.StringVar(&cfg.GRPCTLSKeyFile, "grpc-tls-key", cfg.GRPCTLSKeyFile, "Upstream gRPC mTLS 클라이언트 key PEM")
	fs.StringVar(&cfg.GRPCTLSCAFile, "grpc-tls-ca", cfg.GRPCTLSCAFile, "Upstream 서버 검증용 CA bundle")
	fs.StringVar(&cfg.GRPCTLSServerName, "grpc-tls-sni", cfg.GRPCTLSServerName, "Upstream SNI / hostname")
	fs.StringVar(&cfg.TLSCertFile, "tls-cert", cfg.TLSCertFile, "외부 TLS 서버 cert PEM (있으면 HTTPS)")
	fs.StringVar(&cfg.TLSKeyFile, "tls-key", cfg.TLSKeyFile, "외부 TLS 서버 key PEM")
	fs.StringVar(&cfg.TLSClientCAFile, "tls-client-ca", cfg.TLSClientCAFile, "외부 mTLS 클라이언트 CA bundle")

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

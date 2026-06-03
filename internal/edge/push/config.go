// Package push (edge) 는 WTG 의 DMZ 측 WebSocket gateway (mci-edge-push).
//
// Internal mci-push 의 PushService gRPC stream 에 접속해서 unsolicited 메시지
// 를 받고, DMZ 측 WebSocket 클라이언트들에게 사용자별 fan-out.
//
// 사용자 ↔ ws.Conn 매핑은 edge 의 Registry 가 자체 관리. 1차 prototype 은
// 모든 메시지를 internal 에서 받아 edge 가 logon_id 기반 라우팅.
package push

import (
	"flag"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/winwaysystems/wtg/pkg/netutil"
	"github.com/winwaysystems/wtg/pkg/ratelimit"
)

// Config 는 mci-edge-push 런타임 설정.
type Config struct {
	// 외부 listen 주소.
	ListenAddr string

	// Internal mci-push 의 gRPC endpoint.
	UpstreamGRPC string

	// 이 edge 식별자 (PushService.Subscribe 의 subscriber_id).
	SubscriberID string

	// 인증.
	DevMode bool

	// WebSocket.
	WsPingInterval time.Duration
	WsPongTimeout  time.Duration
	SendQueueSize  int

	// HTTP.
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	IdleTimeout  time.Duration

	// gRPC.
	DialTimeout time.Duration

	// Rate limit fallback — 룰 매칭 안 된 path 의 한도. 0=비활성.
	IPRatePerSec float64
	IPBurst      int

	// Path-aware rate limit 룰셋. nil → DefaultRateLimitRules().
	RateLimitRules []ratelimit.Rule

	// etcd 기반 hot reload.
	EtcdEndpoints    string
	EtcdRateLimitKey string // default "wtg/ratelimit/edge-push"

	// Redis backend — 다중 인스턴스 단일 카운터. 비면 in-memory.
	RateLimitRedisAddr     string
	RateLimitRedisPassword string
	RateLimitRedisDB       int

	LogLevel string

	// Upstream gRPC TLS — DMZ → Internal mci-push 호출용 클라이언트 인증서.
	// CertFile/KeyFile 채워지면 mTLS 클라이언트 인증.
	GRPCTLSCertFile   string
	GRPCTLSKeyFile    string
	GRPCTLSCAFile     string
	GRPCTLSServerName string

	// 외부 종단 (브라우저 ↔ edge) HTTPS — 일반적으로 ingress/LB 가 처리하지만
	// edge 자체에서도 처리 가능하게 옵셔널 지원.
	TLSCertFile     string
	TLSKeyFile      string
	TLSClientCAFile string

	// AllowCIDRs — 외부 접근 허용 CIDR 화이트리스트.
	// pkg/netutil.IPAllowList 미들웨어가 사용. 비면 모두 허용.
	AllowCIDRs []*net.IPNet

	// OTel TracerProvider.
	OtelEndpoint    string
	OtelInsecure    bool
	OtelStdout      bool
	OtelSampleRatio float64
}

// DefaultConfig.
func DefaultConfig() Config {
	host, _ := os.Hostname()
	return Config{
		ListenAddr:       ":8084",
		UpstreamGRPC:     "127.0.0.1:50052",
		SubscriberID:     "mci-edge-push@" + host,
		DevMode:          false,
		WsPingInterval:   30 * time.Second,
		WsPongTimeout:    60 * time.Second,
		SendQueueSize:    256,
		ReadTimeout:      10 * time.Second,
		WriteTimeout:     10 * time.Second,
		IdleTimeout:      120 * time.Second,
		DialTimeout:      5 * time.Second,
		IPRatePerSec:     100,
		IPBurst:          200,
		EtcdRateLimitKey: "wtg/ratelimit/edge-push",
		LogLevel:         "info",
	}
}

// LoadConfig — flag/env (WTG_EPUSH_*).
func LoadConfig(args []string) (Config, error) {
	cfg := DefaultConfig()

	if v := os.Getenv("WTG_EPUSH_LISTEN"); v != "" {
		cfg.ListenAddr = v
	}
	if v := os.Getenv("WTG_EPUSH_UPSTREAM"); v != "" {
		cfg.UpstreamGRPC = v
	}
	if v := os.Getenv("WTG_EPUSH_SUBSCRIBER"); v != "" {
		cfg.SubscriberID = v
	}
	if v := os.Getenv("WTG_EPUSH_DEV"); v == "1" || v == "true" {
		cfg.DevMode = true
	}
	if v := os.Getenv("WTG_EPUSH_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
	if v := os.Getenv("WTG_EPUSH_QUEUE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.SendQueueSize = n
		}
	}
	if v := os.Getenv("WTG_EPUSH_GRPC_TLS_CERT"); v != "" {
		cfg.GRPCTLSCertFile = v
	}
	if v := os.Getenv("WTG_EPUSH_GRPC_TLS_KEY"); v != "" {
		cfg.GRPCTLSKeyFile = v
	}
	if v := os.Getenv("WTG_EPUSH_GRPC_TLS_CA"); v != "" {
		cfg.GRPCTLSCAFile = v
	}
	if v := os.Getenv("WTG_EPUSH_GRPC_TLS_SNI"); v != "" {
		cfg.GRPCTLSServerName = v
	}
	if v := os.Getenv("WTG_EPUSH_TLS_CERT"); v != "" {
		cfg.TLSCertFile = v
	}
	if v := os.Getenv("WTG_EPUSH_TLS_KEY"); v != "" {
		cfg.TLSKeyFile = v
	}
	if v := os.Getenv("WTG_EPUSH_TLS_CLIENT_CA"); v != "" {
		cfg.TLSClientCAFile = v
	}
	cidrStr := os.Getenv("WTG_EPUSH_ALLOW_CIDRS")

	fs := flag.NewFlagSet("mci-edge-push", flag.ContinueOnError)
	fs.StringVar(&cidrStr, "allow-cidrs", cidrStr, "외부 접근 허용 CIDR (콤마 구분, 비면 모두 허용)")
	fs.StringVar(&cfg.ListenAddr, "listen", cfg.ListenAddr, "HTTP/WS listen 주소")
	fs.StringVar(&cfg.UpstreamGRPC, "upstream", cfg.UpstreamGRPC, "Internal mci-push gRPC endpoint")
	fs.StringVar(&cfg.SubscriberID, "subscriber-id", cfg.SubscriberID, "edge 식별자")
	fs.BoolVar(&cfg.DevMode, "dev", cfg.DevMode, "개발 모드 — JWT 우회")
	fs.IntVar(&cfg.SendQueueSize, "send-queue", cfg.SendQueueSize, "ws 클라이언트별 큐 크기")
	fs.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "로그 레벨")
	fs.DurationVar(&cfg.DialTimeout, "dial-timeout", cfg.DialTimeout, "gRPC dial timeout")
	fs.Float64Var(&cfg.IPRatePerSec, "ip-rate", cfg.IPRatePerSec, "fallback rate limit TPS (룰 매칭 안 된 path, 0=비활성)")
	fs.IntVar(&cfg.IPBurst, "ip-burst", cfg.IPBurst, "fallback burst 한도")
	fs.StringVar(&cfg.EtcdEndpoints, "etcd", cfg.EtcdEndpoints, "etcd endpoints (콤마 구분, 비면 정적 룰만)")
	fs.StringVar(&cfg.EtcdRateLimitKey, "etcd-ratelimit-key", cfg.EtcdRateLimitKey, "etcd PolicyDoc key (default wtg/ratelimit/edge-push)")
	fs.StringVar(&cfg.RateLimitRedisAddr, "ratelimit-redis", cfg.RateLimitRedisAddr, "Redis addr — rate limit 분산 backend (host:port)")
	fs.StringVar(&cfg.RateLimitRedisPassword, "ratelimit-redis-pass", cfg.RateLimitRedisPassword, "Redis password")
	fs.IntVar(&cfg.RateLimitRedisDB, "ratelimit-redis-db", cfg.RateLimitRedisDB, "Redis DB index")
	fs.StringVar(&cfg.GRPCTLSCertFile, "grpc-tls-cert", cfg.GRPCTLSCertFile, "Upstream gRPC mTLS 클라이언트 cert PEM")
	fs.StringVar(&cfg.GRPCTLSKeyFile, "grpc-tls-key", cfg.GRPCTLSKeyFile, "Upstream gRPC mTLS 클라이언트 key PEM")
	fs.StringVar(&cfg.GRPCTLSCAFile, "grpc-tls-ca", cfg.GRPCTLSCAFile, "Upstream 서버 검증용 CA bundle")
	fs.StringVar(&cfg.GRPCTLSServerName, "grpc-tls-sni", cfg.GRPCTLSServerName, "Upstream SNI / hostname")
	fs.StringVar(&cfg.TLSCertFile, "tls-cert", cfg.TLSCertFile, "외부 TLS 서버 cert PEM (있으면 HTTPS)")
	fs.StringVar(&cfg.TLSKeyFile, "tls-key", cfg.TLSKeyFile, "외부 TLS 서버 key PEM")
	fs.StringVar(&cfg.TLSClientCAFile, "tls-client-ca", cfg.TLSClientCAFile, "외부 mTLS 클라이언트 CA bundle")
	fs.StringVar(&cfg.OtelEndpoint, "otel-endpoint", cfg.OtelEndpoint, "OTel OTLP gRPC endpoint (비면 비활성)")
	fs.BoolVar(&cfg.OtelInsecure, "otel-insecure", cfg.OtelInsecure, "OTel gRPC TLS 없음 (dev)")
	fs.BoolVar(&cfg.OtelStdout, "otel-stdout", cfg.OtelStdout, "OTel span stdout (debug)")
	fs.Float64Var(&cfg.OtelSampleRatio, "otel-sample", cfg.OtelSampleRatio, "OTel 샘플링 비율 (0..1)")

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

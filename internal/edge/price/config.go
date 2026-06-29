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
	"strings"
	"time"

	"github.com/winwaysystems/wtg/pkg/netutil"
	"github.com/winwaysystems/wtg/pkg/ratelimit"
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

	// Rate limit fallback — 룰 매칭 안 된 path 의 한도. 0=비활성.
	IPRatePerSec float64
	IPBurst      int

	// Path-aware rate limit 룰셋. nil → DefaultRateLimitRules().
	RateLimitRules []ratelimit.Rule

	// etcd 기반 hot reload.
	EtcdEndpoints    string
	EtcdRateLimitKey string // default "wtg/ratelimit/edge-price"

	// EtcdCustomerPairsPrefix — customer 별 ws 구독 허용 pair allowlist 의 etcd
	// prefix. 채워지면 CustomerPairPolicy 활성 + watcher 가동. 빈값이면 비활성
	// (글로벌 정책만 적용, backward compat).
	//
	// etcd schema: <prefix><customerID> = JSON []string  (예: ["USD/KRW","EUR/USD"])
	EtcdCustomerPairsPrefix string // default "wtg/customers/"

	// Redis backend — 다중 인스턴스 단일 카운터. 비면 in-memory.
	RateLimitRedisAddr     string
	RateLimitRedisPassword string
	RateLimitRedisDB       int

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

	// ─── Quote stream (Profile 별 마진 적용된 시세) ───────────────────────
	//
	// EnableQuoteStream=true 면 PriceService.SubscribeQuote 를 추가로 호출해
	// Profile-routed CustomerQuote 를 받고, 각 ws 세션의 Profile 과 매칭되는
	// 항목만 fan-out 한다 (Subscriber.profileKey 기준).
	//
	// 기존 PriceService.Subscribe (raw tick broadcast) 도 그대로 동작 — 둘은 독립.
	EnableQuoteStream bool

	// QuoteProfileKeys — Internal 에 보낼 profile_keys 화이트리스트.
	// 비어있으면 모든 Profile 의 quote 를 받는다 (edge 가 사용자별 분기 담당).
	// 좁히면 broker→edge 트래픽 절감.
	QuoteProfileKeys []string

	// EnableCustomerStream — Phase 4c. 활성 시:
	//   - mci-price 와 장기 RegisterCustomer stream 유지 (customerRegManager).
	//   - SubscribeCustomerQuote stream 으로 customer-tag 된 quote 수신.
	//   - ws connect 시 Principal.Usid 를 customer-id 로 자동 등록,
	//     disconnect 시 자동 해제.
	//
	// EnableQuoteStream 과 독립 — Profile-only quote / customer-specific quote
	// 둘 다 받을 수도, 둘 중 하나만 받을 수도 있음.
	EnableCustomerStream bool

	// QuoteSeedPairs — Phase 2 권한 가드의 초기 시드. operator 가 알고 있는
	// 운영 pair 카탈로그를 사전 등록해 첫 quote 도착 전에도 subscribe 가능하게.
	// 비면 passive learning 만 — 첫 quote 까지는 어떤 pair 도 허용 안 됨.
	QuoteSeedPairs []string

	// Phase 4 stale detection — pair 가 이 시간 이상 무음이면 stale 알림 전송.
	// 0 이면 비활성 (옛 동작). default 30s.
	StaleThreshold time.Duration
	// stale scanner 의 주기. default 5s. StaleThreshold=0 이면 무관.
	StaleScanInterval time.Duration

	// Phase 4b admin allowlist — /v1/admin/* endpoint 접근 허용 CIDR.
	// 비면 모든 admin 요청 거부 (default secure). AllowCIDRs (일반 ws) 와
	// 별도로 좁게 운영망에서만 허용 권장.
	AdminAllowCIDRs []*net.IPNet

	// EnvelopeFormat — ws 클라이언트에 어떤 JSON 형식으로 BEST tick 을 송신할지.
	//
	//   "best"   (default) : 현 신규 형식 — {market_id, symbol, seq_num,
	//                        data:{sym, bid, ask, src:"BEST", seq, ts}}
	//                        브라우저 / 신규 클라이언트가 사용.
	//
	//   "legacy" : forwarder/broker subscribe 호환 — {ts, feed:"BEST", seq,
	//              msgtype:"incremental", symbol, entries:[
	//                  {type:"bid", px, qty:0},
	//                  {type:"ask", px, qty:0}
	//              ]}
	//              legacy cs framework 가 mymqd broker subscribe 시 받던 schema
	//              와 1:1. cs 의 parser 코드 변경 없이 ws 로 마이그레이션 가능.
	//
	// 두 형식이 같은 ws endpoint 에서 mix 되진 않는다 — instance 단위 설정.
	// 필요 시 두 인스턴스 (best / legacy) 를 별도 포트에 띄워 운영.
	EnvelopeFormat string

	// OTel TracerProvider — Endpoint 비고 OtelStdout=false 면 비활성. PR 2/3.
	OtelEndpoint    string
	OtelInsecure    bool
	OtelStdout      bool
	OtelSampleRatio float64
}

// DefaultConfig 는 합리적인 디폴트.
func DefaultConfig() Config {
	host, _ := os.Hostname()
	return Config{
		ListenAddr:        ":8083",
		UpstreamGRPC:      "127.0.0.1:50051",
		SubscriberID:      "mci-edge-price@" + host,
		DevMode:           false,
		WsPingInterval:    30 * time.Second,
		WsPongTimeout:     60 * time.Second,
		SendQueueSize:     256,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       120 * time.Second,
		DialTimeout:       5 * time.Second,
		IPRatePerSec:      100,
		IPBurst:           200,
		EtcdRateLimitKey:        "wtg/ratelimit/edge-price",
		EtcdCustomerPairsPrefix: "wtg/customers/",
		LogLevel:          "info",
		EnableQuoteStream: false,
		StaleThreshold:    30 * time.Second,
		StaleScanInterval: 5 * time.Second,
		EnvelopeFormat:    "best",
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
	if v := os.Getenv("WTG_EPRICE_QUOTE_STREAM"); v == "1" || v == "true" {
		cfg.EnableQuoteStream = true
	}
	if v := os.Getenv("WTG_EPRICE_CUSTOMER_STREAM"); v == "1" || v == "true" {
		cfg.EnableCustomerStream = true
	}
	if v := os.Getenv("WTG_EPRICE_ENVELOPE_FORMAT"); v != "" {
		cfg.EnvelopeFormat = v
	}
	if v := os.Getenv("WTG_EPRICE_QUOTE_PROFILES"); v != "" {
		for _, p := range strings.Split(v, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				cfg.QuoteProfileKeys = append(cfg.QuoteProfileKeys, p)
			}
		}
	}
	if v := os.Getenv("WTG_EPRICE_QUOTE_SEED_PAIRS"); v != "" {
		for _, p := range strings.Split(v, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				cfg.QuoteSeedPairs = append(cfg.QuoteSeedPairs, p)
			}
		}
	}
	cidrStr := os.Getenv("WTG_EPRICE_ALLOW_CIDRS")
	adminCidrStr := os.Getenv("WTG_EPRICE_ADMIN_ALLOW_CIDRS")
	seedPairsStr := ""

	fs := flag.NewFlagSet("mci-edge-price", flag.ContinueOnError)
	fs.StringVar(&cidrStr, "allow-cidrs", cidrStr, "외부 접근 허용 CIDR (콤마 구분, 비면 모두 허용)")
	fs.StringVar(&adminCidrStr, "admin-allow-cidrs", adminCidrStr, "Phase 4b admin endpoint 접근 허용 CIDR (콤마 구분, 비면 모두 거부)")
	fs.StringVar(&cfg.ListenAddr, "listen", cfg.ListenAddr, "HTTP/WS listen 주소")
	fs.StringVar(&cfg.UpstreamGRPC, "upstream", cfg.UpstreamGRPC, "Internal mci-price gRPC endpoint")
	fs.StringVar(&cfg.SubscriberID, "subscriber-id", cfg.SubscriberID, "edge 인스턴스 식별자")
	fs.BoolVar(&cfg.DevMode, "dev", cfg.DevMode, "개발 모드 — JWT 검증 우회")
	fs.IntVar(&cfg.SendQueueSize, "send-queue", cfg.SendQueueSize, "ws 클라이언트별 send queue 크기")
	fs.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "로그 레벨")
	fs.DurationVar(&cfg.DialTimeout, "dial-timeout", cfg.DialTimeout, "gRPC dial timeout")
	fs.Float64Var(&cfg.IPRatePerSec, "ip-rate", cfg.IPRatePerSec, "fallback rate limit TPS (룰 매칭 안 된 path, 0=비활성)")
	fs.StringVar(&cfg.EtcdEndpoints, "etcd", cfg.EtcdEndpoints, "etcd endpoints (콤마 구분, 비면 정적 룰만)")
	fs.StringVar(&cfg.EtcdRateLimitKey, "etcd-ratelimit-key", cfg.EtcdRateLimitKey, "etcd PolicyDoc key (default wtg/ratelimit/edge-price)")
	fs.StringVar(&cfg.EtcdCustomerPairsPrefix, "etcd-customer-pairs-prefix", cfg.EtcdCustomerPairsPrefix, "customer 별 ws 구독 허용 pair allowlist etcd prefix. 빈값이면 비활성 (글로벌 정책만)")
	fs.StringVar(&cfg.RateLimitRedisAddr, "ratelimit-redis", cfg.RateLimitRedisAddr, "Redis addr — rate limit 분산 backend (host:port)")
	fs.StringVar(&cfg.RateLimitRedisPassword, "ratelimit-redis-pass", cfg.RateLimitRedisPassword, "Redis password")
	fs.IntVar(&cfg.RateLimitRedisDB, "ratelimit-redis-db", cfg.RateLimitRedisDB, "Redis DB index")
	fs.IntVar(&cfg.IPBurst, "ip-burst", cfg.IPBurst, "IP burst 한도")
	fs.StringVar(&cfg.GRPCTLSCertFile, "grpc-tls-cert", cfg.GRPCTLSCertFile, "Upstream gRPC mTLS 클라이언트 cert PEM")
	fs.StringVar(&cfg.GRPCTLSKeyFile, "grpc-tls-key", cfg.GRPCTLSKeyFile, "Upstream gRPC mTLS 클라이언트 key PEM")
	fs.StringVar(&cfg.GRPCTLSCAFile, "grpc-tls-ca", cfg.GRPCTLSCAFile, "Upstream 서버 검증용 CA bundle")
	fs.StringVar(&cfg.GRPCTLSServerName, "grpc-tls-sni", cfg.GRPCTLSServerName, "Upstream SNI / hostname")
	fs.StringVar(&cfg.TLSCertFile, "tls-cert", cfg.TLSCertFile, "외부 TLS 서버 cert PEM (있으면 HTTPS)")
	fs.StringVar(&cfg.TLSKeyFile, "tls-key", cfg.TLSKeyFile, "외부 TLS 서버 key PEM")
	fs.StringVar(&cfg.TLSClientCAFile, "tls-client-ca", cfg.TLSClientCAFile, "외부 mTLS 클라이언트 CA bundle")
	fs.BoolVar(&cfg.EnableQuoteStream, "quote-stream", cfg.EnableQuoteStream, "PriceService.SubscribeQuote 활성 (Profile-routed CustomerQuote)")
	fs.BoolVar(&cfg.EnableCustomerStream, "customer-stream", cfg.EnableCustomerStream, "Phase 4c: RegisterCustomer + SubscribeCustomerQuote 활성 (customer-specific 마진)")
	quoteProfStr := strings.Join(cfg.QuoteProfileKeys, ",")
	fs.StringVar(&quoteProfStr, "quote-profiles", quoteProfStr, "수신할 profile_keys 화이트리스트 (콤마 구분, 빈값=모두)")
	seedPairsStr = strings.Join(cfg.QuoteSeedPairs, ",")
	fs.StringVar(&seedPairsStr, "quote-seed-pairs", seedPairsStr, "Phase 2 권한 가드 초기 시드 pair (콤마 구분, 빈값=passive learning 만)")
	fs.DurationVar(&cfg.StaleThreshold, "stale-threshold", cfg.StaleThreshold, "Phase 4 stale 알림 임계 (0=비활성, default 30s)")
	fs.DurationVar(&cfg.StaleScanInterval, "stale-scan", cfg.StaleScanInterval, "stale scanner 주기 (default 5s)")
	fs.StringVar(&cfg.EnvelopeFormat, "envelope-format", cfg.EnvelopeFormat, "ws envelope 형식 — 'best' (default, 신규) 또는 'legacy' (broker subscribe 호환, msgtype/entries)")
	fs.StringVar(&cfg.OtelEndpoint, "otel-endpoint", cfg.OtelEndpoint, "OTel OTLP gRPC endpoint (비면 비활성)")
	fs.BoolVar(&cfg.OtelInsecure, "otel-insecure", cfg.OtelInsecure, "OTel gRPC TLS 없음 (dev)")
	fs.BoolVar(&cfg.OtelStdout, "otel-stdout", cfg.OtelStdout, "OTel span stdout (debug)")
	fs.Float64Var(&cfg.OtelSampleRatio, "otel-sample", cfg.OtelSampleRatio, "OTel 샘플링 비율 (0..1)")

	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	// quoteProfStr 가 flag 로 갱신될 수 있으므로 다시 슬라이스로 변환.
	cfg.QuoteProfileKeys = cfg.QuoteProfileKeys[:0]
	for _, p := range strings.Split(quoteProfStr, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			cfg.QuoteProfileKeys = append(cfg.QuoteProfileKeys, p)
		}
	}
	cfg.QuoteSeedPairs = cfg.QuoteSeedPairs[:0]
	for _, p := range strings.Split(seedPairsStr, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			cfg.QuoteSeedPairs = append(cfg.QuoteSeedPairs, p)
		}
	}
	if cidrs, err := netutil.ParseCIDRs(cidrStr); err != nil {
		return cfg, err
	} else {
		cfg.AllowCIDRs = cidrs
	}
	if cidrs, err := netutil.ParseCIDRs(adminCidrStr); err != nil {
		return cfg, err
	} else {
		cfg.AdminAllowCIDRs = cidrs
	}
	return cfg, nil
}

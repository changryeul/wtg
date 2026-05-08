// Package api 는 WTG 의 REST API 서비스 (mci-api).
//
// 외환 트레이딩 web 클라이언트의 sync RPC 진입점. JSON POST 요청을 받아서
// MyMQ broker 로 트랜잭션을 보내고 응답을 JSON 으로 회신한다.
package api

import (
	"flag"
	"os"
	"strconv"
	"time"

	"github.com/winwaysystems/wtg/pkg/routing"
)

// Config 는 mci-api 의 런타임 설정.
//
// 1단계는 flag + env 기반으로 단순하게 처리하고, 추후 YAML 설정 파일로 확장한다.
// 운영 환경에서는 K8s ConfigMap / Secret 으로 env 주입이 일반적.
type Config struct {
	// HTTP 서버 listen 주소 (예: ":8080" 또는 "127.0.0.1:8080").
	ListenAddr string

	// MyMQ broker 접속 정보.
	BrokerHost string
	BrokerPort int

	// 이 mci-api 인스턴스의 ApplName 식별자.
	// 다중 인스턴스 운영 시 Instance 와 결합되어 "mci-api-NN" 으로 등록됨.
	ApplName string
	Instance int

	// 핸드셰이크 / read 타임아웃.
	DialTimeout      time.Duration
	HandshakeTimeout time.Duration

	// HTTP 서버 기본 timeout.
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	IdleTimeout  time.Duration

	// MyMQ 호출 기본 timeout (Call 의 ctx deadline 으로 사용).
	BrokerCallTimeout time.Duration

	// 개발 모드 — JWT 검증 우회, X-WTG-User 헤더로 usid 주입 허용.
	// 운영에서는 반드시 false.
	DevMode bool

	// 로그 레벨 ("debug" / "info" / "warn" / "error"). 기본 "info".
	LogLevel string

	// TLS — 인증서 경로가 있으면 ListenAndServeTLS.
	// TLSClientCAFile 이 함께 채워지면 mTLS (클라이언트 인증서 요구).
	TLSCertFile     string
	TLSKeyFile      string
	TLSClientCAFile string

	// TrustEdgeHeaders — true 면 X-WTG-SID 헤더(mci-edge-api 가 주입) 를 신뢰.
	// 외부 노출 listener 에서는 절대 활성화 금지. mTLS 로 보호된 Internal 망
	// 가정 — auth.md §4 흐름의 Internal 단.
	TrustEdgeHeaders bool

	// etcd 엔드포인트 (콤마 구분). 비어있으면 InMemoryRegistry — 단일 인스턴스용.
	// 다중 mci-api / mci-admin 환경에서는 etcd 로 룰 동기화.
	EtcdEndpoints  string
	EtcdRoutesPath string // "wtg/routes/" 디폴트
	EtcdPolicyKey  string // "wtg/policy" 디폴트

	// Broker TLS — broker 측에 TLS listener 가 있어야 사용 가능.
	// docs/broker-tls.md 참조. 비면 plain TCP (기존 동작).
	BrokerTLSCertFile string
	BrokerTLSKeyFile  string
	BrokerTLSCAFile   string
	BrokerTLSSNI      string

	// DevRoutesFile — DevMode 시 라우팅 룰 시드 JSON 경로. mci-admin 과 같은
	// 파일을 양쪽이 읽으면 dev stack 의 alias 시드가 일관됨. 비면 hardcode default.
	DevRoutesFile string

	// DevRoutesPolicy — cfg ↔ in-memory 동기화 정책. "additive" (기본) | "sync".
	// pkg/routing.SeedPolicy 참조. mci-admin 과 정책을 일치시키지 않으면 두 인스턴스의
	// 라우팅 룰이 갈라질 수 있으므로 양쪽 모두 같은 값으로 운영하길 권장.
	DevRoutesPolicy string

	// DevPolicyURL — DevMode 에서 mci-api 가 mci-admin 의 정책 snapshot 을
	// 주기적으로 fetch 할 endpoint (예: http://127.0.0.1:9090/v1/admin/policy).
	// etcd 미사용 시 split-brain (admin 토글이 api 에 안 닿음) 을 막는
	// 가벼운 우회. 운영에선 비우고 etcd 사용.
	DevPolicyURL string
}

// DefaultConfig 는 합리적인 디폴트가 채워진 Config 를 반환한다.
func DefaultConfig() Config {
	return Config{
		ListenAddr:        ":8080",
		BrokerHost:        "127.0.0.1",
		BrokerPort:        11217,
		ApplName:          "mci-api",
		Instance:          0,
		DialTimeout:       5 * time.Second,
		HandshakeTimeout:  5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		BrokerCallTimeout: 5 * time.Second,
		DevMode:           false,
		LogLevel:          "info",
	}
}

// LoadConfig 는 flag + env 를 합쳐 Config 를 채운다.
// flag 는 explicit 우선, 없으면 env, 없으면 default.
//
// 환경변수 prefix: WTG_API_*
//
//	WTG_API_LISTEN, WTG_API_BROKER_HOST, WTG_API_BROKER_PORT,
//	WTG_API_APPL, WTG_API_INSTANCE, WTG_API_DEV_MODE, WTG_API_LOG_LEVEL
func LoadConfig(args []string) (Config, error) {
	cfg := DefaultConfig()

	// env 로 먼저 덮어쓰기.
	if v := os.Getenv("WTG_API_LISTEN"); v != "" {
		cfg.ListenAddr = v
	}
	if v := os.Getenv("WTG_API_BROKER_HOST"); v != "" {
		cfg.BrokerHost = v
	}
	if v := os.Getenv("WTG_API_BROKER_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.BrokerPort = n
		}
	}
	if v := os.Getenv("WTG_API_APPL"); v != "" {
		cfg.ApplName = v
	}
	if v := os.Getenv("WTG_API_INSTANCE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Instance = n
		}
	}
	if v := os.Getenv("WTG_API_DEV_MODE"); v == "1" || v == "true" {
		cfg.DevMode = true
	}
	if v := os.Getenv("WTG_API_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
	if v := os.Getenv("WTG_API_TLS_CERT"); v != "" {
		cfg.TLSCertFile = v
	}
	if v := os.Getenv("WTG_API_TLS_KEY"); v != "" {
		cfg.TLSKeyFile = v
	}
	if v := os.Getenv("WTG_API_TLS_CLIENT_CA"); v != "" {
		cfg.TLSClientCAFile = v
	}
	if v := os.Getenv("WTG_API_TRUST_EDGE"); v == "1" || v == "true" {
		cfg.TrustEdgeHeaders = true
	}
	if v := os.Getenv("WTG_API_ETCD"); v != "" {
		cfg.EtcdEndpoints = v
	}
	if v := os.Getenv("WTG_API_ETCD_PREFIX"); v != "" {
		cfg.EtcdRoutesPath = v
	}
	if v := os.Getenv("WTG_API_ETCD_POLICY_KEY"); v != "" {
		cfg.EtcdPolicyKey = v
	}
	if v := os.Getenv("WTG_API_BROKER_TLS_CERT"); v != "" {
		cfg.BrokerTLSCertFile = v
	}
	if v := os.Getenv("WTG_API_BROKER_TLS_KEY"); v != "" {
		cfg.BrokerTLSKeyFile = v
	}
	if v := os.Getenv("WTG_API_BROKER_TLS_CA"); v != "" {
		cfg.BrokerTLSCAFile = v
	}
	if v := os.Getenv("WTG_API_BROKER_TLS_SNI"); v != "" {
		cfg.BrokerTLSSNI = v
	}
	if v := os.Getenv("WTG_API_DEV_ROUTES_FILE"); v != "" {
		cfg.DevRoutesFile = v
	}
	if v := os.Getenv("WTG_API_DEV_ROUTES_POLICY"); v != "" {
		cfg.DevRoutesPolicy = v
	}
	if v := os.Getenv("WTG_API_DEV_POLICY_URL"); v != "" {
		cfg.DevPolicyURL = v
	}

	// flag 가 env 를 덮어씀 (CLI 가 가장 강력).
	fs := flag.NewFlagSet("mci-api", flag.ContinueOnError)
	fs.StringVar(&cfg.ListenAddr, "listen", cfg.ListenAddr, "HTTP listen 주소")
	fs.StringVar(&cfg.BrokerHost, "broker-host", cfg.BrokerHost, "mymqd 호스트")
	fs.IntVar(&cfg.BrokerPort, "broker-port", cfg.BrokerPort, "mymqd 포트")
	fs.StringVar(&cfg.ApplName, "appl", cfg.ApplName, "ApplName")
	fs.IntVar(&cfg.Instance, "instance", cfg.Instance, "다중 인스턴스 일련번호 (0=비활성)")
	fs.BoolVar(&cfg.DevMode, "dev", cfg.DevMode, "개발 모드 — JWT 검증 우회")
	fs.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "로그 레벨 debug/info/warn/error")
	fs.DurationVar(&cfg.BrokerCallTimeout, "call-timeout", cfg.BrokerCallTimeout, "MyMQ Call 기본 타임아웃")
	fs.StringVar(&cfg.TLSCertFile, "tls-cert", cfg.TLSCertFile, "TLS 서버 인증서 PEM (있으면 HTTPS)")
	fs.StringVar(&cfg.TLSKeyFile, "tls-key", cfg.TLSKeyFile, "TLS 서버 private key PEM")
	fs.StringVar(&cfg.TLSClientCAFile, "tls-client-ca", cfg.TLSClientCAFile, "클라이언트 mTLS 검증용 CA bundle PEM")
	fs.BoolVar(&cfg.TrustEdgeHeaders, "trust-edge", cfg.TrustEdgeHeaders, "X-WTG-SID 헤더 신뢰 (mci-edge-api 뒤에서만)")
	fs.StringVar(&cfg.EtcdEndpoints, "etcd", cfg.EtcdEndpoints, "라우팅 룰 etcd endpoints (콤마 구분, 비면 in-memory)")
	fs.StringVar(&cfg.EtcdRoutesPath, "etcd-prefix", cfg.EtcdRoutesPath, "etcd 키 prefix (default wtg/routes/)")
	fs.StringVar(&cfg.EtcdPolicyKey, "etcd-policy-key", cfg.EtcdPolicyKey, "etcd 정책 단일 key (default wtg/policy)")
	fs.StringVar(&cfg.BrokerTLSCertFile, "broker-tls-cert", cfg.BrokerTLSCertFile, "broker TLS 클라이언트 cert PEM")
	fs.StringVar(&cfg.BrokerTLSKeyFile, "broker-tls-key", cfg.BrokerTLSKeyFile, "broker TLS 클라이언트 key PEM")
	fs.StringVar(&cfg.BrokerTLSCAFile, "broker-tls-ca", cfg.BrokerTLSCAFile, "broker TLS 서버 검증용 CA bundle")
	fs.StringVar(&cfg.BrokerTLSSNI, "broker-tls-sni", cfg.BrokerTLSSNI, "broker TLS SNI / hostname")
	fs.StringVar(&cfg.DevRoutesFile, "dev-routes-file", cfg.DevRoutesFile, "DevMode 라우팅 룰 시드 JSON 경로 (예: ~/mymq/etc/wtg-routes.json). 비면 hardcode default")
	fs.StringVar(&cfg.DevRoutesPolicy, "dev-routes-policy", cfg.DevRoutesPolicy, "cfg ↔ in-memory 동기화 정책. additive(default) | sync. mci-admin 과 일치시킬 것")
	fs.StringVar(&cfg.DevPolicyURL, "dev-policy-url", cfg.DevPolicyURL, "DevMode 정책 snapshot poll URL (예: http://127.0.0.1:9090/v1/admin/policy). 비우면 비활성")

	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	if _, err := routing.ParseSeedPolicy(cfg.DevRoutesPolicy); err != nil {
		return cfg, err
	}
	return cfg, nil
}

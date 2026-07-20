// Package api 는 WTG 의 REST API 서비스 (mci-api).
//
// 외환 트레이딩 web 클라이언트의 sync RPC 진입점. JSON POST 요청을 받아서
// MyMQ broker 로 트랜잭션을 보내고 응답을 JSON 으로 회신한다.
package api

import (
	"flag"
	"fmt"
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

	// JWTKeyFile — RS256 발급용 RSA private key PEM 경로. 채워지면 login 이
	// access_token (JWT) 을 발급하고 /v1/refresh 가 활성화된다. 비면 기존
	// session_id Bearer 만. public key 는 edge 서비스의 --jwt-pub 으로 배포.
	JWTKeyFile string

	// SvcIncDir / SvcCommonHeaderFile — svc I/O 명세 (C 헤더) 디렉토리.
	// 채워지면 /v1/tx·/v1/login 의 data(JSON object) 를 [COMHDR][Input]
	// 고정폭 전문으로 자동 조립하고 응답도 필드별 JSON 으로 파싱한다.
	// mci-admin 의 --svc-inc-dir / --svc-common-header 와 동일 형식.
	SvcIncDir           string
	SvcCommonHeaderFile string

	// LoginMode — /v1/login 동작. "legacy"(기본, 빈값 동일) = 단일 LOGON +
	// cookie_t. "chain" = 엔진 인증 사슬 (W1101S02→W1130A02, cookie 없음).
	// chain 은 SvcIncDir 필수 — 전문 조립이 svc I/O 명세에 의존.
	// docs/superpowers/specs/2026-07-20-engine-login-chain-design.md 참조.
	LoginMode string

	// chain 모드 단계별 alias override (비면 W1101S02/W1130A02/W1130A03).
	LoginCertAlias    string
	LoginSessionAlias string
	LoginLogoutAlias  string

	// 로그 레벨 ("debug" / "info" / "warn" / "error"). 기본 "info".
	LogLevel string

	// OTel TracerProvider — Endpoint 비면 비활성. Endpoint 채우면 OTLP gRPC,
	// OtelStdout=true 면 stdout (debug). 자세히는 docs/broker-tracing.md.
	OtelEndpoint    string  // 예: "otel-collector:4317"
	OtelInsecure    bool    // dev — TLS 없이 gRPC
	OtelStdout      bool    // debug — span 을 stdout 으로
	OtelSampleRatio float64 // 0..1 (default 1.0 = 전체)

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

	// etcd 클라이언트 TLS — 모두 비어있으면 평문.
	// routing.New 와 policy.StartEtcdSync 양쪽에 동일 인증서 사용.
	EtcdTLSCertFile   string
	EtcdTLSKeyFile    string
	EtcdTLSCAFile     string
	EtcdTLSServerName string

	// ─── 사용자 프로파일 (Site/Tier 권위 출처) ─────────────────────────────
	//
	// 우선순위:
	//   1) UserProfilesPrefix 채워지면 etcd watch (운영 표준 — admin 으로 관리)
	//   2) UserProfilesFile 채워지면 정적 JSON (dev / 단일 인스턴스)
	//   3) 둘 다 비면 resolver 없음 → 모든 사용자가 빈 Profile (degraded — raw 시세만)
	UserProfilesFile   string
	UserProfilesPrefix string // default 사용 시 "wtg/auth/user-profiles/"

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

	// ── Redis (운영 다중 인스턴스 session / refresh 공유) ──
	//
	// RedisAddr 비어있으면 in-memory store 사용 (단일 인스턴스 dev / 1차).
	// 운영 다중 mci-api 에서는 필수 — 한 인스턴스가 발급한 session/refresh 가
	// 다른 인스턴스에서도 보여야 한다. SetSessionStore/SetRefreshStore 로
	// 명시 주입한 경우 그쪽이 우선.
	//
	// 형식 (auth.md §7 참조):
	//   단일       : "127.0.0.1:6379"
	//   Sentinel   : "addr1,addr2,addr3" + RedisSentinelMaster
	//   Cluster    : RedisMode="cluster" + 콤마 분리 endpoints
	RedisAddr           string
	RedisPassword       string
	RedisDB             int
	RedisPrefix         string // default "wtg:auth"
	RedisSentinelMaster string // sentinel 사용 시 master 이름
	RedisMode           string // "direct" | "sentinel" | "cluster" (빈값=auto: 1addr→direct, 2+→sentinel)

	// TxRingSize — 매매 audit ring 크기 (in-memory circular). 0 이면 비활성
	// (기존 동작). >0 이면 최근 N 건 매매가 /v1/admin/recent-tx 로 노출.
	// 운영 권장 1000~5000.
	TxRingSize int

	// Idempotency 정책 — `Idempotency-Key` 헤더 처리.
	// IdempotencyEnabled=false 면 헤더 있어도 무시 (기존 동작).
	// Backend:
	//   IdempotencyRedis 비면   → Memory (단일 인스턴스 / dev)
	//   채워지면                → Redis (다중 인스턴스 공유 reservation)
	// Redis 형식은 일반 redis-* flag (--redis, --redis-prefix) 와 별개 — 동일
	// addr 이라도 prefix 분리. IdempotencyRedisPrefix 빈값이면 "wtg:idem:".
	IdempotencyEnabled     bool
	IdempotencyTTL         time.Duration // 0 이면 5분 default (pkg/idempotency.Options)
	IdempotencyRedisPrefix string
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
	if v := os.Getenv("WTG_API_ETCD_TLS_CERT"); v != "" {
		cfg.EtcdTLSCertFile = v
	}
	if v := os.Getenv("WTG_API_ETCD_TLS_KEY"); v != "" {
		cfg.EtcdTLSKeyFile = v
	}
	if v := os.Getenv("WTG_API_ETCD_TLS_CA"); v != "" {
		cfg.EtcdTLSCAFile = v
	}
	if v := os.Getenv("WTG_API_ETCD_TLS_SNI"); v != "" {
		cfg.EtcdTLSServerName = v
	}
	if v := os.Getenv("WTG_API_USER_PROFILES_FILE"); v != "" {
		cfg.UserProfilesFile = v
	}
	if v := os.Getenv("WTG_API_USER_PROFILES_PREFIX"); v != "" {
		cfg.UserProfilesPrefix = v
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
	if v := os.Getenv("WTG_API_REDIS_ADDR"); v != "" {
		cfg.RedisAddr = v
	}
	if v := os.Getenv("WTG_API_REDIS_PASSWORD"); v != "" {
		cfg.RedisPassword = v
	}
	if v := os.Getenv("WTG_API_REDIS_DB"); v != "" {
		fmt.Sscanf(v, "%d", &cfg.RedisDB)
	}
	if v := os.Getenv("WTG_API_REDIS_PREFIX"); v != "" {
		cfg.RedisPrefix = v
	}
	if v := os.Getenv("WTG_API_REDIS_MASTER"); v != "" {
		cfg.RedisSentinelMaster = v
	}
	if v := os.Getenv("WTG_API_REDIS_MODE"); v != "" {
		cfg.RedisMode = v
	}
	if v := os.Getenv("WTG_API_LOGIN_MODE"); v != "" {
		cfg.LoginMode = v
	}

	// flag 가 env 를 덮어씀 (CLI 가 가장 강력).
	fs := flag.NewFlagSet("mci-api", flag.ContinueOnError)
	fs.StringVar(&cfg.ListenAddr, "listen", cfg.ListenAddr, "HTTP listen 주소")
	fs.StringVar(&cfg.BrokerHost, "broker-host", cfg.BrokerHost, "mymqd 호스트")
	fs.IntVar(&cfg.BrokerPort, "broker-port", cfg.BrokerPort, "mymqd 포트")
	fs.StringVar(&cfg.ApplName, "appl", cfg.ApplName, "ApplName")
	fs.IntVar(&cfg.Instance, "instance", cfg.Instance, "다중 인스턴스 일련번호 (0=비활성)")
	fs.BoolVar(&cfg.DevMode, "dev", cfg.DevMode, "개발 모드 — JWT 검증 우회")
	fs.StringVar(&cfg.JWTKeyFile, "jwt-key", cfg.JWTKeyFile, "RS256 JWT 발급용 RSA private key PEM (비면 session_id Bearer 만)")
	fs.StringVar(&cfg.SvcIncDir, "svc-inc-dir", cfg.SvcIncDir, "매매 svc 헤더 디렉터리 (콤마 구분) — data JSON object 자동 전문 조립")
	fs.StringVar(&cfg.SvcCommonHeaderFile, "svc-common-header", cfg.SvcCommonHeaderFile, "공통 transaction 헤더 파일 (comhdr.h)")
	fs.StringVar(&cfg.LoginMode, "login-mode", cfg.LoginMode, "로그인 모드: legacy(기본) | chain (엔진 인증 사슬 W1101S02→W1130A02). chain 은 --svc-inc-dir 필수")
	fs.StringVar(&cfg.LoginCertAlias, "login-cert-alias", cfg.LoginCertAlias, "chain ① 인증서 인증 alias (기본 W1101S02)")
	fs.StringVar(&cfg.LoginSessionAlias, "login-session-alias", cfg.LoginSessionAlias, "chain ③ 세션개설 alias (기본 W1130A02)")
	fs.StringVar(&cfg.LoginLogoutAlias, "login-logout-alias", cfg.LoginLogoutAlias, "chain 로그아웃 반납 alias (기본 W1130A03)")
	fs.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "로그 레벨 debug/info/warn/error")
	fs.StringVar(&cfg.OtelEndpoint, "otel-endpoint", cfg.OtelEndpoint, "OTel OTLP gRPC endpoint (예: otel-collector:4317). 비면 비활성")
	fs.BoolVar(&cfg.OtelInsecure, "otel-insecure", cfg.OtelInsecure, "OTel gRPC TLS 없음 (dev)")
	fs.BoolVar(&cfg.OtelStdout, "otel-stdout", cfg.OtelStdout, "OTel span 을 stdout 출력 (debug)")
	fs.Float64Var(&cfg.OtelSampleRatio, "otel-sample", cfg.OtelSampleRatio, "OTel 샘플링 비율 (0..1, default 1.0=전체)")
	fs.DurationVar(&cfg.BrokerCallTimeout, "call-timeout", cfg.BrokerCallTimeout, "MyMQ Call 기본 타임아웃")
	fs.StringVar(&cfg.TLSCertFile, "tls-cert", cfg.TLSCertFile, "TLS 서버 인증서 PEM (있으면 HTTPS)")
	fs.StringVar(&cfg.TLSKeyFile, "tls-key", cfg.TLSKeyFile, "TLS 서버 private key PEM")
	fs.StringVar(&cfg.TLSClientCAFile, "tls-client-ca", cfg.TLSClientCAFile, "클라이언트 mTLS 검증용 CA bundle PEM")
	fs.BoolVar(&cfg.TrustEdgeHeaders, "trust-edge", cfg.TrustEdgeHeaders, "X-WTG-SID 헤더 신뢰 (mci-edge-api 뒤에서만)")
	fs.StringVar(&cfg.EtcdEndpoints, "etcd", cfg.EtcdEndpoints, "라우팅 룰 etcd endpoints (콤마 구분, 비면 in-memory)")
	fs.StringVar(&cfg.EtcdRoutesPath, "etcd-prefix", cfg.EtcdRoutesPath, "etcd 키 prefix (default wtg/routes/)")
	fs.StringVar(&cfg.EtcdPolicyKey, "etcd-policy-key", cfg.EtcdPolicyKey, "etcd 정책 단일 key (default wtg/policy)")
	fs.StringVar(&cfg.EtcdTLSCertFile, "etcd-tls-cert", cfg.EtcdTLSCertFile, "etcd 클라이언트 cert PEM (mTLS)")
	fs.StringVar(&cfg.EtcdTLSKeyFile, "etcd-tls-key", cfg.EtcdTLSKeyFile, "etcd 클라이언트 key PEM (mTLS)")
	fs.StringVar(&cfg.EtcdTLSCAFile, "etcd-tls-ca", cfg.EtcdTLSCAFile, "etcd 서버 검증용 CA bundle")
	fs.StringVar(&cfg.EtcdTLSServerName, "etcd-tls-sni", cfg.EtcdTLSServerName, "etcd TLS SNI / hostname")
	fs.StringVar(&cfg.UserProfilesFile, "user-profiles", cfg.UserProfilesFile, "사용자 프로파일 JSON 파일 (정적 — etcd prefix 미설정 시)")
	fs.StringVar(&cfg.UserProfilesPrefix, "user-profiles-prefix", cfg.UserProfilesPrefix, "사용자 프로파일 etcd prefix (default wtg/auth/user-profiles/)")
	fs.StringVar(&cfg.BrokerTLSCertFile, "broker-tls-cert", cfg.BrokerTLSCertFile, "broker TLS 클라이언트 cert PEM")
	fs.StringVar(&cfg.BrokerTLSKeyFile, "broker-tls-key", cfg.BrokerTLSKeyFile, "broker TLS 클라이언트 key PEM")
	fs.StringVar(&cfg.BrokerTLSCAFile, "broker-tls-ca", cfg.BrokerTLSCAFile, "broker TLS 서버 검증용 CA bundle")
	fs.StringVar(&cfg.BrokerTLSSNI, "broker-tls-sni", cfg.BrokerTLSSNI, "broker TLS SNI / hostname")
	fs.StringVar(&cfg.DevRoutesFile, "dev-routes-file", cfg.DevRoutesFile, "DevMode 라우팅 룰 시드 JSON 경로 (예: ~/mymq/etc/wtg-routes.json). 비면 hardcode default")
	fs.StringVar(&cfg.DevRoutesPolicy, "dev-routes-policy", cfg.DevRoutesPolicy, "cfg ↔ in-memory 동기화 정책. additive(default) | sync. mci-admin 과 일치시킬 것")
	fs.StringVar(&cfg.DevPolicyURL, "dev-policy-url", cfg.DevPolicyURL, "DevMode 정책 snapshot poll URL (예: http://127.0.0.1:9090/v1/admin/policy). 비우면 비활성")
	fs.StringVar(&cfg.RedisAddr, "redis", cfg.RedisAddr, "session/refresh 공유 redis 주소. 단일 host:port 또는 콤마 분리. 비면 in-memory")
	fs.StringVar(&cfg.RedisPassword, "redis-password", cfg.RedisPassword, "redis 비밀번호")
	fs.IntVar(&cfg.RedisDB, "redis-db", cfg.RedisDB, "redis DB index")
	fs.StringVar(&cfg.RedisPrefix, "redis-prefix", cfg.RedisPrefix, "redis 키 prefix (default wtg:auth)")
	fs.StringVar(&cfg.RedisSentinelMaster, "redis-master", cfg.RedisSentinelMaster, "Sentinel master 이름 (다중 addr + sentinel)")
	fs.StringVar(&cfg.RedisMode, "redis-mode", cfg.RedisMode, "topology 명시: direct / sentinel / cluster (빈값=auto)")
	fs.IntVar(&cfg.TxRingSize, "tx-ring", cfg.TxRingSize, "매매 audit ring 크기 (in-memory, 0=비활성). >0 이면 /v1/admin/recent-tx 노출. 운영 권장 1000~5000")
	fs.BoolVar(&cfg.IdempotencyEnabled, "idempotency", cfg.IdempotencyEnabled, "Idempotency-Key 헤더 처리 활성 (default off — 헤더 무시). 운영 권장 — 중복 매매 차단. backend 는 --redis 가 채워지면 Redis, 비면 Memory (단일 인스턴스).")
	fs.DurationVar(&cfg.IdempotencyTTL, "idempotency-ttl", cfg.IdempotencyTTL, "Idempotency reservation / cached reply TTL (default 5m)")
	fs.StringVar(&cfg.IdempotencyRedisPrefix, "idempotency-redis-prefix", cfg.IdempotencyRedisPrefix, "Idempotency Redis key prefix (default wtg:idem:). --redis 활성 시 sessions/refresh 와 prefix 분리")

	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	if _, err := routing.ParseSeedPolicy(cfg.DevRoutesPolicy); err != nil {
		return cfg, err
	}
	switch cfg.LoginMode {
	case "", "legacy":
	case "chain":
		if cfg.SvcIncDir == "" {
			return cfg, fmt.Errorf("--login-mode=chain 은 --svc-inc-dir 필수 (전문 조립이 svc I/O 명세 의존)")
		}
	default:
		return cfg, fmt.Errorf("--login-mode: %q — legacy | chain 중 하나", cfg.LoginMode)
	}
	return cfg, nil
}

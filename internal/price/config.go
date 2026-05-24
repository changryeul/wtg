// Package price 는 WTG 의 FX 시세 fan-out 서비스 (mci-price).
//
// MyMQ broker 의 PRICE exchange broadcast 를 unsolicited 모드로 받아서
// 심볼별 conflation 후 edge (mci-edge-price) 로 fan-out 한다.
//
// Phase 4 1차 범위:
//   - mymq.Client unsolicited 모드 connect (CONNECT 핸드셰이크)
//   - pushdata 디코딩 (mymq.h 의 struct pushdata)
//   - 심볼별 conflation (latest 만 유지)
//   - fanout 구독자 인터페이스 (1차 stdout / stats stub, Phase 5 에서 gRPC stream)
package price

import (
	"flag"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config 는 mci-price 의 런타임 설정.
type Config struct {
	// 모니터링 / 헬스체크 HTTP listen 주소.
	ListenAddr string

	// gRPC PriceService listen 주소 (Internal → DMZ stream).
	// 비어있으면 gRPC 서버 비활성 (1차 prototype 검증용).
	GRPCAddr string

	// gRPC subscriber 별 큐 크기. 기본 1024.
	GRPCBufSize int

	// MyMQ broker.
	BrokerHost string
	BrokerPort int

	// ApplName + Instance.
	ApplName string
	Instance int

	// 구독할 broker 큐 / exchange.
	QueueName    string
	ExchangeName string // 받을 broadcast 의 exchange (필터링 용)

	// 핸드셰이크 / I/O 타임아웃.
	DialTimeout      time.Duration
	HandshakeTimeout time.Duration

	// HTTP 서버.
	ReadTimeout  time.Duration
	WriteTimeout time.Duration

	// Conflation 진단용 — 통계 출력 주기.
	StatsInterval time.Duration

	// 1차 prototype 출력 옵션 — 처음 N 개 tick 을 stdout 에 dump.
	PrintFirstN int

	// 개발 모드.
	DevMode bool

	LogLevel string

	// gRPC TLS — Internal mci-price 의 PriceService 가 mTLS 요구하도록.
	// CertFile/KeyFile 이 채워지면 gRPC 서버가 TLS, ClientCAFile 도 채워지면 mTLS.
	GRPCTLSCertFile     string
	GRPCTLSKeyFile      string
	GRPCTLSClientCAFile string

	// Broker TLS — docs/broker-tls.md 참조.
	BrokerTLSCertFile string
	BrokerTLSKeyFile  string
	BrokerTLSCAFile   string
	BrokerTLSSNI      string

	// ─── Chart / 봉 영속화 (optional) ─────────────────────────────────────
	//
	// ChartDSN 이 비어있으면 Aggregator/Archiver 비활성 — broker fan-out 만 동작.
	// 즉 1차 prototype (gRPC stream only) 모드는 그대로 사용 가능.
	//
	// 활성 시:
	//   - pgxpool 생성 → PgxInserter → Archiver
	//   - SymbolMap 로드 → Aggregator (JSONCookerDecoder)
	//   - Server.AddConsumer(agg) — broker tick → 봉 누적 → 봉 close → DB INSERT
	ChartDSN          string
	ChartPoolMaxConns int

	// SymbolsFile 은 JSON 으로 직렬화된 []quote.SymbolEntry.
	// 비어있으면 SymbolMap 이 empty — Aggregator 가 모든 tick 을 drop (의도된 안전한 default).
	SymbolsFile string

	// Archiver 옵션 — 0 이면 ArchiverOptions defaults.
	ArchiverQueueSize     int
	ArchiverFlushInterval time.Duration
	ArchiverBatchMax      int

	// Aggregator 의 Sweeper 호출 주기 (default 1s).
	AggregatorSweepInterval time.Duration

	// ─── PricingConsumer (Profile 별 마진 적용 후 broker publish) ──────────
	//
	// 두 가지 카탈로그 소스 모드 (상호배타):
	//   1) 정적 파일 — PricingFile + ProfilesFile (개발/소규모 운영)
	//   2) etcd watch — EtcdEndpoints 설정 시 활성 (운영 hot reload)
	//
	// 둘 다 미설정 또는 SymbolMap 미존재 시 PricingConsumer 비활성 — raw fan-out 만.
	//
	// PricingFile : pricing.PricingTable 의 JSON 직렬화.
	// ProfilesFile: []session.Profile 의 JSON 직렬화 (활성 Profile 카탈로그).
	PricingFile  string
	ProfilesFile string

	// ─── etcd hot reload (옵션) ────────────────────────────────────────────
	//
	// 설정되면 SymbolMap / PricingTable / Profiles 가 etcd watch 로 자동 갱신된다.
	// 정적 파일 옵션 (SymbolsFile / PricingFile / ProfilesFile) 보다 우선.
	//
	// etcd key 컨벤션:
	//   <prefix>quote/symbols/<symbol>   → quote.SymbolEntry JSON
	//   <prefix>pricing/table            → pricing.PricingTableDoc JSON (단일 key)
	//   <prefix>price/profiles/<key>     → session.Profile JSON
	EtcdEndpoints   []string      // 콤마 구분 → 호출자 split. 비면 정적 모드.
	EtcdPrefix      string        // default "wtg/" — 끝에 "/" 자동 보정.
	EtcdDialTimeout time.Duration // default 5s.
	EtcdUsername    string
	EtcdPassword    string

	// etcd 클라이언트 TLS — 운영 권장.
	// CertFile/KeyFile 둘 다 비면 client cert 없이 (CA 검증만), 둘 다 채워지면 mTLS.
	// CAFile 빈 값이면 시스템 trust store 사용.
	// 모두 비면 plain TCP.
	EtcdTLSCertFile   string
	EtcdTLSKeyFile    string
	EtcdTLSCAFile     string
	EtcdTLSServerName string // SNI / 호스트네임 검증

	// ─── QuoteID (pkg/quoteid) ──────────────────────────────────────────────
	// Generator + Registry 활성화. QuoteIDInstance 가 비어 있으면 quoteid
	// 비활성 (기존 동작 유지).
	//
	// 운영: 두 mci-price 가 active-active 일 때 인스턴스 prefix 가 달라야 ID
	// 충돌 없음 (예: "A" / "B"). RFC §6 참조.
	QuoteIDInstance        string        // 빈값이면 quoteid 비활성
	QuoteIDValidity        time.Duration // default 500ms — ValidUntil 윈도우
	QuoteIDGrace           time.Duration // default 1s — Registry GC 유예
	QuoteIDRegistryTimeout time.Duration // default 200ms — Put 단위 timeout

	// Redis Registry — 비어 있으면 MemoryRegistry (dev / 단일 인스턴스).
	// 단일 addr (host:port) → 직접 연결.
	// 콤마 구분 addr 목록 → Sentinel FailoverClient — QuoteIDRedisMaster 필요.
	QuoteIDRedisAddr     string
	QuoteIDRedisPassword string
	QuoteIDRedisDB       int
	QuoteIDRedisPrefix   string // default "wtg:quoteid"
	// QuoteIDRedisMaster — Sentinel 사용 시 master 이름. 단일 addr 모드면 무시.
	QuoteIDRedisMaster string // default "wtg-quoteid-master"
}

// DefaultConfig 는 합리적인 디폴트.
func DefaultConfig() Config {
	return Config{
		ListenAddr:       ":8082",
		GRPCAddr:         "",
		GRPCBufSize:      1024,
		BrokerHost:       "127.0.0.1",
		BrokerPort:       11217,
		ApplName:         "mci-price",
		Instance:         0,
		QueueName:        "mci_price",
		ExchangeName:     "PRICE",
		DialTimeout:      5 * time.Second,
		HandshakeTimeout: 5 * time.Second,
		ReadTimeout:      10 * time.Second,
		WriteTimeout:     10 * time.Second,
		StatsInterval:    5 * time.Second,
		PrintFirstN:      0,
		DevMode:          false,
		LogLevel:         "info",

		ChartDSN:                "",
		ChartPoolMaxConns:       5,
		SymbolsFile:             "",
		ArchiverQueueSize:       10000,
		ArchiverFlushInterval:   time.Second,
		ArchiverBatchMax:        500,
		AggregatorSweepInterval: time.Second,

		PricingFile:  "",
		ProfilesFile: "",

		EtcdEndpoints:   nil,
		EtcdPrefix:      "wtg/",
		EtcdDialTimeout: 5 * time.Second,

		EtcdTLSCertFile:   "",
		EtcdTLSKeyFile:    "",
		EtcdTLSCAFile:     "",
		EtcdTLSServerName: "",

		QuoteIDInstance:        "",
		QuoteIDValidity:        500 * time.Millisecond,
		QuoteIDGrace:           time.Second,
		QuoteIDRegistryTimeout: 200 * time.Millisecond,
		QuoteIDRedisAddr:       "",
		QuoteIDRedisPrefix:     "wtg:quoteid",
		QuoteIDRedisMaster:     "wtg-quoteid-master",
	}
}

// LoadConfig 는 flag + env 를 합쳐 Config 를 채운다.
//
// 환경변수 prefix: WTG_PRICE_*
func LoadConfig(args []string) (Config, error) {
	cfg := DefaultConfig()

	if v := os.Getenv("WTG_PRICE_LISTEN"); v != "" {
		cfg.ListenAddr = v
	}
	if v := os.Getenv("WTG_PRICE_GRPC"); v != "" {
		cfg.GRPCAddr = v
	}
	if v := os.Getenv("WTG_PRICE_BROKER_HOST"); v != "" {
		cfg.BrokerHost = v
	}
	if v := os.Getenv("WTG_PRICE_BROKER_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.BrokerPort = n
		}
	}
	if v := os.Getenv("WTG_PRICE_APPL"); v != "" {
		cfg.ApplName = v
	}
	if v := os.Getenv("WTG_PRICE_INSTANCE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Instance = n
		}
	}
	if v := os.Getenv("WTG_PRICE_QUEUE"); v != "" {
		cfg.QueueName = v
	}
	if v := os.Getenv("WTG_PRICE_EXCHANGE"); v != "" {
		cfg.ExchangeName = v
	}
	if v := os.Getenv("WTG_PRICE_DEV_MODE"); v == "1" || v == "true" {
		cfg.DevMode = true
	}
	if v := os.Getenv("WTG_PRICE_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
	if v := os.Getenv("WTG_PRICE_GRPC_TLS_CERT"); v != "" {
		cfg.GRPCTLSCertFile = v
	}
	if v := os.Getenv("WTG_PRICE_GRPC_TLS_KEY"); v != "" {
		cfg.GRPCTLSKeyFile = v
	}
	if v := os.Getenv("WTG_PRICE_GRPC_TLS_CLIENT_CA"); v != "" {
		cfg.GRPCTLSClientCAFile = v
	}
	if v := os.Getenv("WTG_PRICE_BROKER_TLS_CERT"); v != "" {
		cfg.BrokerTLSCertFile = v
	}
	if v := os.Getenv("WTG_PRICE_BROKER_TLS_KEY"); v != "" {
		cfg.BrokerTLSKeyFile = v
	}
	if v := os.Getenv("WTG_PRICE_BROKER_TLS_CA"); v != "" {
		cfg.BrokerTLSCAFile = v
	}
	if v := os.Getenv("WTG_PRICE_BROKER_TLS_SNI"); v != "" {
		cfg.BrokerTLSSNI = v
	}
	if v := os.Getenv("WTG_PRICE_CHART_DSN"); v != "" {
		cfg.ChartDSN = v
	}
	if v := os.Getenv("WTG_PRICE_CHART_POOL"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.ChartPoolMaxConns = n
		}
	}
	if v := os.Getenv("WTG_PRICE_SYMBOLS_FILE"); v != "" {
		cfg.SymbolsFile = v
	}
	if v := os.Getenv("WTG_PRICE_PRICING_FILE"); v != "" {
		cfg.PricingFile = v
	}
	if v := os.Getenv("WTG_PRICE_PROFILES_FILE"); v != "" {
		cfg.ProfilesFile = v
	}
	if v := os.Getenv("WTG_PRICE_ETCD"); v != "" {
		cfg.EtcdEndpoints = splitCSV(v)
	}
	if v := os.Getenv("WTG_PRICE_ETCD_PREFIX"); v != "" {
		cfg.EtcdPrefix = v
	}
	if v := os.Getenv("WTG_PRICE_ETCD_USER"); v != "" {
		cfg.EtcdUsername = v
	}
	if v := os.Getenv("WTG_PRICE_ETCD_PASS"); v != "" {
		cfg.EtcdPassword = v
	}
	if v := os.Getenv("WTG_PRICE_ETCD_TLS_CERT"); v != "" {
		cfg.EtcdTLSCertFile = v
	}
	if v := os.Getenv("WTG_PRICE_ETCD_TLS_KEY"); v != "" {
		cfg.EtcdTLSKeyFile = v
	}
	if v := os.Getenv("WTG_PRICE_ETCD_TLS_CA"); v != "" {
		cfg.EtcdTLSCAFile = v
	}
	if v := os.Getenv("WTG_PRICE_ETCD_TLS_SNI"); v != "" {
		cfg.EtcdTLSServerName = v
	}

	if v := os.Getenv("WTG_PRICE_QUOTEID_INSTANCE"); v != "" {
		cfg.QuoteIDInstance = v
	}
	if v := os.Getenv("WTG_PRICE_QUOTEID_VALIDITY"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.QuoteIDValidity = d
		}
	}
	if v := os.Getenv("WTG_PRICE_QUOTEID_GRACE"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.QuoteIDGrace = d
		}
	}
	if v := os.Getenv("WTG_PRICE_QUOTEID_REDIS"); v != "" {
		cfg.QuoteIDRedisAddr = v
	}
	if v := os.Getenv("WTG_PRICE_QUOTEID_REDIS_PASS"); v != "" {
		cfg.QuoteIDRedisPassword = v
	}
	if v := os.Getenv("WTG_PRICE_QUOTEID_REDIS_DB"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.QuoteIDRedisDB = n
		}
	}
	if v := os.Getenv("WTG_PRICE_QUOTEID_REDIS_PREFIX"); v != "" {
		cfg.QuoteIDRedisPrefix = v
	}
	if v := os.Getenv("WTG_PRICE_QUOTEID_REDIS_MASTER"); v != "" {
		cfg.QuoteIDRedisMaster = v
	}

	fs := flag.NewFlagSet("mci-price", flag.ContinueOnError)
	fs.StringVar(&cfg.ListenAddr, "listen", cfg.ListenAddr, "HTTP 모니터링 listen 주소")
	fs.StringVar(&cfg.GRPCAddr, "grpc", cfg.GRPCAddr, "gRPC PriceService listen 주소 (비어있으면 비활성)")
	fs.IntVar(&cfg.GRPCBufSize, "grpc-buf", cfg.GRPCBufSize, "gRPC 구독자별 큐 크기")
	fs.StringVar(&cfg.BrokerHost, "broker-host", cfg.BrokerHost, "mymqd 호스트")
	fs.IntVar(&cfg.BrokerPort, "broker-port", cfg.BrokerPort, "mymqd 포트")
	fs.StringVar(&cfg.ApplName, "appl", cfg.ApplName, "ApplName")
	fs.IntVar(&cfg.Instance, "instance", cfg.Instance, "다중 인스턴스 일련번호")
	fs.StringVar(&cfg.QueueName, "queue", cfg.QueueName, "broker 측 큐 이름")
	fs.StringVar(&cfg.ExchangeName, "exchange", cfg.ExchangeName, "필터링 대상 exchange")
	fs.IntVar(&cfg.PrintFirstN, "print", cfg.PrintFirstN, "처음 N 개 tick 을 stdout 에 dump (0 = 비활성)")
	fs.DurationVar(&cfg.StatsInterval, "stats", cfg.StatsInterval, "통계 출력 주기")
	fs.BoolVar(&cfg.DevMode, "dev", cfg.DevMode, "개발 모드")
	fs.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "로그 레벨 debug/info/warn/error")
	fs.StringVar(&cfg.GRPCTLSCertFile, "grpc-tls-cert", cfg.GRPCTLSCertFile, "gRPC TLS 서버 cert PEM")
	fs.StringVar(&cfg.GRPCTLSKeyFile, "grpc-tls-key", cfg.GRPCTLSKeyFile, "gRPC TLS 서버 key PEM")
	fs.StringVar(&cfg.GRPCTLSClientCAFile, "grpc-tls-client-ca", cfg.GRPCTLSClientCAFile, "gRPC mTLS 클라이언트 CA bundle")
	fs.StringVar(&cfg.BrokerTLSCertFile, "broker-tls-cert", cfg.BrokerTLSCertFile, "broker TLS 클라이언트 cert PEM")
	fs.StringVar(&cfg.BrokerTLSKeyFile, "broker-tls-key", cfg.BrokerTLSKeyFile, "broker TLS 클라이언트 key PEM")
	fs.StringVar(&cfg.BrokerTLSCAFile, "broker-tls-ca", cfg.BrokerTLSCAFile, "broker TLS 서버 검증용 CA bundle")
	fs.StringVar(&cfg.BrokerTLSSNI, "broker-tls-sni", cfg.BrokerTLSSNI, "broker TLS SNI / hostname")
	fs.StringVar(&cfg.ChartDSN, "chart-dsn", cfg.ChartDSN, "TimescaleDB DSN (비어있으면 봉 영속 비활성)")
	fs.IntVar(&cfg.ChartPoolMaxConns, "chart-pool", cfg.ChartPoolMaxConns, "chart pgx pool 최대 connection")
	fs.StringVar(&cfg.SymbolsFile, "symbols", cfg.SymbolsFile, "SymbolMap JSON 파일 경로 ([]quote.SymbolEntry)")
	fs.IntVar(&cfg.ArchiverQueueSize, "arc-queue", cfg.ArchiverQueueSize, "Archiver in-memory 큐 크기")
	fs.DurationVar(&cfg.ArchiverFlushInterval, "arc-flush", cfg.ArchiverFlushInterval, "Archiver flush interval")
	fs.IntVar(&cfg.ArchiverBatchMax, "arc-batch", cfg.ArchiverBatchMax, "Archiver batch INSERT 최대 행수")
	fs.DurationVar(&cfg.AggregatorSweepInterval, "agg-sweep", cfg.AggregatorSweepInterval, "Aggregator 만료 봉 sweeper 주기")
	fs.StringVar(&cfg.PricingFile, "pricing", cfg.PricingFile, "PricingTable JSON 파일 (etcd 비활성 시)")
	fs.StringVar(&cfg.ProfilesFile, "profiles", cfg.ProfilesFile, "활성 Profile 카탈로그 JSON ([]session.Profile, etcd 비활성 시)")
	etcdStr := strings.Join(cfg.EtcdEndpoints, ",")
	fs.StringVar(&etcdStr, "etcd", etcdStr, "etcd endpoints 콤마 구분 (설정 시 hot reload 활성, 정적 파일 옵션 무시)")
	fs.StringVar(&cfg.EtcdPrefix, "etcd-prefix", cfg.EtcdPrefix, "etcd key prefix (default wtg/)")
	fs.DurationVar(&cfg.EtcdDialTimeout, "etcd-dial-timeout", cfg.EtcdDialTimeout, "etcd dial timeout")
	fs.StringVar(&cfg.EtcdTLSCertFile, "etcd-tls-cert", cfg.EtcdTLSCertFile, "etcd 클라이언트 cert PEM (mTLS)")
	fs.StringVar(&cfg.EtcdTLSKeyFile, "etcd-tls-key", cfg.EtcdTLSKeyFile, "etcd 클라이언트 key PEM (mTLS)")
	fs.StringVar(&cfg.EtcdTLSCAFile, "etcd-tls-ca", cfg.EtcdTLSCAFile, "etcd 서버 검증용 CA bundle")
	fs.StringVar(&cfg.EtcdTLSServerName, "etcd-tls-sni", cfg.EtcdTLSServerName, "etcd TLS SNI / hostname")

	// ── QuoteID (pkg/quoteid) ─────────────────────────────────────────────
	fs.StringVar(&cfg.QuoteIDInstance, "quoteid-instance", cfg.QuoteIDInstance,
		"QuoteID Generator 인스턴스 prefix (예: A / B). 빈값이면 quoteid 비활성")
	fs.DurationVar(&cfg.QuoteIDValidity, "quoteid-validity", cfg.QuoteIDValidity,
		"QuoteID ValidUntil 윈도우")
	fs.DurationVar(&cfg.QuoteIDGrace, "quoteid-grace", cfg.QuoteIDGrace,
		"Registry GC 유예 (ValidUntil 이후 추가 보존 시간)")
	fs.DurationVar(&cfg.QuoteIDRegistryTimeout, "quoteid-reg-timeout", cfg.QuoteIDRegistryTimeout,
		"Registry.Put 단위 timeout")
	fs.StringVar(&cfg.QuoteIDRedisAddr, "quoteid-redis", cfg.QuoteIDRedisAddr,
		"Redis 주소. 단일 host:port → 직접 연결. 콤마 구분 다중 → Sentinel FailoverClient. 비면 MemoryRegistry")
	fs.StringVar(&cfg.QuoteIDRedisPassword, "quoteid-redis-pass", cfg.QuoteIDRedisPassword, "Redis 비밀번호")
	fs.IntVar(&cfg.QuoteIDRedisDB, "quoteid-redis-db", cfg.QuoteIDRedisDB, "Redis DB index")
	fs.StringVar(&cfg.QuoteIDRedisPrefix, "quoteid-redis-prefix", cfg.QuoteIDRedisPrefix, "Redis 키 prefix")
	fs.StringVar(&cfg.QuoteIDRedisMaster, "quoteid-redis-master", cfg.QuoteIDRedisMaster,
		"Sentinel master 이름 (다중 addr 일 때만 사용)")

	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	// etcd endpoints flag → slice 재구성.
	if etcdStr != "" {
		cfg.EtcdEndpoints = splitCSV(etcdStr)
	}
	return cfg, nil
}

// splitCSV 는 콤마 구분 문자열을 trim 후 slice 로.
func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

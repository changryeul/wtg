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

	// QuotePublishBroker — true 면 customer quote 를 broker ExchangeQuote 로도 publish
	// (legacy fan-out path). false 면 gRPC SubscribeQuote stream 만 사용 — broker
	// 의 시세 부하 0. 시세는 broker bypass 권장 (broker SIGABRT 회피).
	// default true (backward compat) — wtgctl 가 false 로 override 권장.
	QuotePublishBroker bool

	// MyMQ broker.
	BrokerHost string
	BrokerPort int

	// ApplName + Instance.
	ApplName string
	Instance int

	// 구독할 broker 큐 / exchange.
	QueueName    string
	ExchangeName string // 받을 broadcast 의 exchange (필터링 용)

	// SubBufferSize — pkg/mymq.Client 의 subCh (unsolicited 수신 채널) 깊이.
	// 0 이면 pkg/mymq default 256. 고부하에서 broker→consumer backpressure 가
	// 보이면 (SubDrops 증가) 8192 이상으로 운영.
	SubBufferSize int

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

	// HTTP TLS — /v1/quoteid/* 및 /v1/price-stats 등 HTTP 서버 (--listen) 의
	// 직접 TLS termination. reverse proxy 없이 native TLS 가능.
	// CertFile/KeyFile 이 채워지면 https, ClientCAFile 도 채워지면 mTLS.
	// pkg/tlsutil.Reloader 가 cert 핫리로드 — SIGHUP 또는 파일 watch.
	HTTPTLSCertFile     string
	HTTPTLSKeyFile      string
	HTTPTLSClientCAFile string

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

	// ─── BestConsumer (다중시장 best 호가 산정) ───────────────────────────
	//
	// cooker 가 시장별 raw 시세를 publish 하면 BestConsumer 가 (Symbol, Source)
	// 별로 최신 quote 를 캐시하고 매 raw tick 마다 best (max bid, min ask) 를
	// 재계산해 합성 BEST Tick 을 downstream consumer 에 전달한다. Aggregator/
	// PricingConsumer/gRPC 는 이 best 만 본다 — raw 다중시장이 흡수되어 single
	// stream 처럼 보인다. 자세한 동작은 best.go 참조.
	//
	// 단일 feed 환경 (cooker 가 이미 best 합산해서 publish) 에서도 안전 — best
	// of 1 source = 그 자체. BestEnabled=false 로 끄면 raw 가 downstream 까지
	// last-write-wins 로 흐름 (이전 동작).
	BestEnabled      bool
	BestMaxStaleness time.Duration // 0 이면 30s 기본, 음수면 stale 검사 비활성

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

	// QuoteIDRedisMode — 명시적 토폴로지 선택. 빈값 ("") 이면 자동:
	//   addr 1개 → "direct" (단일 인스턴스)
	//   addr 2+   → "sentinel" (FailoverClient)
	// "cluster" 로 명시하면 ClusterClient 사용 (Redis Cluster 토폴로지).
	QuoteIDRedisMode string

	// QuoteIDEnginesAllowlist — 허용 engine_id 목록 (RBAC). 빈 목록이면
	// 검사 비활성 — 모든 caller 통과 (back-compat).
	//
	// caller 별 정책 분리 — mTLS CN 만으로는 부족한 경우 (예: 같은 cert 를
	// 공유하지만 다른 매칭 엔진 인스턴스). engine_id 가 allowlist 에 없으면
	// PermissionDenied / HTTP 403.
	//
	// 정적 (이 슬라이스) 와 동적 (etcd watcher) 둘 중 하나만 활성. etcd 가
	// active 이고 prefix 가 채워지면 정적은 초기 seed 로만 쓰이고 etcd watcher 가
	// 최종 결정.
	QuoteIDEnginesAllowlist []string

	// QuoteIDEnginesEtcdPrefix — etcd watch 활성 prefix. 빈값이면 etcd 갱신
	// 비활성 (정적 슬라이스만 사용). 일반: cfg.EtcdPrefix + "quoteid/engines/".
	QuoteIDEnginesEtcdPrefix string

	// ─── QuoteID async Put (hot path 비블록) ──────────────────────────────
	// AsyncRegistry wrapper 활성 — QuoteIDAsyncQueue > 0 일 때만.
	// PricingConsumer.OnTick 의 Registry.Put 호출이 채널로 즉시 반환 →
	// 백그라운드 worker 가 batch + Pipeline 으로 Redis 송신.
	//
	// trade-off : at-least-once 아님 (queue 가득 시 drop). 운영 best-effort
	// audit 정책상 허용. drop 률은 quoteid_async_dropped 메트릭으로 감시.
	QuoteIDAsyncQueue    int           // default 0 = 동기 (이전 동작)
	QuoteIDAsyncFlush    time.Duration // default 5ms
	QuoteIDAsyncBatchMax int           // default 200
	QuoteIDAsyncTimeout  time.Duration // default 200ms — 단일 PutMany 의 ctx timeout
}

// DefaultConfig 는 합리적인 디폴트.
func DefaultConfig() Config {
	return Config{
		ListenAddr:         ":8082",
		GRPCAddr:           "",
		GRPCBufSize:        1024,
		QuotePublishBroker: true,
		BrokerHost:         "127.0.0.1",
		BrokerPort:         11217,
		ApplName:           "mci-price",
		Instance:           0,
		QueueName:          "", // 빈값 — broker 가 _CLIENT_ type 으로 등록해야 QfUnsolRep
		// 가 동작 (publish.c:185-189 의 _REPRESENTATIVE_UNSOL_RECVER_ 매칭).
		// mci-push 와 동일 컨벤션. 비빈값을 주면 broker 가 broadcast 안 보냄.
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

		BestEnabled:      true,
		BestMaxStaleness: 30 * time.Second,

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
	if v := os.Getenv("WTG_PRICE_HTTP_TLS_CERT"); v != "" {
		cfg.HTTPTLSCertFile = v
	}
	if v := os.Getenv("WTG_PRICE_HTTP_TLS_KEY"); v != "" {
		cfg.HTTPTLSKeyFile = v
	}
	if v := os.Getenv("WTG_PRICE_HTTP_TLS_CLIENT_CA"); v != "" {
		cfg.HTTPTLSClientCAFile = v
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
	if v := os.Getenv("WTG_PRICE_QUOTEID_REDIS_MODE"); v != "" {
		cfg.QuoteIDRedisMode = v
	}
	if v := os.Getenv("WTG_PRICE_QUOTEID_ENGINES"); v != "" {
		cfg.QuoteIDEnginesAllowlist = splitCSV(v)
	}
	if v := os.Getenv("WTG_PRICE_QUOTEID_ENGINES_ETCD"); v != "" {
		cfg.QuoteIDEnginesEtcdPrefix = v
	}
	if v := os.Getenv("WTG_PRICE_QUOTEID_ASYNC_QUEUE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.QuoteIDAsyncQueue = n
		}
	}
	if v := os.Getenv("WTG_PRICE_QUOTEID_ASYNC_FLUSH"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.QuoteIDAsyncFlush = d
		}
	}
	if v := os.Getenv("WTG_PRICE_QUOTEID_ASYNC_BATCH"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.QuoteIDAsyncBatchMax = n
		}
	}
	if v := os.Getenv("WTG_PRICE_QUOTEID_ASYNC_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.QuoteIDAsyncTimeout = d
		}
	}

	fs := flag.NewFlagSet("mci-price", flag.ContinueOnError)
	fs.StringVar(&cfg.ListenAddr, "listen", cfg.ListenAddr, "HTTP 모니터링 listen 주소")
	fs.StringVar(&cfg.GRPCAddr, "grpc", cfg.GRPCAddr, "gRPC PriceService listen 주소 (비어있으면 비활성)")
	fs.BoolVar(&cfg.QuotePublishBroker, "quote-publish-broker", cfg.QuotePublishBroker, "customer quote 를 broker 로도 publish (legacy). false 면 gRPC SubscribeQuote 만 사용 — broker 부하 분리")
	fs.IntVar(&cfg.GRPCBufSize, "grpc-buf", cfg.GRPCBufSize, "gRPC 구독자별 큐 크기")
	fs.StringVar(&cfg.BrokerHost, "broker-host", cfg.BrokerHost, "mymqd 호스트")
	fs.IntVar(&cfg.BrokerPort, "broker-port", cfg.BrokerPort, "mymqd 포트")
	fs.StringVar(&cfg.ApplName, "appl", cfg.ApplName, "ApplName")
	fs.IntVar(&cfg.Instance, "instance", cfg.Instance, "다중 인스턴스 일련번호")
	fs.StringVar(&cfg.QueueName, "queue", cfg.QueueName, "broker 측 큐 이름")
	fs.StringVar(&cfg.ExchangeName, "exchange", cfg.ExchangeName, "필터링 대상 exchange")
	fs.IntVar(&cfg.SubBufferSize, "sub-buffer", cfg.SubBufferSize, "broker subscribe 채널 (subCh) 버퍼 크기 (0=pkg/mymq default 256)")
	fs.IntVar(&cfg.PrintFirstN, "print", cfg.PrintFirstN, "처음 N 개 tick 을 stdout 에 dump (0 = 비활성)")
	fs.DurationVar(&cfg.StatsInterval, "stats", cfg.StatsInterval, "통계 출력 주기")
	fs.BoolVar(&cfg.DevMode, "dev", cfg.DevMode, "개발 모드")
	fs.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "로그 레벨 debug/info/warn/error")
	fs.StringVar(&cfg.GRPCTLSCertFile, "grpc-tls-cert", cfg.GRPCTLSCertFile, "gRPC TLS 서버 cert PEM")
	fs.StringVar(&cfg.GRPCTLSKeyFile, "grpc-tls-key", cfg.GRPCTLSKeyFile, "gRPC TLS 서버 key PEM")
	fs.StringVar(&cfg.GRPCTLSClientCAFile, "grpc-tls-client-ca", cfg.GRPCTLSClientCAFile, "gRPC mTLS 클라이언트 CA bundle")
	fs.StringVar(&cfg.HTTPTLSCertFile, "http-tls-cert", cfg.HTTPTLSCertFile, "HTTP TLS 서버 cert PEM (비면 plain HTTP)")
	fs.StringVar(&cfg.HTTPTLSKeyFile, "http-tls-key", cfg.HTTPTLSKeyFile, "HTTP TLS 서버 key PEM")
	fs.StringVar(&cfg.HTTPTLSClientCAFile, "http-tls-client-ca", cfg.HTTPTLSClientCAFile, "HTTP mTLS 클라이언트 CA bundle")
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
	fs.BoolVar(&cfg.BestEnabled, "best", cfg.BestEnabled, "다중시장 best 호가 산정 활성 (raw tick 들을 합산해 합성 BEST 만 downstream 에 흘림)")
	fs.DurationVar(&cfg.BestMaxStaleness, "best-staleness", cfg.BestMaxStaleness, "feed quote 가 이 시간 이상 갱신 없으면 best 계산에서 제외 (0=30s 기본, 음수=비활성)")
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
		"Sentinel master 이름 (다중 addr + sentinel 모드)")
	fs.StringVar(&cfg.QuoteIDRedisMode, "quoteid-redis-mode", cfg.QuoteIDRedisMode,
		"명시 토폴로지: direct / sentinel / cluster (빈값=auto: 1addr→direct, 2+→sentinel)")
	enginesStr := strings.Join(cfg.QuoteIDEnginesAllowlist, ",")
	fs.StringVar(&enginesStr, "quoteid-engines", enginesStr,
		"허용 engine_id 콤마구분 (빈값=RBAC 비활성). 예: matching-A,matching-B")
	fs.StringVar(&cfg.QuoteIDEnginesEtcdPrefix, "quoteid-engines-etcd", cfg.QuoteIDEnginesEtcdPrefix,
		"etcd watch prefix (예: wtg/quoteid/engines/) — 채우면 hot reload, 빈값=정적만")
	fs.IntVar(&cfg.QuoteIDAsyncQueue, "quoteid-async-queue", cfg.QuoteIDAsyncQueue,
		"AsyncRegistry queue size. >0 면 Put 비동기 batch (hot path 비블록). 0 = 동기")
	fs.DurationVar(&cfg.QuoteIDAsyncFlush, "quoteid-async-flush", cfg.QuoteIDAsyncFlush,
		"AsyncRegistry flush interval (default 5ms)")
	fs.IntVar(&cfg.QuoteIDAsyncBatchMax, "quoteid-async-batch", cfg.QuoteIDAsyncBatchMax,
		"AsyncRegistry batch 최대 (default 200)")
	fs.DurationVar(&cfg.QuoteIDAsyncTimeout, "quoteid-async-timeout", cfg.QuoteIDAsyncTimeout,
		"AsyncRegistry 단일 PutMany ctx timeout (default 200ms)")

	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	// etcd endpoints flag → slice 재구성.
	if etcdStr != "" {
		cfg.EtcdEndpoints = splitCSV(etcdStr)
	}
	if enginesStr != "" {
		cfg.QuoteIDEnginesAllowlist = splitCSV(enginesStr)
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

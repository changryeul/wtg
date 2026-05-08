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

	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	return cfg, nil
}

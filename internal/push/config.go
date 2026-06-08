// Package push 는 WTG 의 WebSocket fan-out 서비스 (mci-push).
//
// MyMQ broker 의 unsolicited 메시지(체결, 주문상태, 알림)를 받아 사용자
// 단말의 WebSocket 으로 그대로 전달한다. 메시지 종류별 transformer 를 두지
// 않는다 (인증 위임 / passthrough 패턴 동일 적용).
package push

import (
	"flag"
	"os"
	"strconv"
	"time"
)

// Config 는 mci-push 의 런타임 설정.
type Config struct {
	// HTTP/WebSocket listen 주소.
	ListenAddr string

	// gRPC PushService listen 주소 (Internal → DMZ stream).
	// 비어있으면 gRPC 서버 비활성.
	GRPCAddr string

	// gRPC subscriber 별 큐 크기.
	GRPCBufSize int

	// MyMQ broker 접속 정보.
	BrokerHost string
	BrokerPort int

	// NoBroker — broker 연결 자체를 시도 안 함 (HTTP push only 모드).
	// Phase 2.7 사전 옵션 또는 dev/test 시나리오 (broker 없이 mci-push 단독 부팅).
	// 활성 시:
	//   - mymq.Open 호출 skip → broker subscribe 비활성
	//   - dispatcher 는 HTTP inject 만 소비 (recvHTTP 카운터)
	//   - ws fan-out / gRPC PushService 정상 동작 (broker 무관)
	//   - 부팅 로그에 "broker 비활성" 명시
	NoBroker bool

	// Phase-1 PoC — broker 우회 HTTP push endpoint (POST /v1/internal/push) 의 인증
	// shared secret. 빈값 = 인증 disable (dev 전용). 운영은 명시 설정 + mTLS 같이.
	PushSecret string

	// ApplName / Instance.
	ApplName string
	Instance int

	// Queue 이름 — broker 측 cfg 와 일치해야 함.
	QueueName string

	// 핸드셰이크 / I/O 타임아웃.
	DialTimeout      time.Duration
	HandshakeTimeout time.Duration
	ReadTimeout      time.Duration
	WriteTimeout     time.Duration
	IdleTimeout      time.Duration

	// WebSocket 측 ping/pong 간격.
	WsPingInterval time.Duration
	WsPongTimeout  time.Duration

	// 사용자별 send queue 크기. 가득 차면 slow consumer 로 간주하고 끊는다.
	SendQueueSize int

	// 개발 모드 — JWT 검증 우회.
	DevMode bool

	// 로그 레벨.
	LogLevel string

	// gRPC TLS — Internal mci-push 의 PushService 가 mTLS 요구하도록.
	// CertFile/KeyFile 채우면 gRPC 서버가 TLS, ClientCAFile 도 채우면 mTLS.
	GRPCTLSCertFile     string
	GRPCTLSKeyFile      string
	GRPCTLSClientCAFile string

	// HTTP TLS — HTTP/WS listener (ListenAddr) 에 TLS 적용.
	// CertFile/KeyFile 채우면 HTTPS, ClientCAFile 도 채우면 mTLS (운영 svc → mci-push).
	// Phase 2.4: POST /v1/internal/push 의 인증을 secret 대신/추가로 client cert 로.
	// mTLS 와 PushSecret 은 독립 — 둘 다 활성 시 이중 검증.
	HTTPTLSCertFile     string
	HTTPTLSKeyFile      string
	HTTPTLSClientCAFile string

	// Broker TLS — docs/broker-tls.md 참조.
	BrokerTLSCertFile string
	BrokerTLSKeyFile  string
	BrokerTLSCAFile   string
	BrokerTLSSNI      string

	// OTel TracerProvider — Endpoint 비고 OtelStdout=false 면 비활성.
	OtelEndpoint    string
	OtelInsecure    bool
	OtelStdout      bool
	OtelSampleRatio float64
}

// DefaultConfig 는 합리적인 디폴트.
func DefaultConfig() Config {
	return Config{
		ListenAddr:  ":8081",
		GRPCAddr:    "",
		GRPCBufSize: 1024,
		BrokerHost:  "127.0.0.1",
		BrokerPort:  11217,
		ApplName:    "mci-push",
		Instance:    0,
		// queue 이름이 비어있어야 broker 가 client 를 _CLIENT_ type 으로 등록한다
		// (publish.c:185-189 가 _CLIENT_ 만 publish 후보로 인정 — _SERVER_ 면 후보
		// 에서 빠져 user-targeted unsolicited 가 도달하지 못함). representative
		// receiver 로서 모든 publish 를 받으려면 빈 값이 정답.
		QueueName:        "",
		DialTimeout:      5 * time.Second,
		HandshakeTimeout: 5 * time.Second,
		ReadTimeout:      60 * time.Second,
		WriteTimeout:     10 * time.Second,
		IdleTimeout:      120 * time.Second,
		WsPingInterval:   30 * time.Second,
		WsPongTimeout:    60 * time.Second,
		SendQueueSize:    256,
		DevMode:          false,
		LogLevel:         "info",
	}
}

// LoadConfig 는 flag + env 를 합쳐 Config 를 채운다.
//
// 환경변수 prefix: WTG_PUSH_*
func LoadConfig(args []string) (Config, error) {
	cfg := DefaultConfig()

	if v := os.Getenv("WTG_PUSH_LISTEN"); v != "" {
		cfg.ListenAddr = v
	}
	if v := os.Getenv("WTG_PUSH_GRPC"); v != "" {
		cfg.GRPCAddr = v
	}
	if v := os.Getenv("WTG_PUSH_BROKER_HOST"); v != "" {
		cfg.BrokerHost = v
	}
	if v := os.Getenv("WTG_PUSH_BROKER_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.BrokerPort = n
		}
	}
	if v := os.Getenv("WTG_PUSH_APPL"); v != "" {
		cfg.ApplName = v
	}
	if v := os.Getenv("WTG_PUSH_INSTANCE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Instance = n
		}
	}
	if v := os.Getenv("WTG_PUSH_QUEUE"); v != "" {
		cfg.QueueName = v
	}
	if v := os.Getenv("WTG_PUSH_DEV_MODE"); v == "1" || v == "true" {
		cfg.DevMode = true
	}
	if v := os.Getenv("WTG_PUSH_NO_BROKER"); v == "1" || v == "true" {
		cfg.NoBroker = true
	}
	if v := os.Getenv("WTG_PUSH_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
	if v := os.Getenv("WTG_PUSH_GRPC_TLS_CERT"); v != "" {
		cfg.GRPCTLSCertFile = v
	}
	if v := os.Getenv("WTG_PUSH_GRPC_TLS_KEY"); v != "" {
		cfg.GRPCTLSKeyFile = v
	}
	if v := os.Getenv("WTG_PUSH_GRPC_TLS_CLIENT_CA"); v != "" {
		cfg.GRPCTLSClientCAFile = v
	}
	if v := os.Getenv("WTG_PUSH_HTTP_TLS_CERT"); v != "" {
		cfg.HTTPTLSCertFile = v
	}
	if v := os.Getenv("WTG_PUSH_HTTP_TLS_KEY"); v != "" {
		cfg.HTTPTLSKeyFile = v
	}
	if v := os.Getenv("WTG_PUSH_HTTP_TLS_CLIENT_CA"); v != "" {
		cfg.HTTPTLSClientCAFile = v
	}
	if v := os.Getenv("WTG_PUSH_BROKER_TLS_CERT"); v != "" {
		cfg.BrokerTLSCertFile = v
	}
	if v := os.Getenv("WTG_PUSH_BROKER_TLS_KEY"); v != "" {
		cfg.BrokerTLSKeyFile = v
	}
	if v := os.Getenv("WTG_PUSH_BROKER_TLS_CA"); v != "" {
		cfg.BrokerTLSCAFile = v
	}
	if v := os.Getenv("WTG_PUSH_BROKER_TLS_SNI"); v != "" {
		cfg.BrokerTLSSNI = v
	}

	fs := flag.NewFlagSet("mci-push", flag.ContinueOnError)
	fs.StringVar(&cfg.ListenAddr, "listen", cfg.ListenAddr, "HTTP/WS listen 주소")
	fs.StringVar(&cfg.GRPCAddr, "grpc", cfg.GRPCAddr, "gRPC PushService listen 주소 (비어있으면 비활성)")
	fs.IntVar(&cfg.GRPCBufSize, "grpc-buf", cfg.GRPCBufSize, "gRPC 구독자별 큐 크기")
	fs.StringVar(&cfg.BrokerHost, "broker-host", cfg.BrokerHost, "mymqd 호스트")
	fs.IntVar(&cfg.BrokerPort, "broker-port", cfg.BrokerPort, "mymqd 포트")
	fs.StringVar(&cfg.PushSecret, "push-secret", cfg.PushSecret,
		"POST /v1/internal/push 의 X-Push-Secret 헤더 인증. 빈값 = 인증 disable (dev only)")
	fs.StringVar(&cfg.ApplName, "appl", cfg.ApplName, "ApplName")
	fs.IntVar(&cfg.Instance, "instance", cfg.Instance, "다중 인스턴스 일련번호")
	fs.StringVar(&cfg.QueueName, "queue", cfg.QueueName, "broker 측 큐 이름 (mymqd.cfg 와 일치)")
	fs.BoolVar(&cfg.DevMode, "dev", cfg.DevMode, "개발 모드 — JWT 검증 우회")
	fs.BoolVar(&cfg.NoBroker, "no-broker", cfg.NoBroker,
		"broker 연결 skip — HTTP push only 모드 (dev/test 또는 Phase 2.7 사전 옵션)")
	fs.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "로그 레벨 debug/info/warn/error")
	fs.IntVar(&cfg.SendQueueSize, "send-queue", cfg.SendQueueSize, "사용자별 send queue 크기")
	fs.DurationVar(&cfg.WsPingInterval, "ws-ping", cfg.WsPingInterval, "WebSocket ping 간격")
	fs.DurationVar(&cfg.WsPongTimeout, "ws-pong-timeout", cfg.WsPongTimeout, "WebSocket pong 응답 timeout")
	fs.StringVar(&cfg.GRPCTLSCertFile, "grpc-tls-cert", cfg.GRPCTLSCertFile, "gRPC TLS 서버 cert PEM")
	fs.StringVar(&cfg.GRPCTLSKeyFile, "grpc-tls-key", cfg.GRPCTLSKeyFile, "gRPC TLS 서버 key PEM")
	fs.StringVar(&cfg.GRPCTLSClientCAFile, "grpc-tls-client-ca", cfg.GRPCTLSClientCAFile, "gRPC mTLS 클라이언트 CA bundle")
	fs.StringVar(&cfg.HTTPTLSCertFile, "http-tls-cert", cfg.HTTPTLSCertFile, "HTTP TLS 서버 cert PEM (HTTPS 활성)")
	fs.StringVar(&cfg.HTTPTLSKeyFile, "http-tls-key", cfg.HTTPTLSKeyFile, "HTTP TLS 서버 key PEM")
	fs.StringVar(&cfg.HTTPTLSClientCAFile, "http-tls-client-ca", cfg.HTTPTLSClientCAFile,
		"HTTP mTLS 클라이언트 CA bundle (POST /v1/internal/push 의 client cert 검증)")
	fs.StringVar(&cfg.BrokerTLSCertFile, "broker-tls-cert", cfg.BrokerTLSCertFile, "broker TLS 클라이언트 cert PEM")
	fs.StringVar(&cfg.BrokerTLSKeyFile, "broker-tls-key", cfg.BrokerTLSKeyFile, "broker TLS 클라이언트 key PEM")
	fs.StringVar(&cfg.BrokerTLSCAFile, "broker-tls-ca", cfg.BrokerTLSCAFile, "broker TLS 서버 검증용 CA bundle")
	fs.StringVar(&cfg.BrokerTLSSNI, "broker-tls-sni", cfg.BrokerTLSSNI, "broker TLS SNI / hostname")
	fs.StringVar(&cfg.OtelEndpoint, "otel-endpoint", cfg.OtelEndpoint, "OTel OTLP gRPC endpoint (비면 비활성)")
	fs.BoolVar(&cfg.OtelInsecure, "otel-insecure", cfg.OtelInsecure, "OTel gRPC TLS 없음 (dev)")
	fs.BoolVar(&cfg.OtelStdout, "otel-stdout", cfg.OtelStdout, "OTel span stdout (debug)")
	fs.Float64Var(&cfg.OtelSampleRatio, "otel-sample", cfg.OtelSampleRatio, "OTel 샘플링 비율 (0..1)")

	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	return cfg, nil
}

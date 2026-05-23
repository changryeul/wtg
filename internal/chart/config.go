// Package chart 는 WTG (Winway Trading Gateway) 의 OHLC 챠트 서비스 (mci-chart).
//
// 책임 범위:
//
//   - TimescaleDB 의 quote_bars 에서 봉 데이터 조회
//   - REST API: GET /v1/chart?pair=...&tf=...&from=...&to=...
//   - 헬스체크 + stats
//
// 책임 밖 (의도적 제외):
//
//   - 봉 생성/INSERT — mci-price 의 Aggregator/Archiver 책임
//   - 실시간 tick stream — mci-edge-price 의 ws 가 별도 담당
//
// 클라이언트는 mci-chart 로 historical 봉을 받고, mci-edge-price 로 라이브
// tick 을 받아 "마지막 봉만 in-progress 표시" 하는 패턴이 표준.
package chart

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config 는 mci-chart 의 런타임 설정.
type Config struct {
	// HTTP listen 주소.
	ListenAddr string

	// TimescaleDB 연결 DSN (예: "postgres://user:pass@host:5432/wtg").
	// pgxpool.New 에 그대로 전달.
	DSN string

	// pgx 연결 풀 크기 (default 10).
	PoolMaxConns int

	// HTTP timeouts.
	ReadTimeout  time.Duration
	WriteTimeout time.Duration

	// 쿼리당 반환 가능한 최대 row 수 (DoS 보호, default 10000).
	QueryMaxRows int

	// 로그 레벨.
	LogLevel string

	// 통계 출력 주기.
	StatsInterval time.Duration

	// ─── 라이브 봉 stream (옵션) ───────────────────────────────────────────
	//
	// UpstreamGRPC 가 채워지면 mci-price 의 PriceService.SubscribeBar 를 호출해
	// 라이브 봉 close 이벤트를 받아 ws 클라이언트에게 fan-out.
	// 비면 REST 만 동작 (historical 봉).
	UpstreamGRPC      string
	GRPCTLSCertFile   string
	GRPCTLSKeyFile    string
	GRPCTLSCAFile     string
	GRPCTLSServerName string
	SubscriberID      string // SubscribeBar 호출자 식별 (default mci-chart@host)
	WsPingInterval    time.Duration
	WsPongTimeout     time.Duration
	WsSendQueueSize   int
}

// DefaultConfig 는 운영 디폴트.
func DefaultConfig() Config {
	host, _ := os.Hostname()
	return Config{
		ListenAddr:      ":8086",
		PoolMaxConns:    10,
		ReadTimeout:     10 * time.Second,
		WriteTimeout:    30 * time.Second,
		QueryMaxRows:    10000,
		LogLevel:        "info",
		StatsInterval:   30 * time.Second,
		WsPingInterval:  30 * time.Second,
		WsPongTimeout:   60 * time.Second,
		WsSendQueueSize: 256,
		SubscriberID:    "mci-chart@" + host,
	}
}

// LoadConfig 는 flag + env 를 합쳐 Config 를 채운다.
//
// 환경변수 prefix: WTG_CHART_*
func LoadConfig(args []string) (Config, error) {
	cfg := DefaultConfig()

	if v := os.Getenv("WTG_CHART_LISTEN"); v != "" {
		cfg.ListenAddr = v
	}
	if v := os.Getenv("WTG_CHART_DSN"); v != "" {
		cfg.DSN = v
	}
	if v := os.Getenv("WTG_CHART_POOL"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.PoolMaxConns = n
		}
	}
	if v := os.Getenv("WTG_CHART_MAX_ROWS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.QueryMaxRows = n
		}
	}
	if v := os.Getenv("WTG_CHART_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
	if v := os.Getenv("WTG_CHART_UPSTREAM_GRPC"); v != "" {
		cfg.UpstreamGRPC = v
	}
	if v := os.Getenv("WTG_CHART_GRPC_TLS_CERT"); v != "" {
		cfg.GRPCTLSCertFile = v
	}
	if v := os.Getenv("WTG_CHART_GRPC_TLS_KEY"); v != "" {
		cfg.GRPCTLSKeyFile = v
	}
	if v := os.Getenv("WTG_CHART_GRPC_TLS_CA"); v != "" {
		cfg.GRPCTLSCAFile = v
	}
	if v := os.Getenv("WTG_CHART_GRPC_TLS_SNI"); v != "" {
		cfg.GRPCTLSServerName = v
	}
	if v := os.Getenv("WTG_CHART_SUBSCRIBER"); v != "" {
		cfg.SubscriberID = v
	}

	fs := flag.NewFlagSet("mci-chart", flag.ContinueOnError)
	fs.StringVar(&cfg.ListenAddr, "listen", cfg.ListenAddr, "HTTP listen 주소")
	fs.StringVar(&cfg.DSN, "dsn", cfg.DSN, "TimescaleDB DSN")
	fs.IntVar(&cfg.PoolMaxConns, "pool", cfg.PoolMaxConns, "pgx pool 최대 connection")
	fs.IntVar(&cfg.QueryMaxRows, "max-rows", cfg.QueryMaxRows, "쿼리당 반환 row 상한")
	fs.DurationVar(&cfg.ReadTimeout, "read-timeout", cfg.ReadTimeout, "HTTP read timeout")
	fs.DurationVar(&cfg.WriteTimeout, "write-timeout", cfg.WriteTimeout, "HTTP write timeout")
	fs.DurationVar(&cfg.StatsInterval, "stats", cfg.StatsInterval, "통계 출력 주기")
	fs.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "로그 레벨 debug/info/warn/error")
	fs.StringVar(&cfg.UpstreamGRPC, "upstream", cfg.UpstreamGRPC, "mci-price gRPC endpoint (비면 라이브 stream 비활성)")
	fs.StringVar(&cfg.SubscriberID, "subscriber-id", cfg.SubscriberID, "SubscribeBar 호출자 식별")
	fs.StringVar(&cfg.GRPCTLSCertFile, "grpc-tls-cert", cfg.GRPCTLSCertFile, "Upstream gRPC mTLS 클라이언트 cert PEM")
	fs.StringVar(&cfg.GRPCTLSKeyFile, "grpc-tls-key", cfg.GRPCTLSKeyFile, "Upstream gRPC mTLS 클라이언트 key PEM")
	fs.StringVar(&cfg.GRPCTLSCAFile, "grpc-tls-ca", cfg.GRPCTLSCAFile, "Upstream 서버 검증용 CA")
	fs.StringVar(&cfg.GRPCTLSServerName, "grpc-tls-sni", cfg.GRPCTLSServerName, "Upstream SNI / hostname")
	fs.DurationVar(&cfg.WsPingInterval, "ws-ping", cfg.WsPingInterval, "ws ping 주기")
	fs.DurationVar(&cfg.WsPongTimeout, "ws-pong", cfg.WsPongTimeout, "ws pong timeout")
	fs.IntVar(&cfg.WsSendQueueSize, "ws-queue", cfg.WsSendQueueSize, "ws 클라이언트별 send queue 크기")

	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// Validate 는 필수 항목을 검사.
func (c Config) Validate() error {
	if c.ListenAddr == "" {
		return fmt.Errorf("chart: ListenAddr 필수")
	}
	if c.DSN == "" {
		return fmt.Errorf("chart: DSN 필수 (--dsn 또는 WTG_CHART_DSN)")
	}
	if c.QueryMaxRows <= 0 {
		return fmt.Errorf("chart: QueryMaxRows > 0 필수")
	}
	if c.PoolMaxConns <= 0 {
		return fmt.Errorf("chart: PoolMaxConns > 0 필수")
	}
	return nil
}

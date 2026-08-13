package futures

import (
	"flag"
	"time"
)

// Config 는 mci-edge-futures 설정.
type Config struct {
	ListenAddr string        // HTTP/WS listen (기본 :8085)
	LogLevel   string        // 로그 레벨
	DevMode    bool          // 개발 모드 (JWT 우회 등 후속)
	WsPing     time.Duration // ws keepalive ping 간격

	// 데모 소스 — 실 피드(broker) 배선 전 단독 실행/시연용 합성 틱 생성.
	Demo         bool
	DemoCodes    string        // 콤마 구분 종목코드
	DemoInterval time.Duration // 틱 생성 주기

	// OpenTelemetry (서비스 공통)
	OtelEndpoint    string
	OtelInsecure    bool
	OtelStdout      bool
	OtelSampleRatio float64
}

// DefaultConfig — 기본값.
func DefaultConfig() Config {
	return Config{
		ListenAddr:      ":8085",
		LogLevel:        "info",
		WsPing:          30 * time.Second,
		Demo:            false,
		DemoCodes:       "101V6000,105V3000",
		DemoInterval:    time.Second,
		OtelSampleRatio: 1.0,
	}
}

// LoadConfig — args 파싱.
func LoadConfig(args []string) (Config, error) {
	cfg := DefaultConfig()
	fs := flag.NewFlagSet("mci-edge-futures", flag.ContinueOnError)
	fs.StringVar(&cfg.ListenAddr, "listen", cfg.ListenAddr, "HTTP/WS listen 주소")
	fs.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "로그 레벨 (debug/info/warn/error)")
	fs.BoolVar(&cfg.DevMode, "dev", cfg.DevMode, "개발 모드")
	fs.DurationVar(&cfg.WsPing, "ws-ping", cfg.WsPing, "WebSocket ping 간격")
	fs.BoolVar(&cfg.Demo, "demo", cfg.Demo, "합성 선물 틱 생성 (실 피드 배선 전 시연용)")
	fs.StringVar(&cfg.DemoCodes, "demo-codes", cfg.DemoCodes, "데모 종목코드 (콤마 구분)")
	fs.DurationVar(&cfg.DemoInterval, "demo-interval", cfg.DemoInterval, "데모 틱 주기")
	fs.StringVar(&cfg.OtelEndpoint, "otel-endpoint", cfg.OtelEndpoint, "OTLP endpoint (비면 비활성)")
	fs.BoolVar(&cfg.OtelInsecure, "otel-insecure", cfg.OtelInsecure, "OTLP insecure")
	fs.BoolVar(&cfg.OtelStdout, "otel-stdout", cfg.OtelStdout, "OTLP stdout exporter")
	fs.Float64Var(&cfg.OtelSampleRatio, "otel-sample", cfg.OtelSampleRatio, "trace sample 비율")
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	return cfg, nil
}

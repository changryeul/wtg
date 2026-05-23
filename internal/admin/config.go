// Package admin 은 WTG 의 직원용 control plane (mci-admin).
//
// 책임 (1차 prototype):
//   - broker admin 명령 (FC_ADMIN/SubGet*) passthrough
//   - 단축 endpoint: status / clients / exchanges / whois / users
//   - 사내망 IP 화이트리스트 강제
//
// 추후 확장 (Phase 7):
//   - Routing 룰 CRUD (DB store)
//   - Policy 룰 CRUD
//   - 실시간 KPI 대시보드
//   - SSO/MFA
//   - Next.js Admin UI embed
//
// 운영 원칙:
//   - 비즈니스 권한은 매매 엔진이 검증 — 본 패키지는 admin 명령 통과만.
//   - 직원 채널 (ChannelAdmin) 강제.
//   - 사내망 외 접근 차단.
package admin

import (
	"flag"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/winwaysystems/wtg/pkg/routing"
)

// Config 는 mci-admin 런타임 설정.
type Config struct {
	// 사내망 listen 주소 (운영시 사내 IP bind 권장).
	ListenAddr string

	// MyMQ broker.
	BrokerHost string
	BrokerPort int

	// ApplName + Instance.
	ApplName string
	Instance int

	// 핸드셰이크 / I/O timeout.
	DialTimeout      time.Duration
	HandshakeTimeout time.Duration

	// HTTP timeout.
	ReadTimeout  time.Duration
	WriteTimeout time.Duration

	// MyMQ admin call timeout (모든 admin 명령에 동일 적용).
	BrokerCallTimeout time.Duration

	// IP 화이트리스트 — 사내망 CIDR 목록.
	// 비어있으면 모두 허용 (DevMode 외에는 권장 안 됨).
	AllowCIDRs []*net.IPNet

	// 인증 모드 — DevMode 면 X-WTG-User 만, 운영시 SSO 통합 (Phase 7).
	DevMode bool

	// NoBroker 가 true 면 mymqd 연결 시도 자체를 스킵한다 (UI 시각 검증 / 개발용).
	// broker 호출이 필요한 핸들러 (AdminCmd / Login 등) 는 503 응답.
	NoBroker bool

	// TrustEdgeHeaders — true 면 mci-edge-api 가 주입한 X-WTG-SID 헤더 신뢰.
	// 사내망 mTLS 환경에서만 활성화.
	TrustEdgeHeaders bool

	// 외부 HTTPS — 사내망에서도 인증서 강제 (운영자 PC ↔ admin 사이).
	// TLSClientCAFile 채워지면 mTLS — 운영자 발급 client cert 만 허용.
	TLSCertFile     string
	TLSKeyFile      string
	TLSClientCAFile string

	// etcd — 라우팅 룰 + 정책 동기화. 비면 단일 인스턴스 모드.
	EtcdEndpoints  string
	EtcdRoutesPath string
	EtcdPolicyKey  string

	// 시세 도메인 자원 etcd key — mci-price 의 watcher 와 동일한 컨벤션.
	// 비어있으면 각 default 사용 (admin_symbols/admin_pricing/admin_profiles 참조).
	EtcdSymbolsPrefix  string // default "wtg/quote/symbols/"
	EtcdPricingKey     string // default "wtg/pricing/table"
	EtcdProfilesPrefix string // default "wtg/price/profiles/"

	// UpstreamAPIURL — mci-api 의 base URL.
	// "" 이면 tx-test reverse proxy 가 비활성. WTG Control UI 의 "Tx echo" preset 이
	// 이 값을 통해 매매 transaction round-trip 을 시각적으로 검증한다.
	// DevMode 에서만 의미 있는 우회 path (운영에선 비활성 권장).
	UpstreamAPIURL string

	// DevRoutesFile — DevMode 시 라우팅 룰 시드 cfg 파일 (JSON).
	// 비면 hardcode default (TSTSVC_PING + WECHO_*) 사용. 운영에선 etcd 가
	// source of truth 라 무시. 권장 위치: ~/mymq/etc/wtg-routes.json.
	DevRoutesFile string

	// DevRoutesPolicy — cfg ↔ in-memory 동기화 정책. "additive" (기본) 또는 "sync".
	// pkg/routing.SeedPolicy 참조. sync 모드는 cfg 가 진실의 원천 → wtgctl routes
	// del/set 이 즉시 in-memory 에 반영되지만, UI 만으로 추가한 룰은 hot reload
	// 시 사라진다.
	DevRoutesPolicy string

	// SvcIncDir — 매매 svc 의 헤더 파일 디렉터리 (예: ~/mywork/win/src/inc/trn).
	// 콤마 구분 다중 디렉터리 지원. 부팅 시 일괄 파싱 → svcio.Registry → UI.
	SvcIncDir string

	// SvcCommonHeaderFile — 공통 transaction 헤더 정의 파일 (예: comhdr.h).
	// 운영 svc 의 wire frame 은 [COMHDR(256B)][TX_BODY] 구조 — 이 파일에서
	// COMHDR / BROADCAST_H / 등 모든 typedef struct 를 named header 로 등록.
	// 비면 헤더 직렬화/파싱 비활성 (모든 svc 가 raw body 로만 동작).
	SvcCommonHeaderFile string

	// Broker TLS — docs/broker-tls.md 참조.
	BrokerTLSCertFile string
	BrokerTLSKeyFile  string
	BrokerTLSCAFile   string
	BrokerTLSSNI      string

	LogLevel string
}

// DefaultConfig.
func DefaultConfig() Config {
	return Config{
		ListenAddr:        ":9090",
		BrokerHost:        "127.0.0.1",
		BrokerPort:        11217,
		ApplName:          "mci-admin",
		Instance:          0,
		DialTimeout:       5 * time.Second,
		HandshakeTimeout:  5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		BrokerCallTimeout: 5 * time.Second,
		DevMode:           false,
		LogLevel:          "info",
	}
}

// LoadConfig — flag/env (WTG_ADMIN_*).
//
// AllowCIDRs 는 환경변수 / flag 에서 콤마 구분 CIDR 리스트로 입력.
// 예: --allow-cidrs="10.0.0.0/8,172.16.0.0/12,192.168.0.0/16"
func LoadConfig(args []string) (Config, error) {
	cfg := DefaultConfig()
	var cidrStr string

	if v := os.Getenv("WTG_ADMIN_LISTEN"); v != "" {
		cfg.ListenAddr = v
	}
	if v := os.Getenv("WTG_ADMIN_BROKER_HOST"); v != "" {
		cfg.BrokerHost = v
	}
	if v := os.Getenv("WTG_ADMIN_BROKER_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.BrokerPort = n
		}
	}
	if v := os.Getenv("WTG_ADMIN_APPL"); v != "" {
		cfg.ApplName = v
	}
	if v := os.Getenv("WTG_ADMIN_DEV"); v == "1" || v == "true" {
		cfg.DevMode = true
	}
	if v := os.Getenv("WTG_ADMIN_NO_BROKER"); v == "1" || v == "true" {
		cfg.NoBroker = true
	}
	if v := os.Getenv("WTG_ADMIN_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
	if v := os.Getenv("WTG_ADMIN_ALLOW_CIDRS"); v != "" {
		cidrStr = v
	}
	if v := os.Getenv("WTG_ADMIN_TRUST_EDGE"); v == "1" || v == "true" {
		cfg.TrustEdgeHeaders = true
	}
	if v := os.Getenv("WTG_ADMIN_TLS_CERT"); v != "" {
		cfg.TLSCertFile = v
	}
	if v := os.Getenv("WTG_ADMIN_TLS_KEY"); v != "" {
		cfg.TLSKeyFile = v
	}
	if v := os.Getenv("WTG_ADMIN_TLS_CLIENT_CA"); v != "" {
		cfg.TLSClientCAFile = v
	}
	if v := os.Getenv("WTG_ADMIN_ETCD"); v != "" {
		cfg.EtcdEndpoints = v
	}
	if v := os.Getenv("WTG_ADMIN_ETCD_PREFIX"); v != "" {
		cfg.EtcdRoutesPath = v
	}
	if v := os.Getenv("WTG_ADMIN_ETCD_POLICY_KEY"); v != "" {
		cfg.EtcdPolicyKey = v
	}
	if v := os.Getenv("WTG_ADMIN_ETCD_SYMBOLS_PREFIX"); v != "" {
		cfg.EtcdSymbolsPrefix = v
	}
	if v := os.Getenv("WTG_ADMIN_ETCD_PRICING_KEY"); v != "" {
		cfg.EtcdPricingKey = v
	}
	if v := os.Getenv("WTG_ADMIN_ETCD_PROFILES_PREFIX"); v != "" {
		cfg.EtcdProfilesPrefix = v
	}
	if v := os.Getenv("WTG_ADMIN_BROKER_TLS_CERT"); v != "" {
		cfg.BrokerTLSCertFile = v
	}
	if v := os.Getenv("WTG_ADMIN_BROKER_TLS_KEY"); v != "" {
		cfg.BrokerTLSKeyFile = v
	}
	if v := os.Getenv("WTG_ADMIN_BROKER_TLS_CA"); v != "" {
		cfg.BrokerTLSCAFile = v
	}
	if v := os.Getenv("WTG_ADMIN_BROKER_TLS_SNI"); v != "" {
		cfg.BrokerTLSSNI = v
	}
	if v := os.Getenv("WTG_ADMIN_UPSTREAM_API"); v != "" {
		cfg.UpstreamAPIURL = v
	}
	if v := os.Getenv("WTG_ADMIN_DEV_ROUTES_FILE"); v != "" {
		cfg.DevRoutesFile = v
	}
	if v := os.Getenv("WTG_ADMIN_DEV_ROUTES_POLICY"); v != "" {
		cfg.DevRoutesPolicy = v
	}
	if v := os.Getenv("WTG_ADMIN_SVC_INC_DIR"); v != "" {
		cfg.SvcIncDir = v
	}
	if v := os.Getenv("WTG_ADMIN_SVC_COMHDR"); v != "" {
		cfg.SvcCommonHeaderFile = v
	}

	fs := flag.NewFlagSet("mci-admin", flag.ContinueOnError)
	fs.StringVar(&cfg.ListenAddr, "listen", cfg.ListenAddr, "사내망 listen 주소")
	fs.StringVar(&cfg.BrokerHost, "broker-host", cfg.BrokerHost, "mymqd 호스트")
	fs.IntVar(&cfg.BrokerPort, "broker-port", cfg.BrokerPort, "mymqd 포트")
	fs.StringVar(&cfg.ApplName, "appl", cfg.ApplName, "ApplName")
	fs.IntVar(&cfg.Instance, "instance", cfg.Instance, "인스턴스 일련번호")
	fs.BoolVar(&cfg.DevMode, "dev", cfg.DevMode, "개발 모드 — JWT 우회")
	fs.BoolVar(&cfg.NoBroker, "no-broker", cfg.NoBroker, "broker(mymqd) 연결 스킵 — UI 시각 검증용")
	fs.StringVar(&cidrStr, "allow-cidrs", cidrStr, "사내망 CIDR (콤마 구분, 비어있으면 모두 허용)")
	fs.DurationVar(&cfg.BrokerCallTimeout, "call-timeout", cfg.BrokerCallTimeout, "broker 호출 timeout")
	fs.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "로그 레벨")
	fs.BoolVar(&cfg.TrustEdgeHeaders, "trust-edge", cfg.TrustEdgeHeaders, "X-WTG-SID 헤더 신뢰 (mci-edge-api 뒤에서만)")
	fs.StringVar(&cfg.TLSCertFile, "tls-cert", cfg.TLSCertFile, "TLS 서버 cert PEM (있으면 HTTPS)")
	fs.StringVar(&cfg.TLSKeyFile, "tls-key", cfg.TLSKeyFile, "TLS 서버 key PEM")
	fs.StringVar(&cfg.TLSClientCAFile, "tls-client-ca", cfg.TLSClientCAFile, "운영자 mTLS 클라이언트 CA bundle")
	fs.StringVar(&cfg.EtcdEndpoints, "etcd", cfg.EtcdEndpoints, "라우팅 룰 etcd endpoints (콤마 구분, 비면 in-memory)")
	fs.StringVar(&cfg.EtcdRoutesPath, "etcd-prefix", cfg.EtcdRoutesPath, "etcd 키 prefix (default wtg/routes/)")
	fs.StringVar(&cfg.EtcdPolicyKey, "etcd-policy-key", cfg.EtcdPolicyKey, "etcd 정책 단일 key (default wtg/policy)")
	fs.StringVar(&cfg.EtcdSymbolsPrefix, "etcd-symbols-prefix", cfg.EtcdSymbolsPrefix, "etcd 통화쌍 prefix (default wtg/quote/symbols/)")
	fs.StringVar(&cfg.EtcdPricingKey, "etcd-pricing-key", cfg.EtcdPricingKey, "etcd PricingTable 단일 key (default wtg/pricing/table)")
	fs.StringVar(&cfg.EtcdProfilesPrefix, "etcd-profiles-prefix", cfg.EtcdProfilesPrefix, "etcd 활성 Profile prefix (default wtg/price/profiles/)")
	fs.StringVar(&cfg.BrokerTLSCertFile, "broker-tls-cert", cfg.BrokerTLSCertFile, "broker TLS 클라이언트 cert PEM")
	fs.StringVar(&cfg.BrokerTLSKeyFile, "broker-tls-key", cfg.BrokerTLSKeyFile, "broker TLS 클라이언트 key PEM")
	fs.StringVar(&cfg.BrokerTLSCAFile, "broker-tls-ca", cfg.BrokerTLSCAFile, "broker TLS 서버 검증용 CA bundle")
	fs.StringVar(&cfg.BrokerTLSSNI, "broker-tls-sni", cfg.BrokerTLSSNI, "broker TLS SNI / hostname")
	fs.StringVar(&cfg.UpstreamAPIURL, "upstream-api", cfg.UpstreamAPIURL, "mci-api base URL — Tx 테스터용 reverse proxy. 예: http://127.0.0.1:8080. 비면 비활성")
	fs.StringVar(&cfg.DevRoutesFile, "dev-routes-file", cfg.DevRoutesFile, "DevMode 라우팅 룰 시드 JSON 경로 (예: ~/mymq/etc/wtg-routes.json). 비면 hardcode default")
	fs.StringVar(&cfg.DevRoutesPolicy, "dev-routes-policy", cfg.DevRoutesPolicy, "cfg ↔ in-memory 동기화 정책. additive(default) | sync. sync 는 cfg 가 진실의 원천 (cfg 삭제 alias 가 in-memory 에서도 제거)")
	fs.StringVar(&cfg.SvcIncDir, "svc-inc-dir", cfg.SvcIncDir, "매매 svc 헤더 디렉터리 (콤마 구분 다중 path). 부팅 시 일괄 파싱 → /v1/admin/svc-io 노출")
	fs.StringVar(&cfg.SvcCommonHeaderFile, "svc-common-header", cfg.SvcCommonHeaderFile, "공통 transaction 헤더 파일 (예: ~/mywork/win/src/inc/com/comhdr.h). 운영 svc 의 wire 가 [COMHDR][TX_BODY] 구조라 헤더 직렬화/파싱에 사용")

	if err := fs.Parse(args); err != nil {
		return cfg, err
	}

	if cidrStr != "" {
		nets, err := parseCIDRs(cidrStr)
		if err != nil {
			return cfg, err
		}
		cfg.AllowCIDRs = nets
	}
	if _, err := routing.ParseSeedPolicy(cfg.DevRoutesPolicy); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// parseCIDRs 는 콤마 구분 문자열을 *net.IPNet 슬라이스로 변환.
func parseCIDRs(s string) ([]*net.IPNet, error) {
	parts := strings.Split(s, ",")
	out := make([]*net.IPNet, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		_, n, err := net.ParseCIDR(p)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

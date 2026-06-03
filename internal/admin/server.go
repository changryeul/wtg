package admin

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	apihandlers "github.com/winwaysystems/wtg/internal/api/handlers"
	"github.com/winwaysystems/wtg/internal/api/middleware"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/server/v3/embed"

	"github.com/winwaysystems/wtg/pkg/auth"
	"github.com/winwaysystems/wtg/pkg/metrics"
	"github.com/winwaysystems/wtg/pkg/mymq"
	"github.com/winwaysystems/wtg/pkg/policy"
	"github.com/winwaysystems/wtg/pkg/routing"
	"github.com/winwaysystems/wtg/pkg/svcio"
	"github.com/winwaysystems/wtg/pkg/tlsutil"
)

// Server 는 mci-admin 의 HTTP 서버 + broker 클라이언트.
type Server struct {
	cfg         Config
	logger      *slog.Logger
	mq          *mymq.Client
	metrics     *metrics.Registry
	sessions    auth.Store
	refresh     auth.RefreshStore
	jwtIss      *auth.Issuer
	jwtVer      *auth.Verifier
	routes      routing.Registry
	policy      *policy.Engine
	policySync  *policy.EtcdSync
	svcio       *svcio.Registry
	audit       *AuditRing
	auditRedis  *redis.Client // audit ring 영속 backend (옵션)
	hub         *Hub
	tlsReloader *tlsutil.Reloader
	http        *http.Server

	// 신규 자원 (pricing/profiles) 용 공유 etcd 클라이언트.
	// EtcdEndpoints 가 비어있으면 nil — 핸들러는 503 반환.
	etcdShared *clientv3.Client

	// devEtcd — DevMode 자동 기동 embedded etcd. cfg.EtcdEndpoints 가 비고
	// DevMode 일 때만 채워짐. Shutdown 시 close.
	devEtcd *embed.Etcd

	// TimescaleDB pgx pool — cfg.ChartDSN 활성 시. 마진 재계산 endpoint 가 사용.
	chartPool *pgxpool.Pool
}

// SetPolicyEngine — 외부에서 정책 엔진 주입 (mci-api 와 공유 가능).
// 호출하지 않으면 자동 생성 (단일 인스턴스).
func (s *Server) SetPolicyEngine(e *policy.Engine) { s.policy = e }

// buildEtcdTLS 는 cfg.EtcdTLS* 필드를 *tls.Config 로 변환한다.
// 모두 비어있으면 nil 반환 (평문 etcd). routing/policy/공유 client 모두 동일 인증서.
func (s *Server) buildEtcdTLS() (*tls.Config, error) {
	if s.cfg.EtcdTLSCertFile == "" && s.cfg.EtcdTLSKeyFile == "" && s.cfg.EtcdTLSCAFile == "" {
		return nil, nil
	}
	return tlsutil.LoadClient(tlsutil.ClientOptions{
		CertFile:     s.cfg.EtcdTLSCertFile,
		KeyFile:      s.cfg.EtcdTLSKeyFile,
		ServerCAFile: s.cfg.EtcdTLSCAFile,
		ServerName:   s.cfg.EtcdTLSServerName,
	})
}

// SetRoutingRegistry 는 외부에서 라우팅 저장소를 주입한다 (테스트 / 운영 시
// etcd-backed 구현 차환). NewServer 직후 / Start 전에 호출.
//
// 호출하지 않으면 Start 시점에 InMemoryRegistry 가 자동 생성된다.
func (s *Server) SetRoutingRegistry(reg routing.Registry) {
	s.routes = reg
}

// SetJWT — mci-api 와 동일한 의미. nil 인자는 무시.
func (s *Server) SetJWT(iss *auth.Issuer, ver *auth.Verifier) {
	if iss != nil {
		s.jwtIss = iss
	}
	if ver != nil {
		s.jwtVer = ver
	}
}

// SetRefreshStore — mci-api 와 동일.
func (s *Server) SetRefreshStore(store auth.RefreshStore) {
	s.refresh = store
}

// NewServer.
func NewServer(cfg Config, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		cfg:     cfg,
		logger:  logger,
		metrics: metrics.NewRegistry(),
	}
}

// Start — broker 연결 + HTTP listen (블로킹).
func (s *Server) Start(ctx context.Context) error {
	// DevMode + EtcdEndpoints 미설정 → embedded etcd 자동 기동.
	// 결과: routes/policy/pricing/profiles/user-profiles/quoteid-engines
	// 가 코드 변경 없이 동일 etcd 를 통해 동작. 재시작 후에도 영속 (stable data dir).
	if s.cfg.DevMode && strings.TrimSpace(s.cfg.EtcdEndpoints) == "" && s.devEtcd == nil {
		srv, clientURL, err := startDevEmbeddedEtcd(ctx, "", s.logger)
		if err != nil {
			return fmt.Errorf("dev embedded etcd: %w", err)
		}
		s.devEtcd = srv
		s.cfg.EtcdEndpoints = clientURL
	}

	// broker 호출용 Caller — NoBroker 면 stub, 아니면 실제 mymq.Client.
	var mqCaller anyCaller
	if s.cfg.NoBroker {
		s.logger.Warn("--no-broker 모드 — mymqd 연결 스킵, broker 호출 핸들러는 503 반환")
		mqCaller = unavailableCaller{}
	} else {
		brokerTLS, err := loadBrokerTLS(&s.cfg)
		if err != nil {
			return fmt.Errorf("broker TLS 구성: %w", err)
		}
		mq, err := mymq.Open(ctx, s.cfg.BrokerHost, s.cfg.BrokerPort, mymq.Options{
			ApplName:         s.cfg.ApplName,
			Instance:         s.cfg.Instance,
			Channel:          mymq.ChannelAdmin,
			DialTimeout:      s.cfg.DialTimeout,
			HandshakeTimeout: s.cfg.HandshakeTimeout,
			Logger:           s.logger,
			TLS:              brokerTLS,
			Reconnect: &mymq.ReconnectOptions{
				InitialBackoff: 1 * time.Second,
				MaxBackoff:     30 * time.Second,
				BackoffFactor:  2.0,
			},
			Metrics: mymq.MetricsHook{
				OnDisconnect:       func(_ error) { s.metrics.IncBrokerDisconnect("mci-admin") },
				OnReconnect:        func(_ int, d time.Duration) { s.metrics.IncBrokerReconnect("mci-admin", d) },
				OnInflightAborted:  func(n int) { s.metrics.IncBrokerInflightAborted("mci-admin", n) },
				OnHeartbeatTimeout: func() { s.metrics.IncBrokerHeartbeatTimeout("mci-admin") },
			},
		})
		if err != nil {
			return fmt.Errorf("mymq.Open: %w", err)
		}
		s.mq = mq
		mqCaller = mq
	}

	// 세션 저장소 — DevMode 가 아니면 in-memory (auth.md §7, Phase 3 Redis 전).
	if !s.cfg.DevMode {
		s.sessions = auth.NewMemoryStore(auth.MemoryStoreOptions{})
		if s.refresh == nil {
			s.refresh = auth.NewMemoryRefreshStore(auth.MemoryRefreshStoreOptions{})
		}
	}
	// etcd TLS 한 번만 빌드 — routing/policy/공유 client 모두 동일 인증서 사용.
	etcdTLS, err := s.buildEtcdTLS()
	if err != nil {
		return fmt.Errorf("admin etcd TLS: %w", err)
	}

	// 라우팅 룰 저장소 — etcd endpoint 가 있으면 EtcdRegistry, 없으면 in-memory.
	// mci-api 인스턴스들과 룰 동기화는 etcd watch 가 책임.
	if s.routes == nil {
		reg, err := routing.New(ctx, routing.FactoryOptions{
			Endpoints: s.cfg.EtcdEndpoints,
			Prefix:    s.cfg.EtcdRoutesPath,
			TLS:       etcdTLS,
			Logger:    s.logger,
		})
		if err != nil {
			return fmt.Errorf("routing registry: %w", err)
		}
		s.routes = reg
	}
	// DevMode 시드 — cfg 파일(s.cfg.DevRoutesFile) 우선, 없으면 hardcode default.
	// 운영에선 etcd 가 source of truth 이므로 시드 안 함.
	// 정책 (additive | sync) 은 routing.SeedPolicy 주석 참조. config 단계에서 검증됨.
	if s.cfg.DevMode {
		policy, _ := routing.ParseSeedPolicy(s.cfg.DevRoutesPolicy)
		routing.SeedDevRoutesExPolicy(s.routes, s.logger, s.cfg.DevRoutesFile, policy)
		// hot reload — cfg 파일 mtime polling. 파일 변경 시 재시드.
		routing.WatchRoutesFilePolicy(ctx, s.routes, s.logger, s.cfg.DevRoutesFile, 2*time.Second, policy)
		// File-backed wrap — UI 에서 등록한 alias 가 file 에 자동 write-back.
		// Seed 끝난 후 wrap → 시드 자체는 file write 노이즈 없음.
		// DevRoutesFile 빈 값이면 wrap 은 no-op (그대로 in-memory only).
		s.routes = routing.WrapWithFileWriteback(s.routes, s.cfg.DevRoutesFile, s.logger)
	}
	// Audit ring buffer — 최근 admin 액션 200개 (UI 표시용).
	if s.audit == nil {
		s.audit = NewAuditRing(200)
	}
	// Audit Redis backend — 영속 저장. 재시작 시에도 보존.
	if s.cfg.AuditRedisAddr != "" {
		s.auditRedis = redis.NewClient(&redis.Options{
			Addr:     s.cfg.AuditRedisAddr,
			Password: s.cfg.AuditRedisPassword,
			DB:       s.cfg.AuditRedisDB,
		})
		s.audit.SetRedisBackend(s.auditRedis, s.cfg.AuditRedisKey, s.cfg.AuditRedisMaxLen, s.logger)
		s.audit.SetRedisFailCallback(func() { s.metrics.IncAuditRedisFail("mci-admin") })
		s.logger.Info("audit Redis backend 활성",
			slog.String("addr", s.cfg.AuditRedisAddr),
			slog.String("key", s.cfg.AuditRedisKey))
	}
	// ws stream hub — 브라우저 UI 실시간 푸시.
	if s.hub == nil {
		s.hub = NewHub(s.logger)
	}
	// audit push 가 hub 도 publish 하도록 연결.
	s.audit.SetOnPush(func(e AuditEntry) { s.hub.Broadcast("audit", e) })
	// 정책 엔진.
	if s.policy == nil {
		s.policy = policy.NewEngine(nil)
	}
	// 정책 변경 → ws stream 에도 publish (etcd sync 가 별도 callback 으로 등록되어
	// 함께 동작 — Engine 이 다중 콜백 지원).
	hub := s.hub
	s.policy.AddOnChange(func(st policy.State) { hub.Broadcast("policy", st) })
	// etcd 동기화 — endpoints 가 있으면 sync 시작 (mci-api 인스턴스들과 공유).
	if eps := policy.SplitEndpoints(s.cfg.EtcdEndpoints); len(eps) > 0 && s.policySync == nil {
		ps, err := policy.StartEtcdSync(ctx, s.policy, policy.EtcdSyncOptions{
			Endpoints: eps,
			Key:       s.cfg.EtcdPolicyKey,
			TLS:       etcdTLS,
			Logger:    s.logger,
		})
		if err != nil {
			return fmt.Errorf("policy etcd sync: %w", err)
		}
		s.policySync = ps
	}

	deps := &HandlerDeps{
		MQ:          mqCaller,
		CallTimeout: s.cfg.BrokerCallTimeout,
		Logger:      s.logger,
	}
	// login/logout 은 api/handlers 의 핸들러를 재사용 — channel 은 ADMIN 디폴트.
	loginDeps := &apihandlers.Deps{
		MQ:             mqCaller,
		CallTimeout:    s.cfg.BrokerCallTimeout,
		Logger:         s.logger,
		Sessions:       s.sessions,
		DefaultChannel: "ADMIN",
		JWTIssuer:      s.jwtIss,
		RefreshStore:   s.refresh,
	}

	routeDeps := &RoutingDeps{
		Registry: s.routes,
		Logger:   s.logger,
		Audit:    s.audit,
		Hub:      s.hub,
	}
	policyDeps := &PolicyDeps{
		Engine: s.policy,
		Logger: s.logger,
		Audit:  s.audit,
		Hub:    s.hub,
	}

	// 시세 도메인 자원 (pricing/profiles) — etcd 직접 KV.
	// EtcdEndpoints 가 비어있으면 dial 안 함 (핸들러는 503).
	var pricingDeps *PricingDeps
	var profilesDeps *ProfilesDeps
	if eps := policy.SplitEndpoints(s.cfg.EtcdEndpoints); len(eps) > 0 && s.etcdShared == nil {
		clientCfg := clientv3.Config{
			Endpoints:   eps,
			DialTimeout: 5 * time.Second,
			TLS:         etcdTLS, // 위에서 1회 빌드, routing/policy/공유 client 동일 사용.
		}
		if etcdTLS != nil {
			s.logger.Info("admin etcd TLS 활성 (routing/policy/shared 동일)",
				slog.Bool("mtls", s.cfg.EtcdTLSCertFile != ""),
				slog.String("sni", s.cfg.EtcdTLSServerName),
			)
		}
		cli, err := clientv3.New(clientCfg)
		if err != nil {
			return fmt.Errorf("admin shared etcd dial: %w", err)
		}
		s.etcdShared = cli
	}
	// TimescaleDB pool — 마진 재계산 endpoint 활성 시.
	if s.cfg.ChartDSN != "" && s.chartPool == nil {
		poolCfg, perr := pgxpool.ParseConfig(s.cfg.ChartDSN)
		if perr != nil {
			return fmt.Errorf("admin chart DSN 파싱: %w", perr)
		}
		max := s.cfg.ChartPoolMaxConns
		if max <= 0 {
			max = 5
		}
		poolCfg.MaxConns = int32(max)
		pool, perr := pgxpool.NewWithConfig(ctx, poolCfg)
		if perr != nil {
			return fmt.Errorf("admin chart pool: %w", perr)
		}
		s.chartPool = pool
		s.logger.Info("admin TimescaleDB pool 활성 (마진 재계산)",
			slog.Int("max_conns", max))
	}
	pricingDeps = &PricingDeps{
		Cli:    s.etcdShared,
		Key:    s.cfg.EtcdPricingKey,
		Logger: s.logger,
		Audit:  s.audit,
		Hub:    s.hub,
	}
	profilesDeps = &ProfilesDeps{
		Cli:    s.etcdShared,
		Prefix: s.cfg.EtcdProfilesPrefix,
		Logger: s.logger,
		Audit:  s.audit,
		Hub:    s.hub,
	}
	userProfilesDeps := &UserProfilesDeps{
		Cli:    s.etcdShared,
		Prefix: s.cfg.EtcdUserProfilesPrefix,
		Logger: s.logger,
		Audit:  s.audit,
		Hub:    s.hub,
	}
	quoteIDEnginesDeps := &QuoteIDEnginesDeps{
		Cli:    s.etcdShared,
		Prefix: s.cfg.EtcdQuoteIDEnginesPrefix,
		Logger: s.logger,
		Audit:  s.audit,
		Hub:    s.hub,
	}
	rateLimitDeps := &RateLimitDeps{
		Cli:    s.etcdShared,
		Prefix: s.cfg.EtcdRateLimitPrefix,
		Logger: s.logger,
		Audit:  s.audit,
		Hub:    s.hub,
	}
	marginDeps := &MarginRecomputeDeps{
		Cli:     s.etcdShared,
		Pool:    s.chartPool,
		EtcdKey: s.cfg.EtcdPricingKey,
		Logger:  s.logger,
		Audit:   s.audit,
	}

	// svc I/O 명세 — 부팅 시 헤더 디렉터리 일괄 인덱싱. cfg 가 비어있으면 빈
	// registry — UI 가 안내 메시지만 표시.
	if s.svcio == nil {
		s.svcio = svcio.NewRegistry()
	}
	// 1. 공통 헤더 (comhdr.h) 먼저 로드 — spec 들이 HeaderFields inline 으로
	//    가져갈 수 있도록 svc 헤더 LoadDir 보다 *먼저* 등록.
	if s.cfg.SvcCommonHeaderFile != "" {
		if err := s.svcio.LoadHeaderFile(s.cfg.SvcCommonHeaderFile, s.logger); err != nil {
			s.logger.Warn("svcio 공통 헤더 로드 실패",
				slog.String("path", s.cfg.SvcCommonHeaderFile), slog.Any("err", err))
		}
	}
	// 2. 디렉터리별 default 헤더 — 운영 (`win/src/inc/trn`) 은 COMHDR,
	//    dev (`svc-headers`) 는 헤더 없음. svc 별 override 는 spec 의
	//    `@wtg-header: NAME` 주석으로.
	s.svcio.SetDirHeaderDefault("win/src/inc/trn", "COMHDR")
	// 3. svc 헤더 디렉터리 일괄 인덱싱.
	if s.cfg.SvcIncDir != "" {
		if _, _, err := s.svcio.LoadDirs(s.cfg.SvcIncDir, s.logger); err != nil {
			s.logger.Warn("svcio 헤더 디렉터리 인덱싱 실패",
				slog.String("dirs", s.cfg.SvcIncDir), slog.Any("err", err))
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/ping", PingHandler())
	mux.HandleFunc("POST /v1/login", apihandlers.Login(loginDeps))
	mux.HandleFunc("POST /v1/logout", apihandlers.Logout(loginDeps))
	mux.HandleFunc("POST /v1/refresh", apihandlers.Refresh(loginDeps))
	mux.HandleFunc("POST /v1/admin/cmd", AdminCmd(deps))
	mux.HandleFunc("GET /v1/admin/status", GetStatus(deps))
	mux.HandleFunc("GET /v1/admin/clients", GetClients(deps))
	mux.HandleFunc("GET /v1/admin/exchanges", GetExchanges(deps))
	mux.HandleFunc("GET /v1/admin/users", GetUsers(deps))
	mux.HandleFunc("GET /v1/admin/whois", GetWhois(deps))
	// 라우팅 룰 CRUD — alias → exchange/routing_key 매핑 동적 관리.
	mux.HandleFunc("GET /v1/admin/routes", ListRoutes(routeDeps))
	mux.HandleFunc("GET /v1/admin/routes/{alias}", GetRoute(routeDeps))
	mux.HandleFunc("PUT /v1/admin/routes/{alias}", PutRoute(routeDeps))
	mux.HandleFunc("DELETE /v1/admin/routes/{alias}", DeleteRoute(routeDeps))
	mux.HandleFunc("POST /v1/admin/routes/{alias}/active", SetRouteActive(routeDeps))
	mux.HandleFunc("GET /v1/admin/audit", AuditList(routeDeps))

	// 시세 도메인 — etcd 직접 KV CRUD (mci-price 가 watch 로 즉시 반영).
	// SymbolMap 은 PairMaster derived view 이므로 별도 CRUD 없음 (fx-sync 가 SoT).
	mux.HandleFunc("GET /v1/admin/pricing/table", GetPricingTable(pricingDeps))
	mux.HandleFunc("PUT /v1/admin/pricing/table", PutPricingTable(pricingDeps))
	mux.HandleFunc("POST /v1/admin/pricing/preview", PreviewPricing(pricingDeps))
	mux.HandleFunc("POST /v1/admin/pricing/preview-matrix", PreviewPricingMatrix(pricingDeps))

	mux.HandleFunc("GET /v1/admin/profiles", ListProfiles(profilesDeps))
	mux.HandleFunc("GET /v1/admin/profiles/{key}", GetProfile(profilesDeps))
	mux.HandleFunc("PUT /v1/admin/profiles/{key}", PutProfile(profilesDeps))
	mux.HandleFunc("DELETE /v1/admin/profiles/{key}", DeleteProfile(profilesDeps))

	// 사용자 프로파일 (Site/Tier 권위 출처) — mci-api 의 Login 이 watch 로 받아 사용.
	mux.HandleFunc("GET /v1/admin/user-profiles", ListUserProfiles(userProfilesDeps))
	mux.HandleFunc("GET /v1/admin/user-profiles/{usid}", GetUserProfile(userProfilesDeps))
	mux.HandleFunc("PUT /v1/admin/user-profiles/{usid}", PutUserProfile(userProfilesDeps))
	mux.HandleFunc("DELETE /v1/admin/user-profiles/{usid}", DeleteUserProfile(userProfilesDeps))

	mux.HandleFunc("GET /v1/admin/quoteid-engines", ListQuoteIDEngines(quoteIDEnginesDeps))
	mux.HandleFunc("GET /v1/admin/quoteid-engines/{engine_id}", GetQuoteIDEngine(quoteIDEnginesDeps))
	mux.HandleFunc("PUT /v1/admin/quoteid-engines/{engine_id}", PutQuoteIDEngine(quoteIDEnginesDeps))
	mux.HandleFunc("DELETE /v1/admin/quoteid-engines/{engine_id}", DeleteQuoteIDEngine(quoteIDEnginesDeps))

	// Rate limit 정책 — service 별 PolicyDoc (mci-edge-* 가 watch 로 hot-swap).
	mux.HandleFunc("GET /v1/admin/ratelimit", ListRateLimitPolicies(rateLimitDeps))
	mux.HandleFunc("GET /v1/admin/ratelimit/{service}", GetRateLimitPolicy(rateLimitDeps))
	mux.HandleFunc("PUT /v1/admin/ratelimit/{service}", PutRateLimitPolicy(rateLimitDeps))
	mux.HandleFunc("DELETE /v1/admin/ratelimit/{service}", DeleteRateLimitPolicy(rateLimitDeps))

	// Prometheus query proxy — 운영 모니터링 페이지가 사용.
	promDeps := &PromProxyDeps{
		BaseURL: s.cfg.PromURL,
		Logger:  s.logger,
	}
	mux.HandleFunc("GET /v1/admin/prom-query", PromQuery(promDeps))

	// Grafana alert state proxy — 운영 모니터링 페이지의 alert 섹션.
	grafanaDeps := &GrafanaProxyDeps{
		BaseURL:  s.cfg.GrafanaURL,
		Username: s.cfg.GrafanaUser,
		Password: s.cfg.GrafanaPass,
		Logger:   s.logger,
	}
	mux.HandleFunc("GET /v1/admin/grafana-alerts", GrafanaAlerts(grafanaDeps))
	mux.HandleFunc("GET /v1/admin/grafana-config", GrafanaConfig(grafanaDeps))

	mux.HandleFunc("POST /v1/admin/margin/recompute", PostMarginRecompute(marginDeps))
	// 정책 엔진 — kill switch / 정비 창 / 차단 심볼·라우팅키.
	mux.HandleFunc("GET /v1/admin/policy", GetPolicy(policyDeps))
	mux.HandleFunc("POST /v1/admin/policy/kill-switch", SetKillSwitch(policyDeps))
	mux.HandleFunc("POST /v1/admin/policy/maintenance", SetMaintenance(policyDeps))
	mux.HandleFunc("POST /v1/admin/policy/blocked-symbols", SetBlockedSymbols(policyDeps))
	mux.HandleFunc("POST /v1/admin/policy/blocked-routing-keys", SetBlockedRoutingKeys(policyDeps))
	// 시세 통계 proxy — admin UI 의 "시세 통계" 페이지가 same-origin 으로 mci-price 호출.
	mux.HandleFunc("GET /v1/admin/price/{kind}", PriceStatsProxy(s.cfg.PriceURL))
	// svc I/O 명세 — 매매 svc 의 input/output 구조 (헤더 파싱 결과) 노출.
	mux.HandleFunc("GET /v1/admin/svc-io", ListSvcIO(s.svcio))
	// 공통 헤더 (COMHDR/BROADCAST_H/...) 등록 list.
	mux.HandleFunc("GET /v1/admin/svc-io/headers", ListSvcIOHeaders(s.svcio))
	mux.HandleFunc("GET /v1/admin/svc-io/{code}", GetSvcIO(s.svcio))
	// 전문 (wire frame) 테스트 — JSON input → wire 직렬화 → broker → byte 응답 parse.
	// mci 의 정책/감사 layer 통과. legacy native client 가 보낼 wire 와 동일.
	wireDeps := &TestWireDeps{
		Registry:    s.svcio,
		Routes:      s.routes,
		MQ:          mqCaller,
		Policy:      s.policy,
		Audit:       s.audit,
		Hub:         s.hub,
		CallTimeout: s.cfg.BrokerCallTimeout,
		Logger:      s.logger,
	}
	mux.HandleFunc("POST /v1/admin/svc-io/{code}/test-wire", TestWireSvc(wireDeps))
	// 헤더 source 편집 — dev 디렉터리 (svc-headers) 만 편집 허용.
	editDeps := &EditDeps{Registry: s.svcio, Logger: s.logger, Audit: s.audit}
	mux.HandleFunc("GET /v1/admin/svc-io/{code}/source", GetSvcIOSource(editDeps))
	mux.HandleFunc("PUT /v1/admin/svc-io/{code}/source", SaveSvcIOSource(editDeps))
	// Tx 테스터 — UI 안에서 mci-api 의 /v1/tx 로 reverse proxy.
	// UpstreamAPIURL 이 비어있으면 503. DevMode 검증용 우회 path.
	mux.HandleFunc("POST /v1/admin/tx-test", TxTestProxy(s.cfg.UpstreamAPIURL, s.logger))
	// alias × tier 통계 — mci-api 의 /v1/admin/alias-stats 로 reverse proxy.
	// UI 의 운영 대시보드에서 alias 별 calls/latency/error_rate × tier 분리 관찰.
	mux.HandleFunc("GET /v1/admin/alias-stats", UpstreamProxy(s.cfg.UpstreamAPIURL, "/v1/admin/alias-stats", s.logger))
	// Push 테스터 — mci-admin 의 broker connection 으로 user-targeted unsolicited
	// 메시지(FC_PUSH/SubPush) 를 발사한다. mci-push 띄워둔 상태에서 ws 로 흘러오는지
	// 시각 검증용. NoBroker 모드면 503.
	mux.HandleFunc("POST /v1/admin/push-test", PushTestHandler(s.mq, s.logger))
	// 실시간 이벤트 ws 채널.
	mux.HandleFunc("GET /v1/admin/stream", StreamHandler(s.hub, s.logger))
	mux.Handle("GET /metrics", s.metrics.Handler())

	authMW := middleware.Auth(middleware.AuthConfig{
		DevMode:          s.cfg.DevMode,
		SessionStore:     s.sessions,
		JWTVerifier:      s.jwtVer,
		TrustEdgeHeaders: s.cfg.TrustEdgeHeaders,
		Logger:           s.logger,
	})
	ipMW := IPAllowList(s.cfg.AllowCIDRs, s.logger)
	apiChain := middleware.Chain(
		mux,
		authMW,
		// ws stream 클라이언트 호환 — query 키를 헤더로 변환.
		// access_token (JWT) → Authorization, x_wtg_user (DevMode) → X-WTG-User.
		// 브라우저 WebSocket API 가 사용자 정의 헤더 주입을 지원하지 않기 때문.
		middleware.BearerFromQuery(),
		middleware.UserFromQuery(),
		metrics.HTTPMiddleware(s.metrics, "mci-admin"),
		ipMW, // ipMW 는 인증 전에 즉시 차단
		middleware.AccessLog(s.logger),
		middleware.RequestID(),
		middleware.Recover(s.logger),
	)

	// 최상위 mux — UI 정적 파일은 인증 우회, 그 외는 apiChain (인증).
	// 분리 이유: SPA 의 로그인 화면은 인증되지 않은 사용자도 받아야 함.
	root := http.NewServeMux()
	uiHandler := UIHandler()
	root.Handle("/", uiHandler)
	root.Handle("/v1/", apiChain)
	root.Handle("/metrics", apiChain)
	root.Handle("/healthz", apiChain)
	root.Handle("/readyz", apiChain)
	chain := root

	s.http = &http.Server{
		Addr:         s.cfg.ListenAddr,
		Handler:      chain,
		ReadTimeout:  s.cfg.ReadTimeout,
		WriteTimeout: s.cfg.WriteTimeout,
		BaseContext:  func(_ net.Listener) context.Context { return ctx },
	}

	tlsEnabled := s.cfg.TLSCertFile != "" && s.cfg.TLSKeyFile != ""
	if tlsEnabled {
		rl, err := tlsutil.NewReloader(tlsutil.ReloaderOptions{
			CertFile:     s.cfg.TLSCertFile,
			KeyFile:      s.cfg.TLSKeyFile,
			ClientCAFile: s.cfg.TLSClientCAFile,
			Logger:       s.logger,
		})
		if err != nil {
			return fmt.Errorf("TLS 구성: %w", err)
		}
		rl.WatchSIGHUP()
		rl.WatchFile(30 * time.Second)
		s.tlsReloader = rl
		s.http.TLSConfig = rl.ServerConfig()
	}

	s.logger.Info("HTTP listen 시작",
		slog.String("addr", s.cfg.ListenAddr),
		slog.String("broker", fmt.Sprintf("%s:%d", s.cfg.BrokerHost, s.cfg.BrokerPort)),
		slog.Bool("dev_mode", s.cfg.DevMode),
		slog.Int("allow_cidrs", len(s.cfg.AllowCIDRs)),
		slog.Bool("tls", tlsEnabled),
		slog.Bool("mtls", tlsEnabled && s.cfg.TLSClientCAFile != ""),
	)

	errCh := make(chan error, 1)
	go func() {
		var err error
		if tlsEnabled {
			err = s.http.ListenAndServeTLS("", "")
		} else {
			err = s.http.ListenAndServe()
		}
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return s.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

// Shutdown — 그레이스풀.
func (s *Server) Shutdown(ctx context.Context) error {
	var first error
	if s.http != nil {
		if err := s.http.Shutdown(ctx); err != nil {
			first = err
		}
	}
	if s.mq != nil {
		if err := s.mq.Close(); err != nil && first == nil {
			first = err
		}
	}
	if s.sessions != nil {
		if err := s.sessions.Close(); err != nil && first == nil {
			first = err
		}
	}
	if s.refresh != nil {
		if err := s.refresh.Close(); err != nil && first == nil {
			first = err
		}
	}
	if closer, ok := s.routes.(interface{ Close() error }); ok {
		if err := closer.Close(); err != nil && first == nil {
			first = err
		}
	}
	if s.policySync != nil {
		if err := s.policySync.Close(); err != nil && first == nil {
			first = err
		}
	}
	if s.etcdShared != nil {
		if err := s.etcdShared.Close(); err != nil && first == nil {
			first = err
		}
	}
	// devEtcd 는 etcdShared / policySync 의 client 가 모두 닫힌 뒤에 종료해야
	// "use of closed connection" 류 노이즈 없음.
	if s.devEtcd != nil {
		s.devEtcd.Close()
	}
	if s.chartPool != nil {
		s.chartPool.Close()
	}
	if s.auditRedis != nil {
		_ = s.auditRedis.Close()
	}
	if s.hub != nil {
		s.hub.Close()
	}
	if s.tlsReloader != nil {
		s.tlsReloader.Stop()
	}
	return first
}

// loadBrokerTLS — Config 의 broker TLS 옵션이 있으면 *tls.Config, 아니면 nil.
func loadBrokerTLS(cfg *Config) (*tls.Config, error) {
	if cfg.BrokerTLSCertFile == "" && cfg.BrokerTLSCAFile == "" {
		return nil, nil
	}
	return tlsutil.LoadClient(tlsutil.ClientOptions{
		CertFile:     cfg.BrokerTLSCertFile,
		KeyFile:      cfg.BrokerTLSKeyFile,
		ServerCAFile: cfg.BrokerTLSCAFile,
		ServerName:   cfg.BrokerTLSSNI,
	})
}

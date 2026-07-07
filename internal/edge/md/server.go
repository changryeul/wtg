// Package md — mci-edge-md 의 FIX 4.4 시세 gateway (DMZ).
//
// Phase A (skeleton) — Logon + MarketDataRequest(35=V) → 하드코딩 quote 로
// MarketDataSnapshotFullRefresh(35=W) 응답. 정적 counterparty seed 만. 증분
// (35=X) / MDR reject (35=Y) / gRPC upstream 은 Phase B~C 에서 확장.
//
// 자세한 설계: docs/fix-gateway-design.md (line 370 참조).
package md

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/quickfixgo/quickfix"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// Server — mci-edge-md lifecycle. quickfix.Acceptor wrap.
type Server struct {
	cfg       Config
	logger    *slog.Logger
	app       *fixApp
	acceptor  *quickfix.Acceptor
	cpWatcher *EtcdCounterpartyWatcher
	cpEtcdCli *clientv3.Client
	upstream  *GrpcQuoteSource // nil 가능 — Config.UpstreamAddr 비었을 때
	reloadMu  sync.Mutex
}

// Config — Server 옵션.
type Config struct {
	// ListenPort — FIX session listen 포트 (Phase A default 5011 — edge-fix 5001 과 분리).
	ListenPort int

	// SenderCompID — WTG self CompID.
	SenderCompID string

	// HeartBtInt — Heartbeat 주기 (초).
	HeartBtInt int

	// Counterparties — 정적 seed. Phase A 호환. EtcdEndpoints 가 채워지면
	// etcd snapshot 이 위에 덮음 (edge-fix 와 동일).
	Counterparties map[string]Counterparty

	// EtcdEndpoints + EtcdCounterpartiesPrefix — Phase B-1 동적 정책. edge-fix
	// 와 동일 store (`wtg/fix/counterparties/`) 를 읽되 AllowsMD() 필터. 빈값
	// 이면 정적 seed 만.
	EtcdEndpoints            string
	EtcdCounterpartiesPrefix string

	// UpstreamAddr — Phase B-2a. mci-price 의 gRPC endpoint (예: 127.0.0.1:50051).
	// 채워지면 PriceService.SubscribeQuote stream 유지 + 심볼별 최신 quote 캐시.
	// MDR 응답 시 static provider 대신 이 캐시 우선. 빈값이면 static fallback 만.
	UpstreamAddr string

	// UpstreamSubscriberID — SubscribeQuote req 의 subscriber_id. 다중 인스턴스
	// 구분용. 빈값이면 "mci-edge-md-<port>".
	UpstreamSubscriberID string

	// UpstreamProfileKeys — SubscribeQuote req 의 profile filter (예:
	// "FIX.HQ.VIP"). Phase B-2a 는 편의로 빈 리스트 = 모두 수신. Phase C 에서
	// 등록된 MD counterparty profile 의 합집합으로 자동 계산 예정.
	UpstreamProfileKeys []string

	// LogQuickfix — quickfix 내부 log 노출. default false.
	LogQuickfix bool
}

// DefaultConfig — 기본값.
func DefaultConfig() Config {
	return Config{
		ListenPort:               5011,
		SenderCompID:             "WTG_MD",
		HeartBtInt:               30,
		EtcdCounterpartiesPrefix: "wtg/fix/counterparties/",
	}
}

// Stats — 서비스 카운터 (운영 endpoint / 테스트).
type Stats struct {
	LogonOK        uint64 `json:"logon_ok"`
	LogonReject    uint64 `json:"logon_reject"`
	MDRReceived    uint64 `json:"mdr_received"`
	MDRRejected    uint64 `json:"mdr_rejected"`
	SnapshotSent   uint64 `json:"snapshot_sent"`
	SymbolMissing  uint64 `json:"symbol_missing"`
	ActiveSessions int    `json:"active_sessions"`
}

// NewServer — Server 생성. logger nil 이면 slog.Default(). quickfix.Acceptor 는
// Start() 호출 시까지 listen 안 함.
func NewServer(cfg Config, logger *slog.Logger) (*Server, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.ListenPort == 0 {
		cfg.ListenPort = 5011
	}
	if cfg.HeartBtInt == 0 {
		cfg.HeartBtInt = 30
	}
	if cfg.SenderCompID == "" {
		cfg.SenderCompID = "WTG_MD"
	}

	// policy 선택 — etcd 이면 MemoryCounterpartyPolicy + Start 에서 watcher 가동.
	// 아니면 staticPolicy (Phase A 호환).
	var policy CounterpartyPolicy
	var memPolicy *MemoryCounterpartyPolicy
	if cfg.EtcdEndpoints != "" {
		memPolicy = NewMemoryCounterpartyPolicy()
		// 정적 seed 를 첫 snapshot 으로 (etcd 초기 load 가 그 위에 Replace).
		if len(cfg.Counterparties) > 0 {
			memPolicy.Replace(cfg.Counterparties)
		}
		policy = memPolicy
	} else {
		policy = &staticPolicy{m: cfg.Counterparties}
	}
	// upstream — 있으면 GrpcQuoteSource 준비 (Start 에서 goroutine).
	var upstream *GrpcQuoteSource
	if cfg.UpstreamAddr != "" {
		subID := cfg.UpstreamSubscriberID
		if subID == "" {
			subID = fmt.Sprintf("mci-edge-md-%d", cfg.ListenPort)
		}
		upstream = NewGrpcQuoteSource(cfg.UpstreamAddr, subID, cfg.UpstreamProfileKeys, logger)
	}
	app := newFixApp(cfg, logger, policy, DefaultStaticProvider(), upstream)

	settings, err := buildSettings(cfg)
	if err != nil {
		return nil, fmt.Errorf("buildSettings: %w", err)
	}
	storeFactory := quickfix.NewMemoryStoreFactory()
	logFactory := quickfix.NewNullLogFactory()
	acceptor, err := quickfix.NewAcceptor(app, storeFactory, settings, logFactory)
	if err != nil {
		return nil, fmt.Errorf("NewAcceptor: %w", err)
	}
	return &Server{cfg: cfg, logger: logger, app: app, acceptor: acceptor, upstream: upstream}, nil
}

// Start — etcd watcher + upstream quote loop + quickfix acceptor 시작 +
// ctx.Done() 까지 유지. 블로킹.
func (s *Server) Start(ctx context.Context) error {
	if err := s.startCounterpartyWatcher(ctx); err != nil {
		s.logger.Warn("MD counterparty etcd watcher 실패 — 정적 seed 만",
			slog.Any("err", err))
	}
	// Phase B-2a — upstream stream 시작 (있다면).
	if s.upstream != nil {
		go s.upstream.StartLoop(ctx)
	}
	if err := s.acceptor.Start(); err != nil {
		return fmt.Errorf("acceptor.Start: %w", err)
	}
	s.logger.Info("mci-edge-md listen 시작",
		slog.Int("port", s.cfg.ListenPort),
		slog.String("sender", s.cfg.SenderCompID),
		slog.Int("counterparties_seed", len(s.cfg.Counterparties)),
		slog.String("etcd", s.cfg.EtcdEndpoints),
		slog.String("upstream", s.cfg.UpstreamAddr))
	<-ctx.Done()
	s.acceptor.Stop()
	if s.cpWatcher != nil {
		s.cpWatcher.Stop()
	}
	if s.cpEtcdCli != nil {
		_ = s.cpEtcdCli.Close()
	}
	s.logger.Info("mci-edge-md 종료")
	return nil
}

// UpstreamStats — 진단용 upstream 통계 (nil 가능).
func (s *Server) UpstreamStats() *GrpcQuoteSourceStats {
	if s.upstream == nil {
		return nil
	}
	st := s.upstream.Stats()
	return &st
}

// startCounterpartyWatcher — etcd 활성 환경에서 dial + watcher Start.
func (s *Server) startCounterpartyWatcher(ctx context.Context) error {
	if s.cfg.EtcdEndpoints == "" {
		return nil
	}
	memPolicy, ok := s.app.policy.(*MemoryCounterpartyPolicy)
	if !ok {
		return nil
	}
	eps := strings.Split(s.cfg.EtcdEndpoints, ",")
	for i, e := range eps {
		eps[i] = strings.TrimSpace(e)
	}
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   eps,
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("etcd dial: %w", err)
	}
	prefix := s.cfg.EtcdCounterpartiesPrefix
	if prefix == "" {
		prefix = "wtg/fix/counterparties/"
	}
	w := NewEtcdCounterpartyWatcher(cli, prefix, memPolicy, s.logger)
	if err := w.Start(ctx); err != nil {
		_ = cli.Close()
		return fmt.Errorf("watcher Start: %w", err)
	}
	s.cpEtcdCli = cli
	s.cpWatcher = w
	s.logger.Info("MD CounterpartyPolicy etcd watcher 활성",
		slog.String("prefix", prefix))
	return nil
}

// Stats — 서비스 카운터 스냅샷.
func (s *Server) Stats() Stats {
	return s.app.snapshot()
}

// CounterpartySnapshot — 진단용 정책 스냅샷 노출.
func (s *Server) CounterpartySnapshot() map[string]Counterparty {
	return s.app.policy.Snapshot()
}

// Provider — 진단/테스트용 provider 조회.
func (s *Server) Provider() *StaticQuoteProvider { return s.app.provider }

// Reload — SIGHUP handler. 현재 policy snapshot 으로 settings 재빌드 + acceptor
// 재시작. 신규 CID 등록 시 [SESSION] block 이 부팅 시 seed 만 잡기 때문에 필요.
// 짧은 (수 초) 재로그온 윈도우 발생 — 시장 마감 시간 등 적용 권장.
//
// thread-safe. SIGHUP handler 또는 admin endpoint 에서 호출.
func (s *Server) Reload() (retErr error) {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	defer func() {
		if retErr != nil {
			getMetrics().reload.WithLabelValues("fail").Inc()
		} else {
			getMetrics().reload.WithLabelValues("ok").Inc()
		}
	}()

	snap := s.app.policy.Snapshot()
	cfg := s.cfg
	cfg.Counterparties = snap
	settings, err := buildSettings(cfg)
	if err != nil {
		return fmt.Errorf("reload buildSettings: %w", err)
	}
	if s.acceptor != nil {
		s.acceptor.Stop()
		s.acceptor = nil
	}
	newAcceptor, err := quickfix.NewAcceptor(s.app, quickfix.NewMemoryStoreFactory(), settings, quickfix.NewNullLogFactory())
	if err != nil {
		return fmt.Errorf("reload NewAcceptor: %w", err)
	}
	if err := newAcceptor.Start(); err != nil {
		return fmt.Errorf("reload Start: %w", err)
	}
	s.acceptor = newAcceptor
	// s.cfg.Counterparties 는 초기 seed 로만 보관. Reload 후엔 policy 가 SoT.
	// (edge-fix 와 동일 — write 하면 Start goroutine 의 log read 와 race)
	s.logger.Info("mci-edge-md reload",
		slog.Int("counterparties", len(snap)))
	return nil
}

// buildSettings — quickfix acceptor settings 조립. cfg.Counterparties 의 CID 마다
// [SESSION] block 등록 (mci-edge-fix 와 동일 패턴).
func buildSettings(cfg Config) (*quickfix.Settings, error) {
	var b bytes.Buffer
	fmt.Fprintf(&b, `[DEFAULT]
ConnectionType=acceptor
SocketAcceptPort=%d
BeginString=FIX.4.4
SenderCompID=%s
HeartBtInt=%d
StartTime=00:00:00
EndTime=00:00:00
ResetOnLogon=Y
`, cfg.ListenPort, cfg.SenderCompID, cfg.HeartBtInt)

	if len(cfg.Counterparties) == 0 {
		fmt.Fprintf(&b, "\n[SESSION]\nTargetCompID=PLACEHOLDER\n")
	}
	for cid := range cfg.Counterparties {
		cid = strings.TrimSpace(cid)
		if cid == "" {
			continue
		}
		fmt.Fprintf(&b, "\n[SESSION]\nTargetCompID=%s\n", cid)
	}
	settings, err := quickfix.ParseSettings(&b)
	if err != nil {
		return nil, fmt.Errorf("ParseSettings: %w", err)
	}
	return settings, nil
}

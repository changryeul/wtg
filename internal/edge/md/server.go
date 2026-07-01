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

	"github.com/quickfixgo/quickfix"
)

// Server — mci-edge-md lifecycle. quickfix.Acceptor wrap.
type Server struct {
	cfg      Config
	logger   *slog.Logger
	app      *fixApp
	acceptor *quickfix.Acceptor
	reloadMu sync.Mutex
}

// Config — Server 옵션.
type Config struct {
	// ListenPort — FIX session listen 포트 (Phase A default 5011 — edge-fix 5001 과 분리).
	ListenPort int

	// SenderCompID — WTG self CompID.
	SenderCompID string

	// HeartBtInt — Heartbeat 주기 (초).
	HeartBtInt int

	// Counterparties — 정적 seed. Phase A 는 이거만. Phase B 는 etcd watch 로
	// 교체.
	Counterparties map[string]Counterparty

	// LogQuickfix — quickfix 내부 log 노출 (Phase B). default false.
	LogQuickfix bool
}

// DefaultConfig — Phase A 기본값.
func DefaultConfig() Config {
	return Config{
		ListenPort:   5011,
		SenderCompID: "WTG_MD",
		HeartBtInt:   30,
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

	policy := &staticPolicy{m: cfg.Counterparties}
	app := newFixApp(cfg, logger, policy, DefaultStaticProvider())

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
	return &Server{cfg: cfg, logger: logger, app: app, acceptor: acceptor}, nil
}

// Start — quickfix acceptor 시작 + ctx.Done() 까지 유지. 블로킹.
func (s *Server) Start(ctx context.Context) error {
	if err := s.acceptor.Start(); err != nil {
		return fmt.Errorf("acceptor.Start: %w", err)
	}
	s.logger.Info("mci-edge-md listen 시작",
		slog.Int("port", s.cfg.ListenPort),
		slog.String("sender", s.cfg.SenderCompID),
		slog.Int("counterparties_seed", len(s.cfg.Counterparties)))
	<-ctx.Done()
	s.acceptor.Stop()
	s.logger.Info("mci-edge-md 종료")
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

// Reload — Phase C 대비. Phase A 는 seed 재갱신만 (실제 SIGHUP 은 아직 미연결).
// mci-edge-fix.Reload 와 동일 시그니처 유지.
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
	s.cfg.Counterparties = snap
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

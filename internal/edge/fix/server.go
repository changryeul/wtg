// Package fix — mci-edge-fix 의 FIX 4.4 session 종단 + 매매 변환.
//
// Phase A — Logon + NewOrderSingle (35=D) → POST /v1/tx 단방향. ExecutionReport
// 동기 응답 (39=0 New) 만. drop copy (35=8 비동기) 는 Phase B.
//
// 자세한 설계: docs/fix-gateway-design.md.
package fix

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/quickfixgo/quickfix"
)

// Server — mci-edge-fix 의 lifecycle.
//
// quickfix.Acceptor 를 wrap. ctx.Done() 시 Stop().
type Server struct {
	cfg      Config
	logger   *slog.Logger
	app      *fixApp
	acceptor *quickfix.Acceptor
}

// Config — Server 옵션.
type Config struct {
	// ListenPort — FIX session listen 포트 (default 5001).
	ListenPort int

	// SenderCompID — WTG 의 self CompID. 운영 시 SOR/HQ/BRANCH 등.
	SenderCompID string

	// HeartBtInt — Heartbeat 주기 (초). default 30.
	HeartBtInt int

	// Counterparties — 정적 counterparty seed. Phase A 의 단순화 — etcd watch
	// 는 Phase B. map key = SenderCompID (외부 client 의 49=).
	Counterparties map[string]Counterparty

	// TxForwardURL — `/v1/tx` 호출 backend (mci-api). 빈값이면 envelope 을
	// log 만 (envelope wire 검증 모드 — Phase A PoC default).
	TxForwardURL string

	// LogQuickfix — true 면 quickfix 내부 log 도 slog 로 노출. default false
	// (NullLogFactory — boilerplate 최소).
	LogQuickfix bool
}

// Counterparty — 카운터파티 1개의 인증·라우팅 정보.
//
// Phase A 의 정적 seed 의미. Phase B 에서 etcd `wtg/fix/counterparties/<CID>`
// 의 watch 결과로 동적 교체.
type Counterparty struct {
	// Password — Logon 메시지 tag 554 의 비교 대상. 빈값이면 검증 skip
	// (운영 금지, PoC 한정).
	Password string

	// Profile — Principal.Channel/Site/Tier. `/v1/tx` 호출 시 envelope 의
	// X-WTG-Edge-* 헤더로 전달.
	Channel string // "FIX"
	Site    string // "HQ" / "BRANCH"
	Tier    string // "VIP" / "GOLD" / "STD"

	// Usid — Principal.Usid. log / audit 의 일상 ID.
	Usid string
}

// DefaultConfig.
func DefaultConfig() Config {
	return Config{
		ListenPort:   5001,
		SenderCompID: "WTG",
		HeartBtInt:   30,
	}
}

// NewServer — Server 생성. logger nil 이면 slog.Default().
//
// quickfix.Acceptor 는 Start() 호출 시까지 listen 안 함.
func NewServer(cfg Config, logger *slog.Logger) (*Server, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.ListenPort == 0 {
		cfg.ListenPort = 5001
	}
	if cfg.HeartBtInt == 0 {
		cfg.HeartBtInt = 30
	}
	if cfg.SenderCompID == "" {
		cfg.SenderCompID = "WTG"
	}
	app := newFixApp(cfg, logger)

	settings, err := buildSettings(cfg)
	if err != nil {
		return nil, fmt.Errorf("buildSettings: %w", err)
	}

	logFactory := quickfix.NewNullLogFactory()
	// LogQuickfix 옵션 — Phase B 에서 slog-backed LogFactory 로 교체 가능.
	_ = cfg.LogQuickfix

	acceptor, err := quickfix.NewAcceptor(app,
		quickfix.NewMemoryStoreFactory(), settings, logFactory)
	if err != nil {
		return nil, fmt.Errorf("NewAcceptor: %w", err)
	}

	return &Server{
		cfg:      cfg,
		logger:   logger,
		app:      app,
		acceptor: acceptor,
	}, nil
}

// Start — quickfix acceptor listen 시작 + ctx.Done() 까지 유지.
//
// 블로킹. 호출자가 별도 goroutine 에서 호출하거나 main 마지막에 둠.
func (s *Server) Start(ctx context.Context) error {
	if err := s.acceptor.Start(); err != nil {
		return fmt.Errorf("acceptor.Start: %w", err)
	}
	s.logger.Info("mci-edge-fix listen 시작",
		slog.Int("port", s.cfg.ListenPort),
		slog.String("sender", s.cfg.SenderCompID),
		slog.Int("counterparties", len(s.cfg.Counterparties)))
	<-ctx.Done()
	s.acceptor.Stop()
	s.logger.Info("mci-edge-fix 종료")
	return nil
}

// Stats — Server 의 누적 카운터 (운영 endpoint 용 / 테스트 용).
type Stats struct {
	LogonOK         uint64 `json:"logon_ok"`
	LogonReject     uint64 `json:"logon_reject"`
	OrdersReceived  uint64 `json:"orders_received"`
	OrdersForwarded uint64 `json:"orders_forwarded"`
	OrdersRejected  uint64 `json:"orders_rejected"`
	ActiveSessions  int    `json:"active_sessions"`
}

func (s *Server) Stats() Stats {
	return s.app.snapshot()
}

// buildSettings — Config 의 Counterparties seed 로 quickfix settings 문자열
// 동적 생성. ParseSettings 가 그 reader 를 받음.
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
		// 단일 와일드카드 session — Phase A 의 PoC 모드. 누가 붙어도 fixApp 의
		// FromAdmin 에서 cfg.Counterparties 검증 후 reject.
		fmt.Fprintf(&b, "\n[SESSION]\nTargetCompID=*\n")
	} else {
		// 등록된 모든 counterparty 의 [SESSION] block.
		for cid := range cfg.Counterparties {
			cid = strings.TrimSpace(cid)
			if cid == "" {
				continue
			}
			fmt.Fprintf(&b, "\n[SESSION]\nTargetCompID=%s\n", cid)
		}
	}

	settings, err := quickfix.ParseSettings(&b)
	if err != nil {
		return nil, fmt.Errorf("ParseSettings: %w", err)
	}
	return settings, nil
}

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
	"os"
	"strings"
	"sync"
	"time"

	"github.com/quickfixgo/quickfix"
	"github.com/quickfixgo/quickfix/store/file"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// selectStoreFactory — cfg.StoreDir 채워졌으면 FileStore (seq 영속),
// 빈값이면 MemoryStore (재시작 시 seq 1 부터).
func selectStoreFactory(cfg Config, settings *quickfix.Settings) (quickfix.MessageStoreFactory, error) {
	if cfg.StoreDir == "" {
		return quickfix.NewMemoryStoreFactory(), nil
	}
	if err := os.MkdirAll(cfg.StoreDir, 0o755); err != nil {
		return nil, fmt.Errorf("StoreDir mkdir: %w", err)
	}
	// quickfix file store 는 settings 의 GlobalSettings 의 FileStorePath 를 읽음.
	settings.GlobalSettings().Set("FileStorePath", cfg.StoreDir)
	return file.NewStoreFactory(settings), nil
}

// CounterpartySnapshot — admin 진단용 정책 스냅샷 노출.
func (s *Server) CounterpartySnapshot() map[string]Counterparty {
	return s.app.policy.Snapshot()
}

// Reload — Phase C. 현재 policy snapshot 으로 settings 재빌드 + acceptor
// 재시작. 새 SenderCompID 등록 시 SIGHUP 으로 호출. 기존 active session 은
// 끊김 — 짧은 (수 초) 재로그온 윈도우 발생. 시장 마감 시간 등 적용 권장.
//
// thread-safe — caller 가 SIGHUP signal handler 또는 admin endpoint 에서 호출.
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

	// 새 policy snapshot 으로 cfg.Counterparties 갱신 + settings 재빌드.
	snap := s.app.policy.Snapshot()
	cfg := s.cfg
	cfg.Counterparties = snap

	settings, err := buildSettings(cfg)
	if err != nil {
		return fmt.Errorf("reload buildSettings: %w", err)
	}

	// quickfix 의 global session registry 충돌 회피 — 기존 Stop 먼저, 그 후
	// 새 Acceptor 생성. 짧은 (수 초) 재로그온 윈도우 발생.
	if s.acceptor != nil {
		s.acceptor.Stop()
		s.acceptor = nil
	}

	storeFactory, sfErr := selectStoreFactory(s.cfg, settings)
	if sfErr != nil {
		return sfErr
	}
	newAcceptor, err := quickfix.NewAcceptor(s.app, storeFactory, settings, quickfix.NewNullLogFactory())
	if err != nil {
		return fmt.Errorf("reload NewAcceptor: %w", err)
	}
	if err := newAcceptor.Start(); err != nil {
		return fmt.Errorf("reload Start: %w", err)
	}
	s.acceptor = newAcceptor
	s.cfg.Counterparties = snap
	s.logger.Info("mci-edge-fix reload",
		slog.Int("counterparties", len(snap)))
	return nil
}

// Server — mci-edge-fix 의 lifecycle.
//
// quickfix.Acceptor 를 wrap. ctx.Done() 시 Stop().
type Server struct {
	cfg       Config
	logger    *slog.Logger
	app       *fixApp
	acceptor  *quickfix.Acceptor
	cpWatcher *EtcdCounterpartyWatcher
	cpEtcdCli *clientv3.Client
	reloadMu  sync.Mutex // Reload 직렬화
}

// Config — Server 옵션.
type Config struct {
	// ListenPort — FIX session listen 포트 (default 5001).
	ListenPort int

	// SenderCompID — WTG 의 self CompID. 운영 시 SOR/HQ/BRANCH 등.
	SenderCompID string

	// HeartBtInt — Heartbeat 주기 (초). default 30.
	HeartBtInt int

	// Counterparties — 정적 counterparty seed. Phase A 호환 — etcd 비활성
	// 환경에서 즉시 시작 가능. EtcdEndpoints 가 채워지면 etcd 정책이 우선.
	Counterparties map[string]Counterparty

	// EtcdEndpoints + EtcdCounterpartiesPrefix — Phase B 의 동적 정책. 채워지면
	// CounterpartyPolicy 의 etcd watcher 가동 + 정적 Counterparties 는 무시.
	// 빈값이면 정적 seed 만 사용 (backward compat).
	//
	// etcd schema: <prefix><SenderCompID> = JSON Counterparty
	EtcdEndpoints           string
	EtcdCounterpartiesPrefix string // default "wtg/fix/counterparties/"

	// TxForwardURL — `/v1/tx` 호출 backend (mci-api). 빈값이면 envelope 을
	// log 만 (envelope wire 검증 모드 — Phase A PoC default).
	TxForwardURL string

	// LogQuickfix — true 면 quickfix 내부 log 도 slog 로 노출. default false
	// (NullLogFactory — boilerplate 최소).
	LogQuickfix bool

	// StoreDir — Phase D. 채워지면 FileStore 사용 (재시작 시 sequence 보존).
	// 빈값=MemoryStore (재시작 시 seq 1 부터 — dev/PoC). 운영 권장 — 영속
	// dir 필수.
	//
	// quickfix 가 dir 아래에 session 별 *.body / *.header / *.senderseqnums /
	// *.targetseqnums 파일 생성.
	StoreDir string
}

// Counterparty — 카운터파티 1개의 인증·라우팅 정보.
//
// Phase A 의 정적 seed 의미. Phase B 에서 etcd `wtg/fix/counterparties/<CID>`
// 의 watch 결과로 동적 교체. JSON tag 는 mci-admin 의 admin REST 와 wire 일관.
type Counterparty struct {
	// Password — Logon 메시지 tag 554 의 비교 대상. 빈값이면 검증 skip
	// (운영 금지, PoC 한정).
	Password string `json:"password"`

	// Profile — Principal.Channel/Site/Tier. `/v1/tx` 호출 시 envelope 의
	// X-WTG-Edge-* 헤더로 전달.
	Channel string `json:"channel"` // "FIX"
	Site    string `json:"site"`    // "HQ" / "BRANCH"
	Tier    string `json:"tier"`    // "VIP" / "GOLD" / "STD"

	// Usid — Principal.Usid. log / audit 의 일상 ID.
	Usid string `json:"usid"`

	// OrderAlias — Phase B Layer 2. NewOrderSingle 변환 시 envelope 의 alias
	// 필드. 카운터파티마다 다른 wire/dialect 를 매매 엔진의 routing 으로 분기.
	// 빈값이면 "FIX_NEW_ORDER" default — 모든 카운터파티 동일 alias (Phase A 호환).
	// 예: "ECN_DEUTSCHE_ORDER" / "MM_CITI_ORDER" — 매매 엔진의 alias 룰이 별도
	// transaction 으로 dispatch.
	OrderAlias string `json:"order_alias,omitempty"`
}

// DefaultConfig.
func DefaultConfig() Config {
	return Config{
		ListenPort:               5001,
		SenderCompID:             "WTG",
		HeartBtInt:               30,
		EtcdCounterpartiesPrefix: "wtg/fix/counterparties/",
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
	// policy 결정 — etcd 이면 MemoryCounterpartyPolicy + watcher (Start 에서 가동),
	// 아니면 정적 staticPolicy.
	var policy CounterpartyPolicy
	var memPolicy *MemoryCounterpartyPolicy
	if cfg.EtcdEndpoints != "" {
		memPolicy = NewMemoryCounterpartyPolicy()
		// 정적 seed 가 있으면 첫 snapshot 으로 채움 — etcd 초기 load 가 그 위에
		// Replace 한다 (etcd 가 권위 출처).
		if len(cfg.Counterparties) > 0 {
			memPolicy.Replace(cfg.Counterparties)
		}
		policy = memPolicy
	} else {
		policy = &staticPolicy{m: cfg.Counterparties}
	}
	app := newFixApp(cfg, logger, policy)

	settings, err := buildSettings(cfg)
	if err != nil {
		return nil, fmt.Errorf("buildSettings: %w", err)
	}

	logFactory := quickfix.NewNullLogFactory()
	// LogQuickfix 옵션 — Phase B 에서 slog-backed LogFactory 로 교체 가능.
	_ = cfg.LogQuickfix

	storeFactory, sfErr := selectStoreFactory(cfg, settings)
	if sfErr != nil {
		return nil, sfErr
	}
	acceptor, err := quickfix.NewAcceptor(app, storeFactory, settings, logFactory)
	if err != nil {
		return nil, fmt.Errorf("NewAcceptor: %w", err)
	}

	s := &Server{
		cfg:      cfg,
		logger:   logger,
		app:      app,
		acceptor: acceptor,
	}
	// etcd watcher 준비 (start 는 Server.Start 에서).
	if memPolicy != nil {
		s.cpWatcher = nil // Start 시점에 etcd dial 후 채움
	}
	return s, nil
}

// Start — quickfix acceptor listen 시작 + etcd watcher (있다면) + ctx.Done() 까지 유지.
//
// 블로킹. 호출자가 별도 goroutine 에서 호출하거나 main 마지막에 둠.
func (s *Server) Start(ctx context.Context) error {
	// etcd policy watcher 가동 (EtcdEndpoints 가 채워졌고 app.policy 가
	// MemoryCounterpartyPolicy 인 경우).
	if err := s.startCounterpartyWatcher(ctx); err != nil {
		s.logger.Warn("counterparty etcd watcher 실패 — 정적 seed 만",
			slog.Any("err", err))
	}

	if err := s.acceptor.Start(); err != nil {
		return fmt.Errorf("acceptor.Start: %w", err)
	}
	s.logger.Info("mci-edge-fix listen 시작",
		slog.Int("port", s.cfg.ListenPort),
		slog.String("sender", s.cfg.SenderCompID),
		slog.Int("counterparties_seed", len(s.cfg.Counterparties)),
		slog.String("etcd", s.cfg.EtcdEndpoints))
	<-ctx.Done()
	s.acceptor.Stop()
	if s.cpWatcher != nil {
		s.cpWatcher.Stop()
	}
	if s.cpEtcdCli != nil {
		_ = s.cpEtcdCli.Close()
	}
	s.logger.Info("mci-edge-fix 종료")
	return nil
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
	s.logger.Info("CounterpartyPolicy etcd watcher 활성",
		slog.String("prefix", prefix))
	return nil
}

// Stats — Server 의 누적 카운터 (운영 endpoint 용 / 테스트 용).
type Stats struct {
	LogonOK            uint64 `json:"logon_ok"`
	LogonReject        uint64 `json:"logon_reject"`
	OrdersReceived     uint64 `json:"orders_received"`
	OrdersForwarded    uint64 `json:"orders_forwarded"`
	OrdersRejected     uint64 `json:"orders_rejected"`
	ExecReportSent     uint64 `json:"exec_report_sent"`     // Phase B-2
	ExecReportRejected uint64 `json:"exec_report_rejected"` // Phase B-2
	ActiveSessions     int    `json:"active_sessions"`
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

	// quickfix 의 [SESSION] TargetCompID 는 명시 매칭만 (와일드카드 미지원).
	// 따라서 cfg.Counterparties + (etcd 초기 snapshot) 모두 [SESSION] block 으로
	// 등록. Phase B 의 runtime password 변경은 policy 가 처리하지만, 새 CID 의
	// 등록은 mci-edge-fix 재시작 (또는 SIGHUP) 필요 — Phase C 작업.
	if len(cfg.Counterparties) == 0 {
		// PoC / dev — seed 없으면 placeholder. 운영은 seed 필수.
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

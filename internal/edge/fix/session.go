package fix

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quickfixgo/field"
	"github.com/quickfixgo/fix44/newordersingle"
	"github.com/quickfixgo/quickfix"
	"github.com/quickfixgo/tag"
)

// fixApp — quickfix.Application 인터페이스 구현. lifecycle 7 메서드.
//
// 책임:
//   - OnCreate / OnLogon / OnLogout — session 상태 관리 + 카운터.
//   - FromAdmin — Logon (35=A) 의 Password 검증 + Principal 결정.
//   - FromApp — NewOrderSingle (35=D) 등 application 메시지 처리.
//   - ToAdmin / ToApp — outbound 메시지 (Phase A 는 거의 no-op).
type fixApp struct {
	cfg    Config
	logger *slog.Logger

	// policy — runtime counterparty 검증. etcd 활성이면 MemoryCounterpartyPolicy,
	// 아니면 staticPolicy.
	policy CounterpartyPolicy

	// active : 로그온 통과한 session 의 (sessionID → Principal) 매핑.
	mu     sync.RWMutex
	active map[string]Principal

	// 카운터.
	logonOK         atomic.Uint64
	logonReject     atomic.Uint64
	ordersReceived  atomic.Uint64
	ordersForwarded atomic.Uint64
	ordersRejected  atomic.Uint64

	// forwarder — NewOrderSingle 변환 결과를 /v1/tx 로 보내는 함수.
	// nil 이면 envelope log 만 (PoC default).
	forwarder OrderForwarder
}

// Principal — Logon 통과한 session 의 인증된 주체.
type Principal struct {
	SenderCompID string
	Usid         string
	Channel      string
	Site         string
	Tier         string
}

// OrderForwarder — NewOrderSingle envelope 변환 결과를 backend (mci-api) 로
// 보내는 인터페이스. nil 이면 envelope log 만.
type OrderForwarder interface {
	Forward(ctx context.Context, p Principal, env OrderEnvelope) error
}

func newFixApp(cfg Config, logger *slog.Logger, policy CounterpartyPolicy) *fixApp {
	app := &fixApp{
		cfg:    cfg,
		logger: logger,
		policy: policy,
		active: make(map[string]Principal),
	}
	if cfg.TxForwardURL != "" {
		app.forwarder = newHTTPForwarder(cfg.TxForwardURL, logger)
	}
	return app
}

// OnCreate — quickfix 가 session 발견 직후 호출. Logon 전.
func (a *fixApp) OnCreate(sid quickfix.SessionID) {
	a.logger.Info("FIX session created",
		slog.String("sender", sid.SenderCompID),
		slog.String("target", sid.TargetCompID))
}

// OnLogon — Logon (35=A) 통과 후. FromAdmin 에서 reject 안 했다면 호출됨.
func (a *fixApp) OnLogon(sid quickfix.SessionID) {
	a.logonOK.Add(1)
	a.logger.Info("FIX logon",
		slog.String("sender", sid.SenderCompID),
		slog.String("target", sid.TargetCompID))
}

// OnLogout — Logout (35=5) 또는 session 종료.
func (a *fixApp) OnLogout(sid quickfix.SessionID) {
	a.mu.Lock()
	delete(a.active, sid.String())
	a.mu.Unlock()
	a.logger.Info("FIX logout", slog.String("sender", sid.SenderCompID))
}

// ToAdmin — outbound admin 메시지 (Heartbeat / TestRequest 등).
// Phase A 는 no-op.
func (a *fixApp) ToAdmin(msg *quickfix.Message, sid quickfix.SessionID) {}

// FromAdmin — inbound admin 메시지. Logon (35=A) 의 Password 검증 + Principal
// 결정의 핵심 지점.
//
// reject 반환 시 quickfix 가 Logout 보내고 session 끊음.
func (a *fixApp) FromAdmin(msg *quickfix.Message, sid quickfix.SessionID) quickfix.MessageRejectError {
	msgType, err := msg.MsgType()
	if err != nil {
		return nil
	}
	if msgType != "A" {
		return nil
	}
	// Logon (35=A) — policy lookup (etcd 동적 / 정적 seed 통일).
	cp, ok := a.policy.Lookup(sid.TargetCompID)
	if !ok {
		a.logonReject.Add(1)
		a.logger.Warn("FIX logon reject — counterparty 미등록",
			slog.String("target", sid.TargetCompID))
		return quickfix.NewBusinessMessageRejectError(
			"counterparty 미등록", 0, nil)
	}
	if cp.Password != "" {
		pw, err := msg.Body.GetString(tag.Password)
		if err != nil || pw != cp.Password {
			a.logonReject.Add(1)
			a.logger.Warn("FIX logon reject — password 불일치",
				slog.String("target", sid.TargetCompID))
			return quickfix.NewBusinessMessageRejectError(
				"password 불일치", 0, nil)
		}
	}
	// Principal 주입.
	p := Principal{
		SenderCompID: sid.TargetCompID,
		Usid:         cp.Usid,
		Channel:      cp.Channel,
		Site:         cp.Site,
		Tier:         cp.Tier,
	}
	if p.Usid == "" {
		p.Usid = sid.TargetCompID
	}
	if p.Channel == "" {
		p.Channel = "FIX"
	}
	a.mu.Lock()
	a.active[sid.String()] = p
	a.mu.Unlock()
	return nil
}

// ToApp — outbound application 메시지. Phase A 는 ExecutionReport 동기 응답
// 만 — 그건 별도 send 가 아니라 broker 응답 chain. 여기선 no-op.
func (a *fixApp) ToApp(msg *quickfix.Message, sid quickfix.SessionID) error {
	return nil
}

// FromApp — inbound application 메시지. NewOrderSingle (35=D) 처리.
//
// 흐름:
//  1. msgType 추출 — 35=D 아니면 reject.
//  2. quickfix.fix44.newordersingle 으로 typed 메시지 파싱.
//  3. order_mapper 로 OrderEnvelope 변환.
//  4. forwarder 로 /v1/tx 호출 (있다면) — 결과 ExecutionReport 변환 + send.
//  5. forwarder 가 nil 이면 envelope log 만.
func (a *fixApp) FromApp(msg *quickfix.Message, sid quickfix.SessionID) quickfix.MessageRejectError {
	msgType, err := msg.MsgType()
	if err != nil {
		return nil
	}
	if msgType != "D" {
		// Phase A 는 NewOrderSingle 만. 그 외는 BusinessMessageReject.
		return quickfix.NewBusinessMessageRejectError(
			"Phase A 미지원 메시지", 0, nil)
	}

	a.mu.RLock()
	p, ok := a.active[sid.String()]
	a.mu.RUnlock()
	if !ok {
		return quickfix.NewBusinessMessageRejectError("session 미인증", 0, nil)
	}

	nos := newordersingle.FromMessage(msg)
	env, mapErr := mapNewOrderSingle(nos)
	if mapErr != nil {
		a.ordersRejected.Add(1)
		a.logger.Warn("NewOrderSingle 변환 실패",
			slog.String("sender", sid.TargetCompID),
			slog.Any("error", mapErr))
		return quickfix.NewBusinessMessageRejectError(mapErr.Error(), 0, nil)
	}
	a.ordersReceived.Add(1)

	// PoC default — forwarder 없음. envelope log 만.
	if a.forwarder == nil {
		a.logger.Info("NewOrderSingle 수신 (envelope log 모드)",
			slog.String("sender", sid.TargetCompID),
			slog.String("symbol", env.Symbol),
			slog.String("side", env.Side),
			slog.Float64("qty", env.Qty),
			slog.Float64("price", env.Price),
			slog.String("client_order_id", env.ClientOrderID))
		return nil
	}

	// 운영 모드 — backend forward.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.forwarder.Forward(ctx, p, env); err != nil {
		a.ordersRejected.Add(1)
		a.logger.Warn("/v1/tx forward 실패",
			slog.String("client_order_id", env.ClientOrderID),
			slog.Any("error", err))
		return quickfix.NewBusinessMessageRejectError(err.Error(), 0, nil)
	}
	a.ordersForwarded.Add(1)
	return nil
}

// snapshot — Stats 변환.
func (a *fixApp) snapshot() Stats {
	a.mu.RLock()
	active := len(a.active)
	a.mu.RUnlock()
	return Stats{
		LogonOK:         a.logonOK.Load(),
		LogonReject:     a.logonReject.Load(),
		OrdersReceived:  a.ordersReceived.Load(),
		OrdersForwarded: a.ordersForwarded.Load(),
		OrdersRejected:  a.ordersRejected.Load(),
		ActiveSessions:  active,
	}
}

// fieldSymbol — quickfix.SymbolField 의 thin extract helper. type alias 가
// quickfix import 노출을 줄임.
func fieldSymbol(nos newordersingle.NewOrderSingle) (string, error) {
	var s field.SymbolField
	if err := nos.Get(&s); err != nil {
		return "", errors.New("symbol 추출 실패")
	}
	return s.String(), nil
}

package md

import (
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/quickfixgo/fix44/marketdatarequest"
	"github.com/quickfixgo/quickfix"
	"github.com/quickfixgo/tag"
)

// fixApp — mci-edge-md 의 quickfix.Application. Phase B-2a: MDR → 우선 upstream
// (gRPC latest quote cache) 조회 → miss 시 static provider fallback. 증분
// (35=X) 은 Phase B-2b.
type fixApp struct {
	cfg      Config
	logger   *slog.Logger
	policy   CounterpartyPolicy
	provider *StaticQuoteProvider
	upstream *GrpcQuoteSource // nil 가능 — Phase A 호환 (upstream 없이 static 만)

	mu     sync.RWMutex
	active map[string]Principal

	// 카운터.
	logonOK       atomic.Uint64
	logonReject   atomic.Uint64
	mdrReceived   atomic.Uint64
	mdrRejected   atomic.Uint64
	snapshotSent  atomic.Uint64
	symbolMissing atomic.Uint64
}

// Principal — Logon 통과한 카운터파티. Phase B 에서 upstream SubscribeQuote
// 의 profile_key 결정에 사용.
type Principal struct {
	SenderCompID string
	Usid         string
	Channel      string
	Site         string
	Tier         string
}

func newFixApp(cfg Config, logger *slog.Logger, policy CounterpartyPolicy, provider *StaticQuoteProvider, upstream *GrpcQuoteSource) *fixApp {
	return &fixApp{
		cfg:      cfg,
		logger:   logger,
		policy:   policy,
		provider: provider,
		upstream: upstream,
		active:   make(map[string]Principal),
	}
}

// OnCreate — quickfix session 감지 (Logon 전).
func (a *fixApp) OnCreate(sid quickfix.SessionID) {
	a.logger.Info("MD FIX session created",
		slog.String("sender", sid.SenderCompID),
		slog.String("target", sid.TargetCompID))
}

// OnLogon — Logon 통과 후.
func (a *fixApp) OnLogon(sid quickfix.SessionID) {
	a.logonOK.Add(1)
	getMetrics().logon.WithLabelValues("ok").Inc()
	getMetrics().activeSessions.Inc()
	a.logger.Info("MD FIX logon",
		slog.String("sender", sid.SenderCompID),
		slog.String("target", sid.TargetCompID))
}

// OnLogout — Logout / session 종료.
func (a *fixApp) OnLogout(sid quickfix.SessionID) {
	a.mu.Lock()
	delete(a.active, sid.String())
	a.mu.Unlock()
	getMetrics().activeSessions.Dec()
	a.logger.Info("MD FIX logout", slog.String("sender", sid.SenderCompID))
}

// ToAdmin — outbound admin (Heartbeat 등). Phase A 는 no-op.
func (a *fixApp) ToAdmin(msg *quickfix.Message, sid quickfix.SessionID) {}

// FromAdmin — inbound admin. Logon 검증.
func (a *fixApp) FromAdmin(msg *quickfix.Message, sid quickfix.SessionID) quickfix.MessageRejectError {
	msgType, err := msg.MsgType()
	if err != nil {
		return nil
	}
	if msgType != "A" {
		return nil
	}
	cp, ok := a.policy.Lookup(sid.TargetCompID)
	if !ok {
		a.logonReject.Add(1)
		getMetrics().logon.WithLabelValues("reject").Inc()
		a.logger.Warn("MD FIX logon reject — counterparty 미등록",
			slog.String("target", sid.TargetCompID))
		return quickfix.NewBusinessMessageRejectError("counterparty 미등록", 0, nil)
	}
	if cp.Password != "" {
		pw, err := msg.Body.GetString(tag.Password)
		if err != nil || pw != cp.Password {
			a.logonReject.Add(1)
			getMetrics().logon.WithLabelValues("reject").Inc()
			a.logger.Warn("MD FIX logon reject — password 불일치",
				slog.String("target", sid.TargetCompID))
			return quickfix.NewBusinessMessageRejectError("password 불일치", 0, nil)
		}
	}
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

// ToApp — outbound application (스냅샷 자체). Phase A 는 no-op (직접 SendToTarget).
func (a *fixApp) ToApp(msg *quickfix.Message, sid quickfix.SessionID) error {
	return nil
}

// FromApp — inbound application. Phase A 는 MDR(35=V) 만 처리, 그 외는 reject.
func (a *fixApp) FromApp(msg *quickfix.Message, sid quickfix.SessionID) quickfix.MessageRejectError {
	msgType, err := msg.MsgType()
	if err != nil {
		return nil
	}

	a.mu.RLock()
	_, ok := a.active[sid.String()]
	a.mu.RUnlock()
	if !ok {
		return quickfix.NewBusinessMessageRejectError("session 미인증", 0, nil)
	}

	if msgType != "V" {
		return quickfix.NewBusinessMessageRejectError(
			"미지원 메시지 type="+msgType+" (mci-edge-md 는 35=V 만 수신)", 0, nil)
	}

	a.mdrReceived.Add(1)
	getMetrics().mdrReceived.Inc()

	parsed, pErr := ParseMDR(marketdatarequest.FromMessage(msg))
	if pErr != nil {
		a.mdrRejected.Add(1)
		getMetrics().mdrRejected.WithLabelValues("parse").Inc()
		a.logger.Warn("MDR 파싱 실패",
			slog.String("sender", sid.TargetCompID),
			slog.Any("err", pErr))
		return quickfix.NewBusinessMessageRejectError(pErr.Error(), 0, nil)
	}

	a.logger.Info("MDR 수신",
		slog.String("sender", sid.TargetCompID),
		slog.String("mdreq_id", parsed.MDReqID),
		slog.String("sub_req_type", string(parsed.SubReqType)),
		slog.Int("symbols", len(parsed.Symbols)))

	// Phase B-2a — SubReqType 무관 스냅샷 1회 전송. 우선 upstream cache 조회,
	// miss 시 static provider fallback. Phase B-2b 에서 SNAPSHOT_PLUS_UPDATES 시
	// session subscribe + 35=X 증분 fan-out.
	for _, sym := range parsed.Symbols {
		var snap = tryBuildSnapshot(a, parsed.MDReqID, sym, sid)
		if !snap {
			continue
		}
	}
	return nil
}

// tryBuildSnapshot — upstream cache 우선, static fallback. hit 여부 반환 (metric
// / logging 편의).
func tryBuildSnapshot(a *fixApp, mdReqID, sym string, sid quickfix.SessionID) bool {
	if a.upstream != nil {
		if cq, ok := a.upstream.Latest(sym); ok {
			msg := BuildSnapshotFromCustomerQuote(mdReqID, cq)
			if sendErr := quickfix.SendToTarget(msg.ToMessage(), sid); sendErr != nil {
				a.logger.Warn("스냅샷(upstream) 송신 실패",
					slog.String("symbol", sym), slog.Any("err", sendErr))
				return false
			}
			a.snapshotSent.Add(1)
			getMetrics().snapshotSent.WithLabelValues(sym).Inc()
			return true
		}
	}
	// static fallback.
	q, ok := a.provider.Get(sym)
	if !ok {
		a.symbolMissing.Add(1)
		getMetrics().symbolMissing.WithLabelValues(sym).Inc()
		a.logger.Warn("quote 없음 (upstream miss + static miss) — skip",
			slog.String("symbol", sym))
		return false
	}
	msg := BuildSnapshot(mdReqID, sym, q)
	if sendErr := quickfix.SendToTarget(msg.ToMessage(), sid); sendErr != nil {
		a.logger.Warn("스냅샷(static) 송신 실패",
			slog.String("symbol", sym), slog.Any("err", sendErr))
		return false
	}
	a.snapshotSent.Add(1)
	getMetrics().snapshotSent.WithLabelValues(sym).Inc()
	return true
}

// snapshot — Stats 변환.
func (a *fixApp) snapshot() Stats {
	a.mu.RLock()
	active := len(a.active)
	a.mu.RUnlock()
	return Stats{
		LogonOK:        a.logonOK.Load(),
		LogonReject:    a.logonReject.Load(),
		MDRReceived:    a.mdrReceived.Load(),
		MDRRejected:    a.mdrRejected.Load(),
		SnapshotSent:   a.snapshotSent.Load(),
		SymbolMissing:  a.symbolMissing.Load(),
		ActiveSessions: active,
	}
}

package fix

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quickfixgo/enum"
	"github.com/quickfixgo/field"
	"github.com/quickfixgo/fix44/newordersingle"
	"github.com/quickfixgo/quickfix"
	"github.com/shopspring/decimal"
)

// pickFreePort — 동시 테스트 충돌 회피용 ephemeral 포트.
func pickFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().(*net.TCPAddr)
	_ = l.Close()
	return addr.Port
}

func quietLogger() *slog.Logger {
	// 테스트 진단을 위해 일시적으로 stdout + debug. PoC 통과 후 io.Discard 로 복귀.
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

// loudLogger — 디버그 PoC 용.
func loudLogger(t *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(&tWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

type tWriter struct{ t *testing.T }

func (w *tWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// initiator — 테스트 client 의 quickfix.Application 구현.
type testInitiator struct {
	logonCount atomic.Int32
	mu         sync.Mutex
	logonDone  chan struct{}
}

func newTestInitiator() *testInitiator {
	return &testInitiator{logonDone: make(chan struct{}, 1)}
}

func (i *testInitiator) OnCreate(sid quickfix.SessionID) {}
func (i *testInitiator) OnLogon(sid quickfix.SessionID) {
	i.logonCount.Add(1)
	select {
	case i.logonDone <- struct{}{}:
	default:
	}
}
func (i *testInitiator) OnLogout(sid quickfix.SessionID) {}
func (i *testInitiator) ToAdmin(msg *quickfix.Message, sid quickfix.SessionID) {
	// Logon 메시지에 Password 첨부 — counterparty 검증 통과.
	if t, err := msg.MsgType(); err == nil && t == "A" {
		msg.Body.SetString(554, "secret-pw")
	}
}
func (i *testInitiator) FromAdmin(msg *quickfix.Message, sid quickfix.SessionID) quickfix.MessageRejectError {
	return nil
}
func (i *testInitiator) ToApp(msg *quickfix.Message, sid quickfix.SessionID) error { return nil }
func (i *testInitiator) FromApp(msg *quickfix.Message, sid quickfix.SessionID) quickfix.MessageRejectError {
	return nil
}

// startInitiator — quickfix initiator. acceptorPort 로 연결.
func startInitiator(t *testing.T, acceptorPort int, senderCID, targetCID string) (*testInitiator, *quickfix.Initiator) {
	t.Helper()
	settings := fmt.Sprintf(`[DEFAULT]
ConnectionType=initiator
ReconnectInterval=1
SenderCompID=%s
TargetCompID=%s
SocketConnectHost=127.0.0.1
SocketConnectPort=%d
BeginString=FIX.4.4
HeartBtInt=30
StartTime=00:00:00
EndTime=00:00:00
ResetOnLogon=Y
[SESSION]
TargetCompID=%s
`, senderCID, targetCID, acceptorPort, targetCID)

	cfg, err := quickfix.ParseSettings(strings.NewReader(settings))
	if err != nil {
		t.Fatalf("initiator ParseSettings: %v", err)
	}
	app := newTestInitiator()
	ini, err := quickfix.NewInitiator(app, quickfix.NewMemoryStoreFactory(), cfg, quickfix.NewNullLogFactory())
	if err != nil {
		t.Fatalf("NewInitiator: %v", err)
	}
	if err := ini.Start(); err != nil {
		t.Fatalf("ini.Start: %v", err)
	}
	t.Cleanup(func() { ini.Stop() })
	return app, ini
}

// startServer — acceptor 가동 (별도 goroutine, ctx cancel 로 종료).
func startServer(t *testing.T, cfg Config) (*Server, context.CancelFunc) {
	t.Helper()
	// 디버그 시 loudLogger(t) 로 교체.
	srv, err := NewServer(cfg, quietLogger())
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Start(ctx) }()
	t.Cleanup(cancel)
	// listen 까지 잠시 대기.
	time.Sleep(150 * time.Millisecond)
	return srv, cancel
}

// E2E — Logon + NewOrderSingle 1건 → fixApp 카운터 증가 검증.
func TestFixServer_LogonAndNewOrderSingle(t *testing.T) {
	port := pickFreePort(t)
	cfg := DefaultConfig()
	cfg.ListenPort = port
	cfg.Counterparties = map[string]Counterparty{
		"CLIENT_A": {
			Password: "secret-pw",
			Channel:  "FIX",
			Site:     "HQ",
			Tier:     "VIP",
			Usid:     "ECN_TEST_01",
		},
	}
	srv, _ := startServer(t, cfg)

	iniApp, ini := startInitiator(t, port, "CLIENT_A", "WTG")

	// Logon 통과 대기.
	select {
	case <-iniApp.logonDone:
	case <-time.After(3 * time.Second):
		t.Fatalf("Logon timeout — Stats=%+v", srv.Stats())
	}
	if iniApp.logonCount.Load() != 1 {
		t.Errorf("initiator logon=%d, want 1", iniApp.logonCount.Load())
	}

	// NewOrderSingle 송신.
	nos := newordersingle.New(
		field.NewClOrdID("ORD-TEST-001"),
		field.NewSide(enum.Side_BUY),
		field.NewTransactTime(time.Now()),
		field.NewOrdType(enum.OrdType_LIMIT),
	)
	nos.SetSymbol("USD/KRW")
	nos.SetOrderQty(decimal.NewFromInt(1000000), 0)
	nos.SetPrice(decimal.NewFromFloat(1378.55), 5)
	nos.SetTimeInForce(enum.TimeInForce_DAY)
	nos.SetQuoteID("Q-TEST-XXX")

	sid := quickfix.SessionID{
		BeginString:  "FIX.4.4",
		SenderCompID: "CLIENT_A",
		TargetCompID: "WTG",
	}
	if err := quickfix.SendToTarget(nos, sid); err != nil {
		t.Fatalf("SendToTarget: %v", err)
	}

	// fixApp 카운터 증가 대기.
	if !waitFor(2*time.Second, func() bool { return srv.Stats().OrdersReceived >= 1 }) {
		t.Fatalf("OrdersReceived timeout — Stats=%+v", srv.Stats())
	}
	st := srv.Stats()
	if st.LogonOK != 1 {
		t.Errorf("LogonOK=%d, want 1", st.LogonOK)
	}
	if st.OrdersReceived != 1 {
		t.Errorf("OrdersReceived=%d, want 1", st.OrdersReceived)
	}
	if st.LogonReject != 0 {
		t.Errorf("LogonReject=%d, want 0", st.LogonReject)
	}
	if st.ActiveSessions != 1 {
		t.Errorf("ActiveSessions=%d, want 1", st.ActiveSessions)
	}
	_ = ini
}

// LogonReject — counterparty 등록은 됐지만 password 불일치.
//
// quickfix settings 단계에서 미등록 SenderCompID 는 connect 자체 거부라
// reject 카운터 도달 X. 따라서 본 시나리오는 등록 + password mismatch.
func TestFixServer_LogonReject_WrongPassword(t *testing.T) {
	port := pickFreePort(t)
	cfg := DefaultConfig()
	cfg.ListenPort = port
	cfg.Counterparties = map[string]Counterparty{
		"CLIENT_A": {Password: "CORRECT_PW", Channel: "FIX", Site: "HQ", Tier: "VIP", Usid: "ok"},
	}
	srv, _ := startServer(t, cfg)

	// initiator 의 ToAdmin 이 "secret-pw" 를 첨부 — counterparty 의 CORRECT_PW
	// 와 불일치 → FromAdmin 에서 LogonReject 발동.
	_, _ = startInitiator(t, port, "CLIENT_A", "WTG")
	if !waitFor(3*time.Second, func() bool { return srv.Stats().LogonReject >= 1 }) {
		t.Fatalf("LogonReject timeout — Stats=%+v", srv.Stats())
	}
	st := srv.Stats()
	if st.LogonOK != 0 {
		t.Errorf("LogonOK=%d, want 0", st.LogonOK)
	}
	if st.ActiveSessions != 0 {
		t.Errorf("ActiveSessions=%d, want 0", st.ActiveSessions)
	}
}

// http forward — TxForwardURL 활성 시 httptest server 가 envelope 받음.
func TestFixServer_TxForward(t *testing.T) {
	port := pickFreePort(t)

	received := make(chan map[string]any, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.NewDecoder(bytes.NewReader(body)).Decode(&m)
		// 헤더 검증.
		m["_hdr_user"] = r.Header.Get("X-WTG-Edge-User")
		m["_hdr_channel"] = r.Header.Get("X-WTG-Edge-Channel")
		m["_hdr_site"] = r.Header.Get("X-WTG-Edge-Site")
		m["_hdr_tier"] = r.Header.Get("X-WTG-Edge-Tier")
		received <- m
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer ts.Close()

	cfg := DefaultConfig()
	cfg.ListenPort = port
	cfg.TxForwardURL = ts.URL
	cfg.Counterparties = map[string]Counterparty{
		"CLIENT_A": {
			Password: "secret-pw",
			Channel:  "FIX",
			Site:     "BRANCH",
			Tier:     "GOLD",
			Usid:     "ECN_BANK_01",
		},
	}
	srv, _ := startServer(t, cfg)
	iniApp, _ := startInitiator(t, port, "CLIENT_A", "WTG")
	select {
	case <-iniApp.logonDone:
	case <-time.After(3 * time.Second):
		t.Fatalf("Logon timeout")
	}

	nos := newordersingle.New(
		field.NewClOrdID("ORD-FWD-001"),
		field.NewSide(enum.Side_SELL),
		field.NewTransactTime(time.Now()),
		field.NewOrdType(enum.OrdType_LIMIT),
	)
	nos.SetSymbol("EUR/USD")
	nos.SetOrderQty(decimal.NewFromInt(500000), 0)
	nos.SetPrice(decimal.NewFromFloat(1.0820), 4)
	if err := quickfix.SendToTarget(nos, quickfix.SessionID{
		BeginString: "FIX.4.4", SenderCompID: "CLIENT_A", TargetCompID: "WTG",
	}); err != nil {
		t.Fatalf("SendToTarget: %v", err)
	}

	// httptest 가 envelope 받았는지.
	select {
	case got := <-received:
		if got["alias"] != "FIX_NEW_ORDER" {
			t.Errorf("alias=%v, want FIX_NEW_ORDER", got["alias"])
		}
		data, _ := got["data"].(map[string]any)
		if data["symbol"] != "EUR/USD" {
			t.Errorf("symbol=%v, want EUR/USD", data["symbol"])
		}
		if data["side"] != "sell" {
			t.Errorf("side=%v, want sell", data["side"])
		}
		if data["client_order_id"] != "ORD-FWD-001" {
			t.Errorf("client_order_id=%v, want ORD-FWD-001", data["client_order_id"])
		}
		if got["_hdr_user"] != "ECN_BANK_01" {
			t.Errorf("X-WTG-Edge-User=%v, want ECN_BANK_01", got["_hdr_user"])
		}
		if got["_hdr_channel"] != "FIX" || got["_hdr_site"] != "BRANCH" || got["_hdr_tier"] != "GOLD" {
			t.Errorf("Profile 헤더 누락: %v / %v / %v",
				got["_hdr_channel"], got["_hdr_site"], got["_hdr_tier"])
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("httptest receive timeout — Stats=%+v", srv.Stats())
	}

	if !waitFor(time.Second, func() bool { return srv.Stats().OrdersForwarded >= 1 }) {
		t.Errorf("OrdersForwarded 카운터 증가 안 함: %+v", srv.Stats())
	}
}

func waitFor(d time.Duration, fn func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if fn() {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

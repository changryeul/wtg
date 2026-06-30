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
	"github.com/quickfixgo/fix44/ordercancelreplacerequest"
	"github.com/quickfixgo/fix44/ordercancelrequest"
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
	logonCount  atomic.Int32
	mu          sync.Mutex
	logonDone   chan struct{}
	execReports []*quickfix.Message // 수신한 35=8
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
	mt, _ := msg.MsgType()
	if mt == "8" {
		// ExecutionReport 수신 — copy 보관.
		dup := quickfix.NewMessage()
		msg.CopyInto(dup)
		i.mu.Lock()
		i.execReports = append(i.execReports, dup)
		i.mu.Unlock()
	}
	return nil
}

func (i *testInitiator) recvCount() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return len(i.execReports)
}

func (i *testInitiator) recvLast() *quickfix.Message {
	i.mu.Lock()
	defer i.mu.Unlock()
	if len(i.execReports) == 0 {
		return nil
	}
	return i.execReports[len(i.execReports)-1]
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

// Layer 2 + 3 — counterparty alias + raw_fix 보존 한 케이스로.
func TestFixServer_AliasAndRawFix(t *testing.T) {
	port := pickFreePort(t)

	received := make(chan map[string]any, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.NewDecoder(bytes.NewReader(body)).Decode(&m)
		received <- m
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	cfg := DefaultConfig()
	cfg.ListenPort = port
	cfg.TxForwardURL = ts.URL
	cfg.Counterparties = map[string]Counterparty{
		"ECN_BANK": {
			Password:   "secret-pw",
			Channel:    "FIX",
			Site:       "HQ",
			Tier:       "VIP",
			Usid:       "ECN_BANK_01",
			OrderAlias: "ECN_BANK_ORDER",
		},
	}
	srv, _ := startServer(t, cfg)
	iniApp, _ := startInitiator(t, port, "ECN_BANK", "WTG")
	select {
	case <-iniApp.logonDone:
	case <-time.After(3 * time.Second):
		t.Fatalf("Logon timeout — Stats=%+v", srv.Stats())
	}

	// user-defined tag 5001 (예: NH_INTERNAL_ORDER_ID) 포함한 NewOrderSingle.
	nos := newordersingle.New(
		field.NewClOrdID("ORD-RAW-001"),
		field.NewSide(enum.Side_BUY),
		field.NewTransactTime(time.Now()),
		field.NewOrdType(enum.OrdType_LIMIT),
	)
	nos.SetSymbol("USD/KRW")
	nos.SetOrderQty(decimal.NewFromInt(1000000), 0)
	nos.SetPrice(decimal.NewFromFloat(1378.55), 5)
	// user-defined tag — Body 에 직접 set.
	nos.Body.SetString(5001, "NH_ORD_999")
	nos.Body.SetString(100, "DEUTSCHE_BOOK_A") // ExDestination (FIX 표준 tag)
	if err := quickfix.SendToTarget(nos, quickfix.SessionID{
		BeginString: "FIX.4.4", SenderCompID: "ECN_BANK", TargetCompID: "WTG",
	}); err != nil {
		t.Fatalf("SendToTarget: %v", err)
	}

	select {
	case got := <-received:
		// Layer 2 — alias 가 counterparty 의 OrderAlias.
		if got["alias"] != "ECN_BANK_ORDER" {
			t.Errorf("alias=%v, want ECN_BANK_ORDER (counterparty override 안 됨)", got["alias"])
		}
		data, _ := got["data"].(map[string]any)
		// Layer 1 — typed 필드 (기존).
		if data["symbol"] != "USD/KRW" {
			t.Errorf("symbol=%v", data["symbol"])
		}
		// Layer 3 — raw_fix 에 모든 tag 보존.
		rawFix, ok := data["raw_fix"].(map[string]any)
		if !ok {
			t.Fatalf("raw_fix 누락: %+v", data)
		}
		// 표준 tag 도 포함되어야 (typed 와 중복 — generic 원칙).
		if rawFix["11"] != "ORD-RAW-001" {
			t.Errorf("raw_fix[11]=%v, want ORD-RAW-001", rawFix["11"])
		}
		// user-defined tag.
		if rawFix["5001"] != "NH_ORD_999" {
			t.Errorf("raw_fix[5001]=%v, want NH_ORD_999 (user-defined tag 누락)", rawFix["5001"])
		}
		// 표준이지만 typed 안 한 tag.
		if rawFix["100"] != "DEUTSCHE_BOOK_A" {
			t.Errorf("raw_fix[100]=%v, want DEUTSCHE_BOOK_A (ExDestination 누락)", rawFix["100"])
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("httptest receive timeout — Stats=%+v", srv.Stats())
	}
}

// Phase C — Cancel(35=F) + Replace(35=G) 가 envelope.Op 분기로 처리.
func TestFixServer_CancelAndReplace(t *testing.T) {
	port := pickFreePort(t)

	received := make(chan map[string]any, 4)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.NewDecoder(bytes.NewReader(body)).Decode(&m)
		received <- m
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	cfg := DefaultConfig()
	cfg.ListenPort = port
	cfg.TxForwardURL = ts.URL
	cfg.Counterparties = map[string]Counterparty{
		"ECN_C": {
			Password: "secret-pw", Channel: "FIX", Site: "HQ", Tier: "VIP",
			Usid: "ECN_C", OrderAlias: "ECN_C_LIFECYCLE",
		},
	}
	srv, _ := startServer(t, cfg)
	iniApp, _ := startInitiator(t, port, "ECN_C", "WTG")
	select {
	case <-iniApp.logonDone:
	case <-time.After(3 * time.Second):
		t.Fatalf("Logon timeout — Stats=%+v", srv.Stats())
	}

	sid := quickfix.SessionID{BeginString: "FIX.4.4", SenderCompID: "ECN_C", TargetCompID: "WTG"}

	// 1) OrderCancelRequest (35=F).
	cr := ordercancelrequest.New(
		field.NewOrigClOrdID("ORD-ORIG-001"),
		field.NewClOrdID("CANCEL-001"),
		field.NewSide(enum.Side_SELL),
		field.NewTransactTime(time.Now()),
	)
	cr.SetSymbol("USD/KRW")
	cr.SetOrderID("ENG-001")
	if err := quickfix.SendToTarget(cr, sid); err != nil {
		t.Fatalf("send Cancel: %v", err)
	}
	verifyCRMessage(t, received, "cancel", "CANCEL-001", "ORD-ORIG-001", "ECN_C_LIFECYCLE")

	// 2) OrderCancelReplaceRequest (35=G) — qty 변경.
	rr := ordercancelreplacerequest.New(
		field.NewOrigClOrdID("ORD-ORIG-002"),
		field.NewClOrdID("REPLACE-002"),
		field.NewSide(enum.Side_BUY),
		field.NewTransactTime(time.Now()),
		field.NewOrdType(enum.OrdType_LIMIT),
	)
	rr.SetSymbol("EUR/USD")
	rr.SetOrderQty(decimal.NewFromInt(2000000), 0) // 변경된 수량
	rr.SetPrice(decimal.NewFromFloat(1.0900), 4)
	rr.SetOrderID("ENG-002")
	if err := quickfix.SendToTarget(rr, sid); err != nil {
		t.Fatalf("send Replace: %v", err)
	}
	got := verifyCRMessage(t, received, "replace", "REPLACE-002", "ORD-ORIG-002", "ECN_C_LIFECYCLE")
	data, _ := got["data"].(map[string]any)
	if qty, _ := data["qty"].(float64); qty != 2000000 {
		t.Errorf("replace qty=%v, want 2000000", qty)
	}
	if px, _ := data["price"].(float64); px != 1.09 {
		t.Errorf("replace price=%v, want 1.09", px)
	}
}

func verifyCRMessage(t *testing.T, received chan map[string]any, wantOp, wantClOrdID, wantOrigClOrdID, wantAlias string) map[string]any {
	t.Helper()
	select {
	case got := <-received:
		if got["alias"] != wantAlias {
			t.Errorf("alias=%v, want %s", got["alias"], wantAlias)
		}
		data, _ := got["data"].(map[string]any)
		if data["op"] != wantOp {
			t.Errorf("op=%v, want %s", data["op"], wantOp)
		}
		if data["client_order_id"] != wantClOrdID {
			t.Errorf("client_order_id=%v, want %s", data["client_order_id"], wantClOrdID)
		}
		if data["orig_client_order_id"] != wantOrigClOrdID {
			t.Errorf("orig_client_order_id=%v, want %s", data["orig_client_order_id"], wantOrigClOrdID)
		}
		return got
	case <-time.After(3 * time.Second):
		t.Fatalf("receive timeout for op=%s", wantOp)
		return nil
	}
}

// Phase C — Reload (SIGHUP 시뮬레이션). policy 의 새 CID 가 reload 후 active.
func TestFixServer_Reload(t *testing.T) {
	port := pickFreePort(t)
	cfg := DefaultConfig()
	cfg.ListenPort = port
	cfg.Counterparties = map[string]Counterparty{
		"CP_OLD": {Password: "secret-pw", Channel: "FIX", Site: "HQ", Tier: "VIP", Usid: "OLD"},
	}
	srv, _ := startServer(t, cfg)

	// 등록 안 된 CID 의 initiator — settings 단계에서 차단 (LogonReject 도달 X).
	// reload 전엔 connect 불가.

	// policy 에 새 CP 추가 (etcd 시뮬 — staticPolicy 라 직접 추가 불가, memory 로 전환 필요).
	// 본 테스트는 staticPolicy 의 한계 — Reload 가 cfg.Counterparties snapshot 으로
	// settings 재빌드 OK 만 검증.
	if err := srv.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	// reload 후에도 기존 CP_OLD 의 logon 정상.
	iniApp, _ := startInitiator(t, port, "CP_OLD", "WTG")
	select {
	case <-iniApp.logonDone:
	case <-time.After(3 * time.Second):
		t.Fatalf("reload 후 logon 실패 — Stats=%+v", srv.Stats())
	}
}

// Phase B-2 — POST /v1/internal/exec-report → quickfix initiator 가 35=8 수신.
func TestFixServer_ExecReportDropCopy(t *testing.T) {
	port := pickFreePort(t)
	cfg := DefaultConfig()
	cfg.ListenPort = port
	cfg.Counterparties = map[string]Counterparty{
		"ECN_A": {Password: "secret-pw", Channel: "FIX", Site: "HQ", Tier: "VIP", Usid: "ECN_A_USID"},
	}
	srv, _ := startServer(t, cfg)

	// initiator + Logon 통과 대기.
	iniApp, _ := startInitiator(t, port, "ECN_A", "WTG")
	select {
	case <-iniApp.logonDone:
	case <-time.After(3 * time.Second):
		t.Fatalf("Logon timeout")
	}

	// HTTP receive endpoint mount.
	httpMux := http.NewServeMux()
	httpMux.HandleFunc("POST /v1/internal/exec-report", ExecReportHandler(ExecReportHandlerDeps{
		Server: srv,
		Secret: "test-secret",
		Logger: quietLogger(),
	}))
	hts := httptest.NewServer(httpMux)
	defer hts.Close()

	// 1) 인증 실패 → 401.
	r, _ := http.Post(hts.URL+"/v1/internal/exec-report", "application/json",
		strings.NewReader(`{"target_sender_comp_id":"ECN_A","order_id":"X","exec_id":"Y","exec_type":"0","ord_status":"0","side":"buy"}`))
	if r.StatusCode != 401 {
		t.Errorf("no secret status=%d, want 401", r.StatusCode)
	}
	r.Body.Close()

	// 2) New 단계 ExecutionReport — secret 정확.
	body := `{
		"target_sender_comp_id":"ECN_A",
		"order_id":"ENG-001",
		"client_order_id":"ORD-001",
		"exec_id":"EXEC-001",
		"exec_type":"0",
		"ord_status":"0",
		"side":"buy",
		"symbol":"USD/KRW",
		"leaves_qty":1000000,
		"cum_qty":0,
		"avg_px":0
	}`
	req, _ := http.NewRequest(http.MethodPost, hts.URL+"/v1/internal/exec-report", strings.NewReader(body))
	req.Header.Set("X-Push-Secret", "test-secret")
	req.Header.Set("Content-Type", "application/json")
	r, _ = http.DefaultClient.Do(req)
	if r.StatusCode != 200 {
		t.Fatalf("post status=%d", r.StatusCode)
	}
	r.Body.Close()

	// 3) initiator 가 35=8 수신.
	if !waitFor(2*time.Second, func() bool { return iniApp.recvCount() >= 1 }) {
		t.Fatalf("ExecutionReport 미수신 — Stats=%+v", srv.Stats())
	}
	msg := iniApp.recvLast()
	mt, _ := msg.MsgType()
	if mt != "8" {
		t.Errorf("MsgType=%q, want 8", mt)
	}
	orderID, _ := msg.Body.GetString(37)
	if orderID != "ENG-001" {
		t.Errorf("OrderID=%q, want ENG-001", orderID)
	}
	clOrdID, _ := msg.Body.GetString(11)
	if clOrdID != "ORD-001" {
		t.Errorf("ClOrdID=%q, want ORD-001", clOrdID)
	}

	// 4) Rejection — OrdRejReason 매핑 검증.
	body = `{
		"target_sender_comp_id":"ECN_A",
		"order_id":"ENG-002",
		"exec_id":"EXEC-002",
		"exec_type":"8",
		"ord_status":"8",
		"side":"buy",
		"symbol":"USD/KRW",
		"ord_rej_reason":"1029"
	}`
	req, _ = http.NewRequest(http.MethodPost, hts.URL+"/v1/internal/exec-report", strings.NewReader(body))
	req.Header.Set("X-Push-Secret", "test-secret")
	req.Header.Set("Content-Type", "application/json")
	r, _ = http.DefaultClient.Do(req)
	if r.StatusCode != 200 {
		t.Fatalf("rej post status=%d", r.StatusCode)
	}
	r.Body.Close()

	if !waitFor(2*time.Second, func() bool { return iniApp.recvCount() >= 2 }) {
		t.Fatalf("Rejection ExecutionReport 미수신 — Stats=%+v", srv.Stats())
	}
	msg = iniApp.recvLast()
	rejReason, _ := msg.Body.GetString(103)
	if rejReason != "99" {
		t.Errorf("OrdRejReason=%q, want 99 (errn 1029 → Other)", rejReason)
	}
	text, _ := msg.Body.GetString(58)
	if text != "quote id expired" {
		t.Errorf("Text=%q, want 'quote id expired'", text)
	}

	// 5) target session 미활성 → 503.
	body = `{"target_sender_comp_id":"UNKNOWN","order_id":"E","exec_id":"X","exec_type":"0","ord_status":"0","side":"buy"}`
	req, _ = http.NewRequest(http.MethodPost, hts.URL+"/v1/internal/exec-report", strings.NewReader(body))
	req.Header.Set("X-Push-Secret", "test-secret")
	r, _ = http.DefaultClient.Do(req)
	if r.StatusCode != 503 {
		t.Errorf("unknown target status=%d, want 503", r.StatusCode)
	}
	r.Body.Close()

	// Stats 검증.
	st := srv.Stats()
	if st.ExecReportSent != 2 {
		t.Errorf("ExecReportSent=%d, want 2", st.ExecReportSent)
	}
	if st.ExecReportRejected != 1 {
		t.Errorf("ExecReportRejected=%d, want 1", st.ExecReportRejected)
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

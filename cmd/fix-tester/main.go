// fix-tester — mci-edge-fix smoke test 도구.
//
// 단일 quickfix initiator 를 띄워 Logon + 옵션 NewOrderSingle 송신. 결과
// 출력 후 종료. 운영자가 admin UI 의 "fix-tester CLI 명령 복사" 버튼에서
// 받은 명령을 그대로 터미널에 붙여넣어 실행.
//
// 사용:
//
//	./build/bin/fix-tester \
//	    --target 127.0.0.1:5001 \
//	    --sender ECN_TEST_01 \
//	    --target-comp WTG \
//	    --password test-pw \
//	    --send-order USD/KRW:buy:1000000:1378.55
//
//	./build/bin/fix-tester --target 127.0.0.1:5001 --sender ECN_TEST_01 --password test-pw
//	  (Logon 만 — order 안 보내고 5초 후 종료)
package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/quickfixgo/enum"
	"github.com/quickfixgo/field"
	"github.com/quickfixgo/fix44/newordersingle"
	"github.com/quickfixgo/quickfix"
	"github.com/shopspring/decimal"
)

type app struct {
	logonDone   chan struct{}
	logonCount  atomic.Int32
	logoutCount atomic.Int32
	password    string
	recvCount   atomic.Int32
}

func newApp(password string) *app {
	return &app{logonDone: make(chan struct{}, 1), password: password}
}

func (a *app) OnCreate(sid quickfix.SessionID) {
	fmt.Printf("[fix-tester] session created sender=%s target=%s\n",
		sid.SenderCompID, sid.TargetCompID)
}
func (a *app) OnLogon(sid quickfix.SessionID) {
	a.logonCount.Add(1)
	fmt.Printf("[fix-tester] LOGON OK\n")
	select {
	case a.logonDone <- struct{}{}:
	default:
	}
}
func (a *app) OnLogout(sid quickfix.SessionID) {
	a.logoutCount.Add(1)
	fmt.Printf("[fix-tester] LOGOUT\n")
}
func (a *app) ToAdmin(msg *quickfix.Message, sid quickfix.SessionID) {
	if t, err := msg.MsgType(); err == nil && t == "A" && a.password != "" {
		msg.Body.SetString(554, a.password)
	}
}
func (a *app) FromAdmin(msg *quickfix.Message, sid quickfix.SessionID) quickfix.MessageRejectError {
	return nil
}
func (a *app) ToApp(msg *quickfix.Message, sid quickfix.SessionID) error { return nil }
func (a *app) FromApp(msg *quickfix.Message, sid quickfix.SessionID) quickfix.MessageRejectError {
	a.recvCount.Add(1)
	mt, _ := msg.MsgType()
	if mt == "8" {
		orderID, _ := msg.Body.GetString(37)
		execType, _ := msg.Body.GetString(150)
		ordStatus, _ := msg.Body.GetString(39)
		fmt.Printf("[fix-tester] EXEC_REPORT order_id=%s exec_type=%s ord_status=%s\n",
			orderID, execType, ordStatus)
	} else {
		fmt.Printf("[fix-tester] FromApp msg_type=%s\n", mt)
	}
	return nil
}

func main() {
	var (
		target      = flag.String("target", "127.0.0.1:5001", "mci-edge-fix host:port")
		sender      = flag.String("sender", "ECN_TEST_01", "SenderCompID (이 client)")
		targetComp  = flag.String("target-comp", "WTG", "TargetCompID (mci-edge-fix self)")
		password    = flag.String("password", "", "Logon Password (tag 554). 빈값=skip")
		sendOrder   = flag.String("send-order", "", "선택 — Logon 후 NewOrderSingle 1개. 형식: 'SYMBOL:SIDE:QTY[:PRICE]' (예: USD/KRW:buy:1000000:1378.55). PRICE 빈값=Market")
		waitTime    = flag.Duration("wait", 5*time.Second, "Logon 후 ExecutionReport 등 수신 대기 시간")
		dialTimeout = flag.Duration("dial-timeout", 5*time.Second, "Logon 대기 timeout")
	)
	flag.Parse()

	host, port, err := splitHostPort(*target)
	if err != nil {
		fmt.Fprintln(os.Stderr, "target 파싱 실패:", err)
		os.Exit(2)
	}

	settings := fmt.Sprintf(`[DEFAULT]
ConnectionType=initiator
ReconnectInterval=1
SenderCompID=%s
TargetCompID=%s
SocketConnectHost=%s
SocketConnectPort=%d
BeginString=FIX.4.4
HeartBtInt=30
StartTime=00:00:00
EndTime=00:00:00
ResetOnLogon=Y
[SESSION]
TargetCompID=%s
`, *sender, *targetComp, host, port, *targetComp)

	cfg, err := quickfix.ParseSettings(strings.NewReader(settings))
	if err != nil {
		fmt.Fprintln(os.Stderr, "settings 파싱:", err)
		os.Exit(1)
	}
	a := newApp(*password)
	ini, err := quickfix.NewInitiator(a, quickfix.NewMemoryStoreFactory(), cfg, quickfix.NewNullLogFactory())
	if err != nil {
		fmt.Fprintln(os.Stderr, "initiator:", err)
		os.Exit(1)
	}
	if err := ini.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "start:", err)
		os.Exit(1)
	}
	defer ini.Stop()

	// Logon 통과 대기.
	select {
	case <-a.logonDone:
	case <-time.After(*dialTimeout):
		fmt.Fprintln(os.Stderr, "[fix-tester] LOGON timeout (password 또는 counterparty 등록 확인)")
		os.Exit(3)
	}

	// 선택 — NewOrderSingle 1건 송신.
	if *sendOrder != "" {
		if err := sendNewOrderSingle(*sender, *targetComp, *sendOrder); err != nil {
			fmt.Fprintln(os.Stderr, "[fix-tester] order 송신 실패:", err)
			os.Exit(4)
		}
		fmt.Println("[fix-tester] NewOrderSingle 송신 완료")
	}

	// ExecutionReport 등 수신 대기.
	fmt.Printf("[fix-tester] %s 동안 수신 대기...\n", *waitTime)
	time.Sleep(*waitTime)

	fmt.Printf("[fix-tester] 종료 — recv_count=%d logout_count=%d\n",
		a.recvCount.Load(), a.logoutCount.Load())
}

func sendNewOrderSingle(sender, target, spec string) error {
	parts := strings.Split(spec, ":")
	if len(parts) < 3 {
		return fmt.Errorf("형식 오류 — 'SYMBOL:SIDE:QTY[:PRICE]' (got %q)", spec)
	}
	symbol := parts[0]
	sideStr := strings.ToLower(parts[1])
	qty, err := strconv.ParseFloat(parts[2], 64)
	if err != nil {
		return fmt.Errorf("qty 파싱: %w", err)
	}
	var fixSide enum.Side
	switch sideStr {
	case "buy", "b":
		fixSide = enum.Side_BUY
	case "sell", "s":
		fixSide = enum.Side_SELL
	default:
		return fmt.Errorf("side 미지원: %q", sideStr)
	}
	var price float64
	ordType := enum.OrdType_MARKET
	if len(parts) >= 4 && parts[3] != "" {
		price, err = strconv.ParseFloat(parts[3], 64)
		if err != nil {
			return fmt.Errorf("price 파싱: %w", err)
		}
		ordType = enum.OrdType_LIMIT
	}

	clOrdID := fmt.Sprintf("TESTER-%d", time.Now().Unix())
	nos := newordersingle.New(
		field.NewClOrdID(clOrdID),
		field.NewSide(fixSide),
		field.NewTransactTime(time.Now()),
		field.NewOrdType(ordType),
	)
	nos.SetSymbol(symbol)
	nos.SetOrderQty(decimal.NewFromFloat(qty), 0)
	if ordType == enum.OrdType_LIMIT {
		nos.SetPrice(decimal.NewFromFloat(price), 5)
	}
	sid := quickfix.SessionID{BeginString: "FIX.4.4", SenderCompID: sender, TargetCompID: target}
	return quickfix.SendToTarget(nos, sid)
}

func splitHostPort(hostport string) (string, int, error) {
	i := strings.LastIndex(hostport, ":")
	if i < 0 {
		return "", 0, fmt.Errorf("':' 없음")
	}
	host := hostport[:i]
	port, err := strconv.Atoi(hostport[i+1:])
	if err != nil {
		return "", 0, err
	}
	return host, port, nil
}
